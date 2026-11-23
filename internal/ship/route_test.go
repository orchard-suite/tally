package ship

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
)

func TestIonProvidersReadsBaseURLsOnly(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "ion", "engine.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := IonProviders(b)
	if len(got) != 2 || got["acme-gateway"] != "https://gateway.example.com" || got["elsewhere"] != "https://api.example.org" {
		t.Fatalf("providers %v", got)
	}
	if len(IonProviders([]byte("not json"))) != 0 {
		t.Fatal("an unreadable engine.json is no provider")
	}
}

func TestRouteAllowsOnlyTheGatewaysProviders(t *testing.T) {
	r, err := Through("gateway.example.com", filepath.Join("testdata", "ion", "engine.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]bool{
		"acme-gateway/claude-sonnet-5-5": true,
		"elsewhere/claude-sonnet-5-5":    false,
		"anthropic/claude-opus-5-5":      false,
		"claude-opus-5-5":                false,
		"":                               false,
	} {
		if r.Allows(model) != want {
			t.Errorf("%q allowed %v, want %v", model, !want, want)
		}
	}
	open, _ := Through("", "", "")
	if !open.Allows("claude-opus-5-5") {
		t.Fatal("no route allows every reply")
	}
	none, err := Through("gateway.example.com", filepath.Join(t.TempDir(), "engine.json"), "")
	if err != nil || none.Allows("acme-gateway/claude-sonnet-5-5") {
		t.Fatalf("without engine.json no provider is on the route: %v", err)
	}
}

func TestRouteAllowsClaudeCodeWhenItUsesTheGateway(t *testing.T) {
	r, err := Through("gateway.example.com", filepath.Join("testdata", "ion", "engine.json"), "https://Gateway.example.com/anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Allows("claude-opus-5-5") || !r.Allows("acme-gateway/claude-sonnet-5-5") || r.Allows("elsewhere/claude-sonnet-5-5") || r.Allows("") {
		t.Fatal("Claude Code's bare models go through the gateway it is pointed at, and the providers keep their rule")
	}
	off, _ := Through("gateway.example.com", "", "https://api.anthropic.com")
	if off.Allows("claude-opus-5-5") {
		t.Fatal("Claude Code pointed elsewhere stays on the machine")
	}
}

func TestClaudeBaseURLTakesTheFirstSettingThatSetsIt(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	managed := write("managed.json", `{"env":{"ANTHROPIC_BASE_URL":"https://gateway.example.com"}}`)
	empty := write("local.json", `{"env":{"OTHER":"x"}}`)
	own := write("settings.json", `{"env":{"ANTHROPIC_BASE_URL":"https://own.example.com"}}`)
	missing := filepath.Join(dir, "missing.json")
	if got := ClaudeBaseURL([]string{missing, managed, empty, own}, "https://env.example.com"); got != "https://gateway.example.com" {
		t.Errorf("a managed setting wins: %q", got)
	}
	if got := ClaudeBaseURL([]string{missing, empty, own}, "https://env.example.com"); got != "https://own.example.com" {
		t.Errorf("a person's setting wins over the environment: %q", got)
	}
	if got := ClaudeBaseURL([]string{missing, empty}, "https://env.example.com"); got != "https://env.example.com" {
		t.Errorf("the environment is the fallback: %q", got)
	}
}

// A shipper on a route posts only what the gateway served: the Claude Code
// session and Ion's bare-model replies ran on the machine's own
// subscription and never leave it, not even their titles.
func TestRoundOnARoutePostsOnlyGatewayWork(t *testing.T) {
	dir := t.TempDir()
	ion := filepath.Join(dir, "ion")
	copyTree(t, filepath.Join("testdata", "ion"), ion)
	projects := filepath.Join(dir, "projects")
	copyTree(t, filepath.Join("testdata", "projects"), projects)
	tally := &fakeTally{works: map[string]agents.Work{}, convs: map[string]agents.Conversation{}, buckets: map[string]agents.Bucket{}}
	srv := httptest.NewServer(tally)
	defer srv.Close()
	cfg := Config{URL: srv.URL, Token: "secret", Claude: projects, T3: filepath.Join(dir, "none.sqlite"), Ion: ion, State: filepath.Join(dir, "ship.json"), Every: time.Minute, Route: "gateway.example.com"}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	root := "ion:" + ionRoot
	if len(tally.buckets) != 1 {
		t.Fatalf("one bucket, the gateway's: %v", keys(tally.buckets))
	}
	for k, b := range tally.buckets {
		if b.Conversation != root || !strings.HasPrefix(b.Model, "acme-gateway/") {
			t.Fatalf("bucket %s is off the route", k)
		}
	}
	if len(tally.convs) != 1 || tally.convs[root].ID != root {
		t.Fatalf("only the conversation with a gateway reply: %v", keys(tally.convs))
	}
	if len(tally.works) != 1 || tally.works[root].ID != root {
		t.Fatalf("only that conversation's work: %v", keys(tally.works))
	}

	// The place it saved remembers the conversation is on the route.
	again, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if !again.known[root].Routed || again.known["claude:"+session].Routed {
		t.Fatal("routed is kept per conversation")
	}
}

func TestResetWithdrawsAndForgets(t *testing.T) {
	dir := t.TempDir()
	var method, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, auth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	state := filepath.Join(dir, "ship.json")
	if err := os.WriteFile(state, []byte(`{"sent":{"c:x":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{URL: srv.URL, Token: "secret", State: state}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if method != "DELETE /v1/usage" || auth != "Bearer secret" {
		t.Fatalf("asked %q with %q", method, auth)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) || len(s.sent) != 0 {
		t.Fatal("the place is forgotten")
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestClaudeBaseURLReadsAManagedPlist(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil is macOS's")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "p.json")
	if err := os.WriteFile(src, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://gateway.example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(dir, "com.anthropic.claudecode.plist")
	if out, err := exec.Command("plutil", "-convert", "binary1", "-o", plist, src).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := ClaudeBaseURL([]string{plist}, ""); got != "https://gateway.example.com" {
		t.Errorf("a binary preference profile is read: %q", got)
	}
}
