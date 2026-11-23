// Package ship is tally's shipper: it runs on a machine where agents work,
// reads their transcripts and T3 Code's thread tree, and posts every
// conversation's usage to tally by model and minute. It never sends a
// prompt, a reply, or a file's content; only titles, ids, and counts.
package ship

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
)

// Config is the shipper's settings.
type Config struct {
	// URL is tally's base address, such as https://tally.example.com.
	URL   string
	Token string
	// Claude is Claude Code's projects folder.
	Claude string
	// Plugins is Claude Code's record of the plugins it has installed.
	Plugins string
	// T3 is T3 Code's state database.
	T3 string
	// Ion is Ion's home: its conversations and session bindings.
	Ion string
	// State is where the shipper keeps its place.
	State string
	Every time.Duration
	// Route is the AI gateway's host whose work alone this shipper posts,
	// such as ai.example.com; empty posts every reply. See Route.
	Route string
	// ClaudeSettings are Claude Code's settings files, read each round for
	// where it sends its requests (ClaudeSettings in route.go).
	ClaudeSettings []string
	// ClaudeBaseURL is ANTHROPIC_BASE_URL in the shipper's environment, for
	// a machine that sets it outside Claude Code's settings.
	ClaudeBaseURL string
}

// FromEnv reads TALLY_URL, TALLY_TOKEN or TALLY_TOKEN_FILE, CLAUDE_PROJECTS,
// T3_STATE, ION_DIR, SHIP_STATE, SHIP_EVERY, SHIP_ROUTE, and
// ANTHROPIC_BASE_URL.
func FromEnv() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	conf, err := os.UserConfigDir()
	if err != nil {
		return Config{}, err
	}
	c := Config{
		URL:    strings.TrimRight(os.Getenv("TALLY_URL"), "/"),
		Token:  os.Getenv("TALLY_TOKEN"),
		Claude: env("CLAUDE_PROJECTS", filepath.Join(home, ".claude", "projects")),
		T3:     env("T3_STATE", filepath.Join(home, ".t3", "userdata", "statev2.sqlite")),
		Ion:    env("ION_DIR", filepath.Join(home, ".ion")),
		State:  env("SHIP_STATE", filepath.Join(conf, "tally", "ship.json")),
		Every:  time.Minute,
		Route:  strings.ToLower(strings.TrimSpace(os.Getenv("SHIP_ROUTE"))),
	}
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	c.ClaudeSettings = ClaudeSettings(filepath.Dir(c.Claude), name)
	c.Plugins = filepath.Join(filepath.Dir(c.Claude), "plugins", "installed_plugins.json")
	c.ClaudeBaseURL = os.Getenv("ANTHROPIC_BASE_URL")
	if d, err := time.ParseDuration(os.Getenv("SHIP_EVERY")); err == nil && d > 0 {
		c.Every = d
	}
	if c.Token == "" {
		if f := os.Getenv("TALLY_TOKEN_FILE"); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				return Config{}, fmt.Errorf("TALLY_TOKEN_FILE: %w", err)
			}
			c.Token = strings.TrimSpace(string(b))
		}
	}
	if c.URL == "" || c.Token == "" {
		return Config{}, errors.New("set TALLY_URL and TALLY_TOKEN or TALLY_TOKEN_FILE")
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Quiet is how long a conversation goes without a reply before the shipper
// forgets its counted replies and totals. A conversation that wakes after
// that starts new minutes, so nothing it posts again is short.
const Quiet = 48 * time.Hour

// Shipper is one machine's shipper.
type Shipper struct {
	cfg    Config
	log    *slog.Logger
	client *http.Client

	files map[string]fileState
	known map[string]Known
	sent  map[string]string
	convs map[string]*Conv
	// owner is every claimed reply's conversation, across active ones.
	owner map[uint64]string
}

// fileState is how far a transcript was read: Claude Code's by offset, as
// it only appends; Ion's by size and time, as it rewrites a file whole.
type fileState struct {
	Offset  int64 `json:"offset,omitempty"`
	Size    int64 `json:"size,omitempty"`
	ModTime int64 `json:"mod_time,omitempty"`
}

// New loads the shipper's place, or starts from the beginning.
func New(cfg Config, log *slog.Logger) (*Shipper, error) {
	s := &Shipper{cfg: cfg, log: log, client: &http.Client{Timeout: time.Minute},
		files: map[string]fileState{}, known: map[string]Known{}, sent: map[string]string{}, convs: map[string]*Conv{}, owner: map[uint64]string{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Run ships a round now and every cfg.Every until ctx ends.
func (s *Shipper) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Every)
	defer t.Stop()
	for {
		if err := s.Round(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("ship round failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Round reads every transcript that grew, posts what changed, and saves
// the shipper's place once tally has taken all of it.
func (s *Shipper) Round(ctx context.Context) error {
	started := time.Now()
	read, err := s.read()
	if err != nil {
		return err
	}
	readIon, err := s.readIon()
	if err != nil {
		return err
	}
	read += readIon
	titles, err := ReadIonTitles(filepath.Join(s.cfg.Ion, "tabs.json"))
	if err != nil {
		s.log.Warn("ion titles not read", "error", err)
	}
	s.titleIon(titles)
	var works []agents.Work
	var convs []agents.Conversation
	t3, err := ReadT3(ctx, s.cfg.T3)
	if err != nil {
		// Without the thread tree every T3 session would look like a work
		// of its own; send only usage until it reads again.
		s.log.Warn("t3 not read; sending usage only", "error", err)
	} else {
		bindings, err := ReadBindings(filepath.Join(s.cfg.Ion, "session-bindings.json"))
		if err != nil {
			s.log.Warn("ion bindings not read", "error", err)
		}
		works, convs = Resolve(s.known, t3, bindings)
	}
	route, err := Through(s.cfg.Route, filepath.Join(s.cfg.Ion, "engine.json"), ClaudeBaseURL(s.cfg.ClaudeSettings, s.cfg.ClaudeBaseURL))
	if err != nil {
		// Without the providers no reply can be placed on the route, and
		// posting it anyway could post work that must stay here.
		return fmt.Errorf("ion providers: %w", err)
	}
	batch, marks, held := s.changes(works, convs, route)
	// What the machine has installed is said whole, and again only when it
	// differs from what tally last took.
	if s.cfg.Plugins == "" {
		// No record to read: the machine says nothing of what it has.
	} else if installed, err := ReadInstalled(s.cfg.Plugins); err != nil {
		s.log.Warn("installed plugins not read", "error", err)
	} else if h := digest(installed); s.sent[installedMark] != h {
		batch.Installed = &installed
		marks[installedMark] = h
	}
	n := len(batch.Works) + len(batch.Conversations) + len(batch.Buckets) + len(batch.Uses)
	if batch.Installed != nil {
		n++
	}
	if n > 0 {
		if err := s.post(ctx, batch); err != nil {
			return err
		}
	}
	for k, v := range marks {
		s.sent[k] = v
	}
	for _, c := range s.convs {
		clear(c.Dirty)
		for k := range c.DirtyUses {
			// A typed command waits for the conversation's first reply to
			// name its model.
			if k.Model != "" || c.Model != "" {
				delete(c.DirtyUses, k)
			}
		}
	}
	forgot := s.forget(time.Now())
	if err := s.save(); err != nil {
		return err
	}
	if read == 0 && n == 0 && forgot == 0 {
		return nil
	}
	s.log.Info("ship round done", "files_read", read, "works", len(batch.Works), "conversations", len(batch.Conversations), "buckets", len(batch.Buckets), "uses", len(batch.Uses), "held_off_route", held, "active", len(s.convs), "forgot", forgot, "took_ms", time.Since(started).Milliseconds())
	return nil
}

// read reads every transcript that changed since the last round: sessions
// first, then subagents from the shallowest, so a reply a fork repeats is
// already its parent's.
func (s *Shipper) read() (int, error) {
	paths, err := transcripts(s.cfg.Claude)
	if err != nil {
		return 0, err
	}
	type job struct {
		f     File
		meta  SubagentMeta
		depth int
	}
	var jobs []job
	for _, p := range paths {
		f, ok := ClaudeFile(p)
		if !ok {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st := s.files[p]; fi.Size() == st.Offset {
			continue
		}
		j := job{f: f}
		if f.Agent != "" {
			if b, err := os.ReadFile(strings.TrimSuffix(p, ".jsonl") + ".meta.json"); err == nil {
				_ = json.Unmarshal(b, &j.meta)
			}
			j.depth = max(j.meta.SpawnDepth, 1)
		}
		jobs = append(jobs, j)
	}
	sort.SliceStable(jobs, func(a, b int) bool {
		if jobs[a].depth != jobs[b].depth {
			return jobs[a].depth < jobs[b].depth
		}
		return jobs[a].f.Path < jobs[b].f.Path
	})
	for _, j := range jobs {
		if err := s.readFile(j.f, j.meta); err != nil {
			s.log.Warn("transcript not read", "path", j.f.Path, "error", err)
		}
	}
	return len(jobs), nil
}

func transcripts(root string) ([]string, error) {
	sessions, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	subagents, err := filepath.Glob(filepath.Join(root, "*", "*", "subagents", "agent-*.jsonl"))
	if err != nil {
		return nil, err
	}
	return append(sessions, subagents...), nil
}

func (s *Shipper) readFile(f File, meta SubagentMeta) error {
	fh, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return err
	}
	st := s.files[f.Path]
	if fi.Size() < st.Offset {
		// Rewritten from the start: read it again. Its replies are already
		// claimed while the conversation is active.
		st.Offset = 0
	}
	if _, err := fh.Seek(st.Offset, io.SeekStart); err != nil {
		return err
	}
	c := s.convs[f.Conversation]
	if c == nil {
		c = NewConv()
		s.convs[f.Conversation] = c
	}
	k, ok := s.known[f.Conversation]
	if !ok {
		k = Known{Session: f.Session, Agent: f.Agent}
	}
	if f.Agent != "" {
		k.Title, k.ParentAgent = meta.Description, meta.ParentAgentID
	}
	info := k.Info
	claim := func(key uint64) bool {
		if _, taken := s.owner[key]; taken {
			return false
		}
		s.owner[key] = f.Conversation
		return true
	}
	n, err := Read(fh, c, &info, claim)
	st.Offset += n
	s.files[f.Path] = st
	if f.Agent == "" {
		k.Info = info
	} else if k.CWD == "" {
		k.CWD = info.CWD
	}
	s.known[f.Conversation] = k
	return err
}

// readIon reads every Ion conversation file that changed since the last
// round, whole.
func (s *Shipper) readIon() (int, error) {
	dir := filepath.Join(s.cfg.Ion, "conversations")
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range paths {
		id, ok := IonFile(p)
		if !ok {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		st := fileState{Size: fi.Size(), ModTime: fi.ModTime().UnixNano()}
		if s.files[p] == st {
			continue
		}
		if err := s.readIonFile(p, id); err != nil {
			s.log.Warn("ion conversation not read", "path", p, "error", err)
			continue
		}
		s.files[p] = st
		n++
	}
	return n, nil
}

func (s *Shipper) readIonFile(path, id string) error {
	var base IonHeader
	if fh, err := os.Open(filepath.Join(filepath.Dir(path), id+".llm.jsonl")); err == nil {
		base, _ = ReadIonHeader(fh)
		fh.Close()
	}
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	conv := "ion:" + id
	c := s.convs[conv]
	if c == nil {
		c = NewConv()
		s.convs[conv] = c
	}
	h, dispatched, err := ReadIon(fh, c, base, func(key uint64) bool {
		if _, taken := s.owner[key]; taken {
			return false
		}
		s.owner[key] = conv
		return true
	})
	if err != nil {
		return err
	}
	k := s.known[conv]
	k.Source, k.Session = SourceIon, id
	if h.WorkingDirectory != "" {
		k.CWD = h.WorkingDirectory
	}
	links := &IonLinks{}
	if k.Ion != nil {
		*links = *k.Ion
	}
	if h.ForkOf == "" {
		links.Parent = h.ParentID
	}
	if len(dispatched) > 0 {
		links.Dispatched = dispatched
	}
	links.Native = nil
	for backend, ns := range h.NativeSessions {
		if backend == "claude-code" && ns.Cursor != "" {
			links.Native = append(links.Native, ns.Cursor)
		}
	}
	k.Ion = links
	s.known[conv] = k
	return nil
}

// ReadIonTitles reads the titles Ion's desktop gave its conversations, in
// open tabs and in settled history: a title the person set, or the one Ion
// wrote. A missing file is none.
func ReadIonTitles(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return map[string]string{}, err
	}
	type tab struct {
		ConversationID string `json:"conversationId"`
		Title          string `json:"title"`
		CustomTitle    string `json:"customTitle"`
	}
	var v struct {
		Tabs    []tab `json:"tabs"`
		Settled []tab `json:"settledHistory"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return map[string]string{}, err
	}
	out := map[string]string{}
	for _, t := range append(v.Settled, v.Tabs...) {
		title := t.CustomTitle
		if title == "" {
			title = t.Title
		}
		if t.ConversationID != "" && title != "" {
			out[t.ConversationID] = title
		}
	}
	return out, nil
}

// titleIon gives each titled Ion conversation its title, and every
// conversation it continues from that has none.
func (s *Shipper) titleIon(titles map[string]string) {
	for id, title := range titles {
		seen := map[string]bool{}
		for conv := "ion:" + id; !seen[conv]; {
			seen[conv] = true
			k, ok := s.known[conv]
			if !ok || (k.Title != "" && conv != "ion:"+id) {
				break
			}
			k.Title = title
			s.known[conv] = k
			if k.Ion == nil || k.Ion.Parent == "" {
				break
			}
			conv = "ion:" + k.Ion.Parent
		}
	}
}

// ReadBindings reads Ion's session bindings: a session key, such as an ACP
// session's, to the conversation it last ran. A missing file is none.
func ReadBindings(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return map[string]string{}, err
	}
	var v struct {
		Bindings map[string]string `json:"bindings"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return map[string]string{}, err
	}
	return v.Bindings, nil
}

// changes is everything to post: works and conversations whose record
// differs from what tally last took, and every bucket that changed. marks
// is what to remember as sent once the post succeeds. Off the route, a
// bucket stays here, and so do a conversation with no reply on the route
// and a work with no such conversation; held counts the buckets kept back.
func (s *Shipper) changes(works []agents.Work, convs []agents.Conversation, route Route) (b agents.Batch, marks map[string]string, held int) {
	marks = map[string]string{}
	for id, c := range s.convs {
		for k := range c.Dirty {
			if !route.Allows(k.Model) {
				held++
				continue
			}
			b.Buckets = append(b.Buckets, agents.Bucket{Conversation: id, Model: k.Model, Minute: time.Unix(k.Minute, 0).UTC(), Tokens: c.Buckets[k]})
			if kn, ok := s.known[id]; ok && !kn.Routed {
				kn.Routed = true
				s.known[id] = kn
			}
		}
		// A use goes where its replies go. Off the route, only a plugin's
		// use leaves, and bare: what it is and how often, under an id that
		// says nothing of the conversation. What a plugin is used for stays
		// here with the rest; that it is used is what a catalog counts.
		for k := range c.DirtyUses {
			model := k.Model
			if model == "" {
				model = c.Model
			}
			if model == "" {
				continue
			}
			u := agents.Use{Conversation: id, Model: model, Minute: time.Unix(k.Minute, 0).UTC(), Kind: k.Kind, Name: k.Name, Count: c.Uses[k]}
			if !route.Allows(model) {
				if u.Plugin() == "" {
					continue
				}
				u.Conversation = agents.Bare(id)
			}
			b.Uses = append(b.Uses, u)
		}
	}
	on := func(id string) bool { return route.Host == "" || s.known[id].Routed }
	worked := map[string]bool{}
	for _, c := range convs {
		if !on(c.ID) {
			continue
		}
		worked[c.Work] = true
		if h := digest(c); s.sent["c:"+c.ID] != h {
			b.Conversations = append(b.Conversations, c)
			marks["c:"+c.ID] = h
		}
	}
	for _, w := range works {
		if route.Host != "" && !worked[w.ID] {
			continue
		}
		if h := digest(w); s.sent["w:"+w.ID] != h {
			b.Works = append(b.Works, w)
			marks["w:"+w.ID] = h
		}
	}
	return b, marks, held
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// post sends b in parts tally takes: works and conversations first, so a
// bucket's conversation is known by the time it lands.
func (s *Shipper) post(ctx context.Context, b agents.Batch) error {
	var parts []agents.Batch
	cur := agents.Batch{}
	size := 0
	flush := func() {
		if size > 0 {
			parts = append(parts, cur)
			cur, size = agents.Batch{}, 0
		}
	}
	for _, w := range b.Works {
		cur.Works = append(cur.Works, w)
		if size++; size == agents.MaxRecords {
			flush()
		}
	}
	for _, c := range b.Conversations {
		cur.Conversations = append(cur.Conversations, c)
		if size++; size == agents.MaxRecords {
			flush()
		}
	}
	for _, k := range b.Buckets {
		cur.Buckets = append(cur.Buckets, k)
		if size++; size == agents.MaxRecords {
			flush()
		}
	}
	for _, u := range b.Uses {
		cur.Uses = append(cur.Uses, u)
		if size++; size == agents.MaxRecords {
			flush()
		}
	}
	flush()
	if b.Installed != nil {
		if len(parts) == 0 {
			parts = append(parts, agents.Batch{})
		}
		parts[len(parts)-1].Installed = b.Installed
	}
	for _, p := range parts {
		if err := s.send(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func (s *Shipper) send(ctx context.Context, b agents.Batch) error {
	body, err := json.Marshal(b)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL+"/v1/usage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("tally answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Reset withdraws everything this machine posted to tally and forgets the
// shipper's place, so the next round posts again from the start under the
// rules it holds now. Stop the running shipper first: it keeps its place in
// memory and would save it again.
func (s *Shipper) Reset(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.cfg.URL+"/v1/usage", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("tally answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if err := os.Remove(s.cfg.State); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.files, s.known, s.sent = map[string]fileState{}, map[string]Known{}, map[string]string{}
	s.convs, s.owner = map[string]*Conv{}, map[uint64]string{}
	s.log.Info("ship reset", "url", s.cfg.URL)
	return nil
}

// forget drops every conversation quiet for longer than Quiet, and its
// claimed replies.
func (s *Shipper) forget(now time.Time) int {
	n := 0
	for id, c := range s.convs {
		if now.Sub(c.Last) < Quiet {
			continue
		}
		for k := range c.Seen {
			if s.owner[k] == id {
				delete(s.owner, k)
			}
		}
		delete(s.convs, id)
		n++
	}
	return n
}

// saved is the shipper's place on disk.
type saved struct {
	// Version is the rules the place was kept under (stateVersion).
	Version int                  `json:"version,omitempty"`
	Files   map[string]fileState `json:"files"`
	Known   map[string]Known     `json:"known"`
	Sent    map[string]string    `json:"sent"`
	Active  map[string]savedConv `json:"active"`
}

type savedConv struct {
	Seen    []uint64      `json:"seen"`
	Buckets []savedBucket `json:"buckets"`
	Uses    []savedUse    `json:"uses,omitempty"`
	Model   string        `json:"model,omitempty"`
	Last    time.Time     `json:"last"`
}

type savedUse struct {
	UseKey
	Count int64 `json:"count"`
}

type savedBucket struct {
	Model  string `json:"model"`
	Minute int64  `json:"minute"`
	agents.Tokens
}

func (s *Shipper) save() error {
	out := saved{Version: stateVersion, Files: s.files, Known: s.known, Sent: s.sent, Active: map[string]savedConv{}}
	for id, c := range s.convs {
		sc := savedConv{Last: c.Last, Model: c.Model}
		for k, n := range c.Uses {
			sc.Uses = append(sc.Uses, savedUse{UseKey: k, Count: n})
		}
		for k := range c.Seen {
			sc.Seen = append(sc.Seen, k)
		}
		for k, t := range c.Buckets {
			sc.Buckets = append(sc.Buckets, savedBucket{Model: k.Model, Minute: k.Minute, Tokens: t})
		}
		out.Active[id] = sc
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.State), 0o700); err != nil {
		return err
	}
	tmp := s.cfg.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.cfg.State)
}

func (s *Shipper) load() error {
	b, err := os.ReadFile(s.cfg.State)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var in saved
	if err := json.Unmarshal(b, &in); err != nil {
		return fmt.Errorf("%s: %w", s.cfg.State, err)
	}
	for k, v := range in.Files {
		s.files[k] = v
	}
	for k, v := range in.Known {
		s.known[k] = v
	}
	for k, v := range in.Sent {
		s.sent[k] = v
	}
	for id, sc := range in.Active {
		c := NewConv()
		c.Last, c.Model = sc.Last, sc.Model
		for _, su := range sc.Uses {
			c.Uses[su.UseKey] = su.Count
		}
		for _, k := range sc.Seen {
			c.Seen[k] = struct{}{}
			s.owner[k] = id
		}
		for _, sb := range sc.Buckets {
			c.Buckets[BucketKey{Model: sb.Model, Minute: sb.Minute}] = sb.Tokens
		}
		s.convs[id] = c
	}
	if in.Version < stateVersion {
		s.reread(time.Now())
	}
	return nil
}

// stateVersion counts the rules a shipper posts under. A place kept under
// earlier rules is read again where the new ones post more.
const stateVersion = 2

// installedMark is where the shipper remembers the installed plugins tally
// last took.
const installedMark = "installed"

// Reread is how far back a shipper under new rules reads again: the span a
// count of uses covers.
const Reread = 31 * 24 * time.Hour

// reread makes the next round read again every Claude Code transcript
// written to within Reread, and post again every use an active
// conversation holds: version 2 posts a plugin's use off the route, which
// earlier rules counted and kept. Every record is an upsert of a whole
// minute, so what tally already took lands as it was.
func (s *Shipper) reread(now time.Time) {
	n := 0
	for path := range s.files {
		if !strings.HasPrefix(path, s.cfg.Claude) {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil || now.Sub(fi.ModTime()) > Reread {
			continue
		}
		delete(s.files, path)
		n++
	}
	for _, c := range s.convs {
		for k := range c.Uses {
			c.DirtyUses[k] = struct{}{}
		}
	}
	s.log.Info("reading again under new rules", "transcripts", n, "version", stateVersion)
}
