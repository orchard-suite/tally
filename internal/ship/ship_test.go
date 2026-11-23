package ship

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
)

const session = "11111111-1111-4111-8111-111111111111"

func minute(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestClaudeFile(t *testing.T) {
	f, ok := ClaudeFile("/p/-x/" + session + ".jsonl")
	if !ok || f.Conversation != "claude:"+session || f.Agent != "" {
		t.Fatalf("session: %+v %v", f, ok)
	}
	f, ok = ClaudeFile("/p/-x/" + session + "/subagents/agent-abc.jsonl")
	if !ok || f.Conversation != "claude:"+session+"/abc" || f.Session != session || f.Agent != "abc" {
		t.Fatalf("subagent: %+v %v", f, ok)
	}
	if _, ok := ClaudeFile("/p/-x/" + session + "/subagents/agent-abc.meta.json"); ok {
		t.Fatal("a meta file is not a transcript")
	}
}

func TestReadCountsEachReplyOnce(t *testing.T) {
	path := filepath.Join("testdata", "projects", "-home-user-src-demo", session+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := NewConv()
	var info Info
	seen := map[uint64]bool{}
	n, err := Read(f, c, &info, func(k uint64) bool {
		if seen[k] {
			return false
		}
		seen[k] = true
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if want := int64(strings.LastIndexByte(string(raw), '\n') + 1); n != want {
		t.Fatalf("consumed %d, want %d: the unfinished last line waits", n, want)
	}
	if info.Title != "Thing builder" || info.CWD != "/home/user/src/demo" {
		t.Fatalf("info %+v: a title a person set wins", info)
	}
	want := map[BucketKey]agents.Tokens{
		{"claude-opus-5-5", minute("2026-11-04T09:00:00Z").Unix()}:  {Requests: 2, Input: 15, CacheWrite5m: 200, CacheWrite1h: 1000, CacheRead: 11000, Output: 150},
		{"claude-fable-5-1", minute("2026-11-04T09:05:00Z").Unix()}: {Requests: 1, Input: 1, CacheRead: 7000, Output: 20},
		{"claude-opus-5-5", minute("2026-11-04T09:06:00Z").Unix()}:  {Requests: 1, Input: 1, CacheRead: 1, Output: 1},
	}
	if len(c.Buckets) != len(want) {
		t.Fatalf("buckets %v", c.Buckets)
	}
	for k, w := range want {
		if c.Buckets[k] != w {
			t.Errorf("%v: got %+v want %+v", k, c.Buckets[k], w)
		}
	}
	if !c.Last.Equal(minute("2026-11-04T09:06:00Z")) {
		t.Errorf("last %v", c.Last)
	}
}

// fakeTally takes posts like tally does: every record replaces its key.
type fakeTally struct {
	works   map[string]agents.Work
	convs   map[string]agents.Conversation
	buckets map[string]agents.Bucket
	posts   int
	refuse  bool
}

func (f *fakeTally) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Path != "/v1/usage" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	if f.refuse {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	var b agents.Batch
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &b); err != nil || b.Validate() != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	f.posts++
	for _, x := range b.Works {
		f.works[x.ID] = x
	}
	for _, x := range b.Conversations {
		f.convs[x.ID] = x
	}
	for _, x := range b.Buckets {
		f.buckets[x.Conversation+" "+x.Model+" "+x.Minute.Format(time.RFC3339)] = x
	}
	w.WriteHeader(http.StatusNoContent)
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRoundPostsWholeMinutesOnceAcrossCopiesAndForks(t *testing.T) {
	dir := t.TempDir()
	projects := filepath.Join(dir, "projects")
	copyTree(t, filepath.Join("testdata", "projects"), projects)
	tally := &fakeTally{works: map[string]agents.Work{}, convs: map[string]agents.Conversation{}, buckets: map[string]agents.Bucket{}}
	srv := httptest.NewServer(tally)
	defer srv.Close()
	cfg := Config{URL: srv.URL, Token: "secret", Claude: projects, T3: filepath.Join(dir, "none.sqlite"), State: filepath.Join(dir, "state", "ship.json"), Every: time.Minute}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Round(context.Background()); err != nil {
		t.Fatal(err)
	}

	lead := "claude:" + session
	fork := lead + "/afork1"
	if got := tally.buckets[lead+" claude-opus-5-5 2026-11-04T09:00:00Z"].Tokens; got != (agents.Tokens{Requests: 2, Input: 15, CacheWrite5m: 200, CacheWrite1h: 1000, CacheRead: 11000, Output: 150}) {
		t.Fatalf("the moved project's copy and the stream's repeats count once: %+v", got)
	}
	if _, ok := tally.buckets[fork+" claude-opus-5-5 2026-11-04T09:00:00Z"]; ok {
		t.Fatal("the fork's copy of its parent's reply stays the parent's")
	}
	if got := tally.buckets[fork+" claude-haiku-5-5 2026-11-04T09:02:00Z"].Tokens; got != (agents.Tokens{Requests: 1, Input: 300, Output: 30}) {
		t.Fatalf("fork's own reply: %+v", got)
	}
	if c := tally.convs[fork]; c.Role != agents.Child || c.Parent != lead || c.Work != lead || c.Title != "Check the edges" {
		t.Fatalf("fork conversation %+v", c)
	}
	if c := tally.convs[lead]; c.Role != agents.Lead || c.Work != lead {
		t.Fatalf("lead conversation %+v", c)
	}
	if w := tally.works[lead]; w.Title != "Thing builder" || w.Project != "demo" {
		t.Fatalf("work %+v", w)
	}

	// The unfinished line finishes; the next round posts that minute alone,
	// and posts nothing it posted before.
	posts := tally.posts
	path := filepath.Join(projects, "-home-user-src-demo", session+".jsonl")
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString(`-5-5","usage":{"input_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":3,"output_tokens":4}}}` + "\n")
	fh.Close()

	// A fresh shipper from the saved place, as after a restart.
	s, err = New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	before := len(tally.buckets)
	if err := s.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tally.posts != posts+1 || len(tally.buckets) != before+1 {
		t.Fatalf("posts %d->%d, buckets %d->%d", posts, tally.posts, before, len(tally.buckets))
	}
	if got := tally.buckets[lead+" claude-opus-5-5 2026-11-04T09:07:00Z"].Tokens; got != (agents.Tokens{Requests: 1, Input: 2, CacheRead: 3, Output: 4}) {
		t.Fatalf("finished line: %+v", got)
	}

	// Nothing new: no post at all.
	if err := s.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tally.posts != posts+1 {
		t.Fatal("a quiet round posts nothing")
	}
}

func TestRoundKeepsChangesWhenTallyIsDown(t *testing.T) {
	dir := t.TempDir()
	projects := filepath.Join(dir, "projects")
	copyTree(t, filepath.Join("testdata", "projects"), projects)
	tally := &fakeTally{works: map[string]agents.Work{}, convs: map[string]agents.Conversation{}, buckets: map[string]agents.Bucket{}, refuse: true}
	srv := httptest.NewServer(tally)
	defer srv.Close()
	cfg := Config{URL: srv.URL, Token: "secret", Claude: projects, T3: filepath.Join(dir, "none.sqlite"), State: filepath.Join(dir, "ship.json"), Every: time.Minute}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Round(context.Background()); err == nil {
		t.Fatal("a refused post fails the round")
	}
	if _, err := os.Stat(cfg.State); err == nil {
		t.Fatal("the place is saved only once tally took everything")
	}
	tally.refuse = false
	if err := s.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tally.buckets) != 4 {
		t.Fatalf("the next round sends what the refused one held: %d buckets", len(tally.buckets))
	}
}

func TestResolvePlacesT3SessionsInTheirRootWork(t *testing.T) {
	t3 := T3{
		BySession: map[string]string{"s-root": "thread-root", "s-child": "thread-child"},
		Threads: map[string]Thread{
			"thread-root":  {ID: "thread-root", Title: "Polish the board", Project: "tally"},
			"thread-child": {ID: "thread-child", Title: "Check colors", Project: "tally", Parent: "thread-root"},
		},
	}
	known := map[string]Known{
		"claude:s-root":      {Session: "s-root", Info: Info{Title: "agent title", CWD: "/x/tally"}},
		"claude:s-child":     {Session: "s-child"},
		"claude:s-child/a1":  {Session: "s-child", Agent: "a1", Info: Info{Title: "Look"}},
		"claude:s-bare":      {Session: "s-bare", Info: Info{Title: "Bare", CWD: "/x/elsewhere"}},
		"claude:s-bare/a2":   {Session: "s-bare", Agent: "a2", ParentAgent: "a1"},
		"claude:s-orphan/a3": {Session: "s-orphan", Agent: "a3"},
		"claude:s-untitled":  {Session: "s-untitled"},
		"claude:s-root/a4":   {Session: "s-root", Agent: "a4", Info: Info{Title: "Fork"}},
	}
	works, convs := Resolve(known, t3, nil)
	byID := map[string]agents.Conversation{}
	for _, c := range convs {
		byID[c.ID] = c
	}
	check := func(id, work, role, parent, title string) {
		t.Helper()
		c := byID[id]
		if c.Work != work || c.Role != role || c.Parent != parent || c.Title != title {
			t.Errorf("%s: %+v, want work %s role %s parent %q title %q", id, c, work, role, parent, title)
		}
	}
	check("claude:s-root", "t3:thread-root", agents.Lead, "", "Polish the board")
	check("claude:s-child", "t3:thread-root", agents.Child, "", "Check colors")
	check("claude:s-child/a1", "t3:thread-root", agents.Child, "claude:s-child", "Look")
	check("claude:s-root/a4", "t3:thread-root", agents.Child, "claude:s-root", "Fork")
	check("claude:s-bare", "claude:s-bare", agents.Lead, "", "Bare")
	check("claude:s-bare/a2", "claude:s-bare", agents.Child, "claude:s-bare/a1", "Untitled")
	check("claude:s-orphan/a3", "claude:s-orphan", agents.Child, "claude:s-orphan", "Untitled")

	sort.Slice(works, func(a, b int) bool { return works[a].ID < works[b].ID })
	want := []agents.Work{
		{ID: "claude:s-bare", Title: "Bare", Project: "elsewhere"},
		{ID: "claude:s-orphan", Title: "Untitled"},
		{ID: "claude:s-untitled", Title: "Untitled"},
		{ID: "t3:thread-root", Title: "Polish the board", Project: "tally"},
	}
	if len(works) != len(want) {
		t.Fatalf("works %+v", works)
	}
	for i := range want {
		if works[i] != want[i] {
			t.Errorf("work %d: %+v want %+v", i, works[i], want[i])
		}
	}
}

func TestReadT3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE projection_projects (project_id TEXT, title TEXT)`,
		`CREATE TABLE orchestration_v2_projection_threads (thread_id TEXT, title TEXT, project_id TEXT, payload_json TEXT)`,
		`CREATE TABLE orchestration_v2_projection_provider_threads (provider_thread_id TEXT, thread_id TEXT, payload_json TEXT)`,
		`INSERT INTO projection_projects VALUES ('p1', 'tally')`,
		`INSERT INTO orchestration_v2_projection_threads VALUES ('t-root', 'Root', 'p1', '{"lineage":{"parentThreadId":null}}')`,
		`INSERT INTO orchestration_v2_projection_threads VALUES ('t-kid', 'Kid', 'p1', '{"lineage":{"parentThreadId":"t-root","rootThreadId":"t-root"}}')`,
		`INSERT INTO orchestration_v2_projection_provider_threads VALUES ('pt1', 't-root', '{"driver":"claudeAgent","nativeThreadRef":{"nativeId":"s1"}}')`,
		`INSERT INTO orchestration_v2_projection_provider_threads VALUES ('pt2', 't-kid', '{"driver":"claudeAgent","nativeThreadRef":{"nativeId":"s2"}}')`,
		`INSERT INTO orchestration_v2_projection_provider_threads VALUES ('pt3', 't-kid', '{"driver":"codex","nativeThreadRef":{"nativeId":"c1"}}')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	t3, err := ReadT3(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if t3.BySession["s1"] != "t-root" || t3.BySession["s2"] != "t-kid" || len(t3.BySession) != 2 {
		t.Fatalf("by session %v", t3.BySession)
	}
	if th := t3.Threads["t-kid"]; th.Parent != "t-root" || th.Project != "tally" || th.Title != "Kid" {
		t.Fatalf("thread %+v", th)
	}
	if t3.Root("t-kid") != "t-root" || t3.Root("t-root") != "t-root" {
		t.Fatal("root")
	}
	none, err := ReadT3(context.Background(), filepath.Join(t.TempDir(), "missing.sqlite"))
	if err != nil || len(none.Threads) != 0 {
		t.Fatalf("a missing database is no T3: %v %v", none, err)
	}
}
