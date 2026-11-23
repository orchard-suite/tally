package ship

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
)

const (
	ionRoot     = "1790000000000-aaaaaaaaaaaa"
	ionChild    = "1790000000500-bbbbbbbbbbbb"
	ionCont     = "1790000900000-cccccccccccc"
	ionFork     = "1790001000000-dddddddddddd"
	ionMirror   = "1790000000600-eeeeeeeeeeee"
	ionLegacy   = "1777000000000"
	nativeClaud = "22222222-2222-4222-8222-222222222222"
	dispClaude  = "33333333-3333-4333-8333-333333333333"
)

func ionMinute(ms int64) time.Time {
	return time.UnixMilli(ms).UTC().Truncate(time.Minute)
}

func TestIonFile(t *testing.T) {
	for path, want := range map[string]string{
		"/x/" + ionRoot + ".tree.jsonl":     ionRoot,
		"/x/" + ionLegacy + ".jsonl":        ionLegacy,
		"/x/" + ionRoot + ".llm.jsonl":      "",
		"/x/" + ionRoot + ".memory.md":      "",
		"/x/" + ionRoot + ".tree.jsonl.bak": "",
	} {
		id, ok := IonFile(path)
		if id != want || ok != (want != "") {
			t.Errorf("IonFile(%q) = %q %v, want %q", path, id, ok, want)
		}
	}
}

func readIonFixture(t *testing.T, id string) (*Conv, IonHeader, map[string]string) {
	t.Helper()
	dir := filepath.Join("testdata", "ion", "conversations")
	var base IonHeader
	if fh, err := os.Open(filepath.Join(dir, id+".llm.jsonl")); err == nil {
		base, _ = ReadIonHeader(fh)
		fh.Close()
	}
	name := id + ".tree.jsonl"
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		name = id + ".jsonl"
	}
	fh, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	c := NewConv()
	h, dispatched, err := ReadIon(fh, c, base, func(uint64) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return c, h, dispatched
}

func TestReadIon(t *testing.T) {
	c, h, dispatched := readIonFixture(t, ionRoot)
	T := int64(1790000000000)
	want := map[BucketKey]agents.Tokens{
		{"claude-opus-5-5", ionMinute(T + 5000).Unix()}:                 {Requests: 1, Input: 10, CacheWrite5m: 2000, CacheRead: 5000, Output: 100},
		{"acme-gateway/claude-sonnet-5-5", ionMinute(T + 65000).Unix()}: {Requests: 1, Input: 3, Output: 30},
	}
	if len(c.Buckets) != len(want) {
		t.Fatalf("buckets %v", c.Buckets)
	}
	for k, w := range want {
		if c.Buckets[k] != w {
			t.Errorf("%v: %+v want %+v", k, c.Buckets[k], w)
		}
	}
	for k := range c.Buckets {
		if k.Minute == ionMinute(T+120000).Unix() {
			t.Error("a copy of a run Claude Code served counts in Claude Code's transcript, not here")
		}
	}
	if h.CreatedAt != T || h.WorkingDirectory != "/home/user/src/ops" || h.NativeSessions["claude-code"].Cursor != nativeClaud {
		t.Errorf("both headers merge: %+v", h)
	}
	if dispatched[dispClaude] != "Poll check" || dispatched[ionChild] != "Security Officer" || len(dispatched) != 2 {
		t.Errorf("dispatched %v", dispatched)
	}

	// A continued conversation and a fork start with a copy of the root's
	// reply; only their own replies count.
	for _, id := range []string{ionCont, ionFork} {
		c, _, _ := readIonFixture(t, id)
		if len(c.Buckets) != 1 {
			t.Errorf("%s: %v", id, c.Buckets)
		}
		for k := range c.Buckets {
			if k.Minute == ionMinute(T+5000).Unix() {
				t.Errorf("%s counted the copy of its parent's reply", id)
			}
		}
	}
	if c, _, _ := readIonFixture(t, ionMirror); len(c.Buckets) != 0 {
		t.Error("a mirror records no usage")
	}
	if c, h, _ := readIonFixture(t, ionLegacy); len(c.Buckets) != 1 || h.WorkingDirectory != "/home/user/old" {
		t.Errorf("a file from before the split: %v %+v", c.Buckets, h)
	}
}

