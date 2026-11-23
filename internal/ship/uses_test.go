package ship

import (
	"strings"
	"testing"

	"github.com/orchard-suite/tally/internal/agents"
)

// A transcript with a command typed before the first reply, a reply written
// once per content block (a Bash call and a Skill call), a copy of the Bash
// block, and a later reply calling a plugin's MCP tool.
const usesTranscript = `{"type":"user","uuid":"u1","timestamp":"2026-11-15T10:00:05Z","message":{"role":"user","content":"<command-message>orrery-catalog</command-message>\n<command-name>/orrery:orrery-catalog</command-name>"}}
{"type":"assistant","timestamp":"2026-11-15T10:00:30Z","requestId":"r1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"secret things"}}],"usage":{"input_tokens":10,"output_tokens":5}}}
{"type":"assistant","timestamp":"2026-11-15T10:00:30Z","requestId":"r1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_2","name":"Skill","input":{"skill":"orrery:orrery-model"}}],"usage":{"input_tokens":10,"output_tokens":5}}}
{"type":"assistant","timestamp":"2026-11-15T10:00:30Z","requestId":"r1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"secret things"}}],"usage":{"input_tokens":10,"output_tokens":5}}}
{"type":"assistant","timestamp":"2026-11-15T10:01:10Z","requestId":"r2","message":{"id":"m2","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_3","name":"mcp__plugin_orrery_orrery__search_entities","input":{"q":"x"}}],"usage":{"input_tokens":3,"output_tokens":1}}}
`

func TestReadCountsEachUseOnceByName(t *testing.T) {
	c := NewConv()
	var info Info
	seen := map[uint64]bool{}
	claim := func(k uint64) bool {
		if seen[k] {
			return false
		}
		seen[k] = true
		return true
	}
	if _, err := Read(strings.NewReader(usesTranscript), c, &info, claim); err != nil {
		t.Fatal(err)
	}
	m0, m1 := minute("2026-11-15T10:00:00Z").Unix(), minute("2026-11-15T10:01:00Z").Unix()
	want := map[UseKey]int64{
		{Model: "", Kind: agents.UseCommand, Name: "orrery:orrery-catalog", Minute: m0}:                                  1,
		{Model: "claude-opus-5-5", Kind: agents.UseTool, Name: "Bash", Minute: m0}:                                       1,
		{Model: "claude-opus-5-5", Kind: agents.UseTool, Name: "Skill", Minute: m0}:                                      1,
		{Model: "claude-opus-5-5", Kind: agents.UseSkill, Name: "orrery:orrery-model", Minute: m0}:                       1,
		{Model: "claude-opus-5-5", Kind: agents.UseTool, Name: "mcp__plugin_orrery_orrery__search_entities", Minute: m1}: 1,
	}
	if len(c.Uses) != len(want) {
		t.Fatalf("uses = %v, want %v", c.Uses, want)
	}
	for k, n := range want {
		if c.Uses[k] != n {
			t.Errorf("%+v = %d, want %d", k, c.Uses[k], n)
		}
	}
	if c.Model != "claude-opus-5-5" {
		t.Errorf("the conversation's model is its newest reply's: %q", c.Model)
	}
	// Reading the same lines again counts nothing more.
	if _, err := Read(strings.NewReader(usesTranscript), c, &info, claim); err != nil {
		t.Fatal(err)
	}
	if c.Uses[UseKey{Model: "claude-opus-5-5", Kind: agents.UseTool, Name: "Bash", Minute: m0}] != 1 {
		t.Error("a tool call read twice is counted once")
	}
}

func TestChangesSendUsesWithTheirRepliesModel(t *testing.T) {
	s := &Shipper{convs: map[string]*Conv{}, known: map[string]Known{}, sent: map[string]string{}}
	c := NewConv()
	seen := map[uint64]bool{}
	if _, err := Read(strings.NewReader(usesTranscript), c, &Info{}, func(k uint64) bool { r := !seen[k]; seen[k] = true; return r }); err != nil {
		t.Fatal(err)
	}
	s.convs["claude:"+session] = c
	b, _, _ := s.changes(nil, nil, Route{})
	if len(b.Uses) != 5 {
		t.Fatalf("every use is sent: %+v", b.Uses)
	}
	for _, u := range b.Uses {
		if u.Model != "claude-opus-5-5" {
			t.Errorf("%s %s is sent under its conversation's model: %q", u.Kind, u.Name, u.Model)
		}
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("the batch is one tally takes: %v", err)
	}
}

func TestChangesOffTheRouteSendOnlyAPluginsUseAndBare(t *testing.T) {
	s := &Shipper{convs: map[string]*Conv{}, known: map[string]Known{}, sent: map[string]string{}}
	c := NewConv()
	seen := map[uint64]bool{}
	if _, err := Read(strings.NewReader(usesTranscript), c, &Info{}, func(k uint64) bool { r := !seen[k]; seen[k] = true; return r }); err != nil {
		t.Fatal(err)
	}
	id := "claude:" + session
	s.convs[id] = c
	// Claude Code on its own subscription: none of its replies is the gateway's.
	b, _, _ := s.changes(nil, nil, Route{Host: "gateway.example.com"})
	if len(b.Buckets) != 0 || len(b.Conversations) != 0 || len(b.Works) != 0 {
		t.Fatalf("off the route no reply, conversation, or work leaves: %+v", b)
	}
	names := map[string]bool{}
	for _, u := range b.Uses {
		names[u.Name] = true
		if u.Conversation != agents.Bare(id) || strings.Contains(u.Conversation, session) {
			t.Errorf("%s is posted bare, never under its conversation: %q", u.Name, u.Conversation)
		}
	}
	if len(b.Uses) != 3 || !names["orrery:orrery-catalog"] || !names["orrery:orrery-model"] || !names["mcp__plugin_orrery_orrery__search_entities"] {
		t.Fatalf("a plugin's uses leave, and Bash and Skill stay: %+v", b.Uses)
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("the batch is one tally takes: %v", err)
	}
}
