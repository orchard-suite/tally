package ship

import (
	"bufio"
	"bytes"
	"encoding/json"
	"hash/fnv"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
)

// Claude Code writes one transcript per session at
// <projects>/<project>/<session>.jsonl and one per subagent at
// <projects>/<project>/<session>/subagents/agent-<id>.jsonl, beside a
// agent-<id>.meta.json that names what the subagent was asked to do.

// File is one transcript and the conversation it holds.
type File struct {
	Path string
	// Conversation is claude:<session> or claude:<session>/<agent>.
	Conversation string
	Session      string
	// Agent is the subagent's id, empty for a session.
	Agent string
}

// ClaudeFile names the conversation a transcript path holds, or false for a
// path that is not a transcript.
func ClaudeFile(path string) (File, bool) {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".jsonl") {
		return File{}, false
	}
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "subagents" {
		agent, ok := strings.CutPrefix(strings.TrimSuffix(base, ".jsonl"), "agent-")
		session := filepath.Base(filepath.Dir(dir))
		if !ok || agent == "" || session == "" {
			return File{}, false
		}
		return File{Path: path, Conversation: "claude:" + session + "/" + agent, Session: session, Agent: agent}, true
	}
	session := strings.TrimSuffix(base, ".jsonl")
	return File{Path: path, Conversation: "claude:" + session, Session: session}, true
}

// SubagentMeta is the part of agent-<id>.meta.json the shipper reads.
type SubagentMeta struct {
	Description   string `json:"description"`
	ParentAgentID string `json:"parentAgentId"`
	SpawnDepth    int    `json:"spawnDepth"`
}

// Conv is what the shipper knows of one conversation while it is active:
// the replies it has counted and its totals by model and minute.
type Conv struct {
	// Seen are the replies and tool calls this conversation claimed.
	Seen    map[uint64]struct{}
	Buckets map[BucketKey]agents.Tokens
	// Dirty are the buckets changed since the last post.
	Dirty map[BucketKey]struct{}
	// Uses are the skills, commands, and tools it used, by minute.
	Uses map[UseKey]int64
	// DirtyUses are the uses changed since the last post.
	DirtyUses map[UseKey]struct{}
	// Model is the model of its newest reply: a command the person typed
	// is counted under it, since a typed line names no model.
	Model string
	Last  time.Time
}

// UseKey is one skill, command, or tool in one minute, in replies of one
// model. A command's model is empty until the conversation has a reply.
type UseKey struct {
	Model  string
	Kind   string
	Name   string
	Minute int64
}

// BucketKey is one model in one minute.
type BucketKey struct {
	Model  string
	Minute int64
}

// NewConv is an empty conversation.
func NewConv() *Conv {
	return &Conv{Seen: map[uint64]struct{}{}, Buckets: map[BucketKey]agents.Tokens{}, Dirty: map[BucketKey]struct{}{}, Uses: map[UseKey]int64{}, DirtyUses: map[UseKey]struct{}{}}
}

// Info is what a transcript says about its conversation beyond its usage.
type Info struct {
	Title string `json:"title,omitempty"`
	CWD   string `json:"cwd,omitempty"`
}

// Claim takes a reply for a conversation, and says false when the reply was
// already counted: a forked subagent's transcript repeats its parent's
// replies, and a moved project leaves a copy of every transcript behind.
type Claim func(key uint64) bool

type entry struct {
	Type        string    `json:"type"`
	UUID        string    `json:"uuid"`
	Timestamp   time.Time `json:"timestamp"`
	RequestID   string    `json:"requestId"`
	CWD         string    `json:"cwd"`
	AITitle     string    `json:"aiTitle"`
	CustomTitle string    `json:"customTitle"`
	Message     *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		// Content is a reply's blocks, or a person's line as a string. Only
		// a tool call's id and name, and the name of the skill a Skill call
		// loads, are read from it.
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input         int64 `json:"input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			Output        int64 `json:"output_tokens"`
			Ephemeral     *struct {
				FiveMinutes int64 `json:"ephemeral_5m_input_tokens"`
				OneHour     int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// Read counts every whole line of r into c and info, and returns how many
// bytes it consumed: a last line without its newline is left for the next
// read. A reply is counted once, by its message and request ids, when claim
// takes it.
func Read(r io.Reader, c *Conv, info *Info, claim Claim) (int64, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var consumed int64
	for {
		line, err := br.ReadBytes('\n')
		if err == io.EOF {
			return consumed, nil
		}
		if err != nil {
			return consumed, err
		}
		consumed += int64(len(line))
		readLine(line, c, info, claim)
	}
}

var (
	markCommand   = []byte(`<command-name>/`)
	markAssistant = []byte(`"type":"assistant"`)
	markAITitle   = []byte(`"type":"ai-title"`)
	markCustom    = []byte(`"type":"custom-title"`)
	markCWD       = []byte(`"cwd":"`)
)