func TestResolveIon(t *testing.T) {
	t3 := T3{
		BySession: map[string]string{"acp-0123456789abcdef": "t-ops"},
		Threads:   map[string]Thread{"t-ops": {ID: "t-ops", Title: "Ops review", Project: "ops"}},
	}
	known := map[string]Known{
		"ion:" + ionRoot:               {Source: SourceIon, Session: ionRoot, Info: Info{CWD: "/home/user/src/ops"}, Ion: &IonLinks{Dispatched: map[string]string{dispClaude: "Poll check", ionChild: "Security Officer"}, Native: []string{nativeClaud}}},
		"ion:" + ionChild:              {Source: SourceIon, Session: ionChild, Ion: &IonLinks{Parent: ionRoot}},
		"ion:" + ionCont:               {Source: SourceIon, Session: ionCont, Ion: &IonLinks{Parent: ionRoot}},
		"ion:" + ionFork:               {Source: SourceIon, Session: ionFork, Info: Info{CWD: "/home/user/src/lab"}, Ion: &IonLinks{}},
		"ion:" + ionLegacy:             {Source: SourceIon, Session: ionLegacy, Info: Info{CWD: "/home/user/old"}, Ion: &IonLinks{}},
		"claude:" + nativeClaud:        {Session: nativeClaud, Info: Info{Title: "agent title"}},
		"claude:" + dispClaude:         {Session: dispClaude, Info: Info{Title: "agent title"}},
		"claude:" + dispClaude + "/a1": {Session: dispClaude, Agent: "a1"},
	}
	bindings := map[string]string{"acp-0123456789abcdef": ionCont, "unknown-key": ionLegacy}
	works, convs := Resolve(known, t3, bindings)
	byID := map[string]agents.Conversation{}
	for _, c := range convs {
		byID[c.ID] = c
	}
	check := func(id, work, role, parent, title, agent string) {
		t.Helper()
		c := byID[id]
		if c.Work != work || c.Role != role || c.Parent != parent || c.Title != title || c.Agent != agent {
			t.Errorf("%s: %+v, want work %s role %s parent %q title %q agent %s", id, c, work, role, parent, title, agent)
		}
	}
	// T3 runs the continued conversation; the one it continues is the same thread.
	check("ion:"+ionCont, "t3:t-ops", agents.Lead, "", "Ops review", "ion")
	check("ion:"+ionRoot, "t3:t-ops", agents.Lead, "", "Ops review", "ion")
	check("ion:"+ionChild, "t3:t-ops", agents.Child, "ion:"+ionRoot, "Security Officer", "ion")
	check("claude:"+nativeClaud, "t3:t-ops", agents.Lead, "ion:"+ionRoot, "agent title", "claude")
	check("claude:"+dispClaude, "t3:t-ops", agents.Child, "ion:"+ionRoot, "Poll check", "claude")
	check("claude:"+dispClaude+"/a1", "t3:t-ops", agents.Child, "claude:"+dispClaude, "Untitled", "claude")
	check("ion:"+ionFork, "ion:"+ionFork, agents.Lead, "", "Untitled", "ion")
	check("ion:"+ionLegacy, "ion:"+ionLegacy, agents.Lead, "", "Untitled", "ion")

	got := map[string]agents.Work{}
	for _, w := range works {
		got[w.ID] = w
	}
	if len(got) != 3 || got["t3:t-ops"].Title != "Ops review" || got["ion:"+ionFork].Project != "lab" || got["ion:"+ionLegacy].Project != "old" {
		t.Errorf("works %+v", got)
	}
}

func TestRoundShipsIonOnceAcrossRewrites(t *testing.T) {
	dir := t.TempDir()
	ion := filepath.Join(dir, "ion")
	copyTree(t, filepath.Join("testdata", "ion"), ion)
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	tally := &fakeTally{works: map[string]agents.Work{}, convs: map[string]agents.Conversation{}, buckets: map[string]agents.Bucket{}}
	srv := httptest.NewServer(tally)
	defer srv.Close()
	cfg := Config{URL: srv.URL, Token: "secret", Claude: filepath.Join(dir, "projects"), T3: filepath.Join(dir, "none.sqlite"), Ion: ion, State: filepath.Join(dir, "ship.json"), Every: time.Minute}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tally.buckets) != 6 {
		t.Fatalf("6 buckets: root 2, child, continued, fork, old file; got %d", len(tally.buckets))
	}
	T := int64(1790000000000)
	root := "ion:" + ionRoot
	if got := tally.buckets[root+" claude-opus-5-5 "+ionMinute(T+5000).Format(time.RFC3339)].Tokens; got.Output != 100 {
		t.Fatalf("root reply counted once: %+v", got)
	}

	// Ion saves the root again: a new header and one more reply.
	path := filepath.Join(ion, "conversations", ionRoot+".tree.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(raw)
	var header map[string]any
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatal(err)
	}
	header["leafId"], header["recoveryRepairVersion"] = "a6", 2
	first, _ := json.Marshal(header)
	reply := `{"id":"a6","parentId":"a5","type":"message","timestamp":1790000300000,"data":{"role":"assistant","model":"claude-opus-5-5","stopReason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}}`
	var out []byte
	out = append(append(out, first...), '\n')
	for _, l := range lines[1:] {
		out = append(append(out, l...), '\n')
	}
	out = append(append(out, reply...), '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)

	posts := tally.posts
	before := tally.buckets[root+" claude-opus-5-5 "+ionMinute(T+5000).Format(time.RFC3339)]
	if err := s.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tally.posts != posts+1 || len(tally.buckets) != 7 {
		t.Fatalf("one post with the new minute: posts %d->%d, buckets %d", posts, tally.posts, len(tally.buckets))
	}
	if tally.buckets[root+" claude-opus-5-5 "+ionMinute(T+5000).Format(time.RFC3339)] != before {
		t.Fatal("reading the whole file again counts nothing twice")
	}
	if c := tally.convs[root]; c.Agent != "ion" || c.Role != agents.Lead {
		t.Fatalf("root conversation %+v", c)
	}
	// Titles come from Ion's tabs; a continued conversation's title is
	// also the one it continues from.
	for id, want := range map[string]string{"ion:" + ionFork: "Lab experiment", "ion:" + ionLegacy: "Old notes", root: "Weekly review"} {
		if w := tally.works[id]; w.Title != want {
			t.Errorf("work %s titled %q, want %q", id, w.Title, want)
		}
	}
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