func readLine(line []byte, c *Conv, info *Info, claim Claim) {
	assistant := bytes.Contains(line, markAssistant)
	title := bytes.Contains(line, markAITitle) || bytes.Contains(line, markCustom)
	needCWD := info.CWD == "" && bytes.Contains(line, markCWD)
	command := !assistant && bytes.Contains(line, markCommand)
	if !assistant && !title && !needCWD && !command {
		return
	}
	var e entry
	if json.Unmarshal(line, &e) != nil {
		return
	}
	if e.CWD != "" && info.CWD == "" {
		info.CWD = e.CWD
	}
	switch e.Type {
	case "custom-title":
		if e.CustomTitle != "" {
			info.Title = e.CustomTitle
		}
	case "ai-title":
		// A title the person set wins over the one the agent wrote.
		if e.AITitle != "" && info.Title == "" {
			info.Title = e.AITitle
		}
	case "assistant":
		uses(e, c, claim)
		count(e, c, claim)
	case "user":
		typed(e, c, claim)
	}
}

// block is the part of a reply's content block the shipper reads.
type block struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input struct {
		Skill string `json:"skill"`
	} `json:"input"`
}

// uses counts a reply's tool calls, each once by its id: Claude Code writes
// a reply once per content block, and a copied transcript repeats them. A
// Skill call also counts the skill it loads.
func uses(e entry, c *Conv, claim Claim) {
	m := e.Message
	if m == nil || m.Model == "" || m.Model == "<synthetic>" || e.Timestamp.IsZero() || len(m.Content) == 0 || m.Content[0] != '[' {
		return
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_use" || b.ID == "" || b.Name == "" {
			continue
		}
		key := useKey(b.ID)
		if !claim(key) {
			continue
		}
		c.Seen[key] = struct{}{}
		use(c, m.Model, agents.UseTool, b.Name, e.Timestamp)
		if b.Name == "Skill" && b.Input.Skill != "" {
			use(c, m.Model, agents.UseSkill, b.Input.Skill, e.Timestamp)
		}
	}
}

// typed counts a slash command the person typed, once by its line's id. A
// typed line names no model, so it takes the conversation's newest reply's,
// or waits for its first.
func typed(e entry, c *Conv, claim Claim) {
	if e.Message == nil || e.UUID == "" || e.Timestamp.IsZero() {
		return
	}
	var text string
	if json.Unmarshal(e.Message.Content, &text) != nil {
		return
	}
	_, rest, ok := strings.Cut(text, "<command-name>/")
	if !ok {
		return
	}
	name, _, ok := strings.Cut(rest, "</command-name>")
	if !ok || name == "" || strings.ContainsAny(name, " \n") {
		return
	}
	key := useKey("command:" + e.UUID)
	if !claim(key) {
		return
	}
	c.Seen[key] = struct{}{}
	use(c, c.Model, agents.UseCommand, name, e.Timestamp)
}

// use counts one use into its minute.
func use(c *Conv, model, kind, name string, at time.Time) {
	k := UseKey{Model: model, Kind: kind, Name: name, Minute: at.UTC().Truncate(time.Minute).Unix()}
	c.Uses[k]++
	c.DirtyUses[k] = struct{}{}
	if at.After(c.Last) {
		c.Last = at
	}
}

// useKey is one tool call's or typed line's identity, apart from every
// reply's.
func useKey(id string) uint64 {
	h := fnv.New64a()
	h.Write([]byte("use\x00"))
	h.Write([]byte(id))
	return h.Sum64()
}

func count(e entry, c *Conv, claim Claim) {
	m := e.Message
	if m == nil || m.Usage == nil || m.Model == "" || m.Model == "<synthetic>" || e.Timestamp.IsZero() {
		return
	}
	id := m.ID
	if id == "" {
		id = e.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	key := replyKey(id, e.RequestID)
	if !claim(key) {
		return
	}
	u := m.Usage
	t := agents.Tokens{Requests: 1, Input: u.Input, CacheRead: u.CacheRead, Output: u.Output}
	// The split by how long a write is kept, when the transcript has it;
	// otherwise every write is the five-minute kind, the API's default.
	if u.Ephemeral != nil && u.Ephemeral.FiveMinutes+u.Ephemeral.OneHour == u.CacheCreation {
		t.CacheWrite5m, t.CacheWrite1h = u.Ephemeral.FiveMinutes, u.Ephemeral.OneHour
	} else {
		t.CacheWrite5m = u.CacheCreation
	}
	add(c, key, m.Model, e.Timestamp, t)
}

// add counts one reply's tokens into its model's minute.
func add(c *Conv, key uint64, model string, at time.Time, t agents.Tokens) {
	c.Seen[key] = struct{}{}
	c.Model = model
	k := BucketKey{Model: model, Minute: at.UTC().Truncate(time.Minute).Unix()}
	b := c.Buckets[k]
	b.Add(t)
	c.Buckets[k] = b
	c.Dirty[k] = struct{}{}
	if at.After(c.Last) {
		c.Last = at
	}
}

// replyKey is one reply's identity. Claude Code writes a reply once per
// content block, every copy with the same ids and usage.
func replyKey(message, request string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(message))
	h.Write([]byte{0})
	h.Write([]byte(request))
	return h.Sum64()
}
