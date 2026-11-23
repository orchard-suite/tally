package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestClaudeUsageReadsTheLimitsList(t *testing.T) {
	limits, err := parseClaudeUsage(fixture(t, "claude-usage.json"), "person@example.org", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []Limit{
		{Provider: "claude", Account: "person@example.org", Plan: "max", Window: "session", Percent: 3, ResetsAt: time.Date(2026, 10, 22, 6, 19, 59, 0, time.UTC).Unix()},
		{Provider: "claude", Account: "person@example.org", Plan: "max", Window: "weekly", Percent: 61, ResetsAt: time.Date(2026, 10, 26, 20, 59, 59, 0, time.UTC).Unix()},
		{Provider: "claude", Account: "person@example.org", Plan: "max", Window: "weekly", Scope: "Fable", Percent: 88, ResetsAt: time.Date(2026, 10, 26, 20, 59, 59, 0, time.UTC).Unix()},
	}
	if len(limits) != len(want) {
		t.Fatalf("got %d limits, want %d: %+v", len(limits), len(want), limits)
	}
	for i := range want {
		if limits[i] != want[i] {
			t.Errorf("limit %d = %+v, want %+v", i, limits[i], want[i])
		}
	}
}

func TestClaudePlanNameComesFromTheProfile(t *testing.T) {
	home := t.TempDir()
	if got := claudePlanName(home); got != "" {
		t.Fatalf("no config = %q; want none", got)
	}
	cases := map[string]string{
		`{"oauthAccount":{"planDisplayName":"Ultra","seatTier":null}}`: "ultra",
		`{"oauthAccount":{"seatTier":null}}`:                           "",
		`not json`:                                                     "",
	}
	for config, want := range cases {
		os.WriteFile(filepath.Join(home, ".claude.json"), []byte(config), 0o600)
		if got := claudePlanName(home); got != want {
			t.Errorf("%s: plan = %q, want %q", config, got, want)
		}
	}
	limits, err := parseClaudeUsage(fixture(t, "claude-usage.json"), "person@example.org", "ultra")
	if err != nil || limits[0].Plan != "ultra" {
		t.Fatalf("the profile's name wins over the answer's max: %+v, %v", limits, err)
	}
}

func TestClaudeUsageFallsBackToTheNamedWindows(t *testing.T) {
	raw := `{"type":"control_response","response":{"subtype":"success","request_id":"x","response":{
		"rate_limits_available":true,
		"rate_limits":{"five_hour":{"utilization":10,"resets_at":"2026-10-22T06:00:00Z"},"seven_day":{"utilization":20,"resets_at":""},
		"model_scoped":[{"display_name":"Opus","utilization":30,"resets_at":""}],"spend":{"enabled":true,"percent":5}}}}}`
	limits, err := parseClaudeUsage([]byte(raw), "a@b.c", "pro")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range limits {
		got = append(got, l.Window+"/"+l.Scope+"/"+l.Plan)
	}
	if strings.Join(got, " ") != "session//pro weekly//pro weekly/Opus/pro spend//pro" {
		t.Errorf("got %v", got)
	}
}

func TestClaudeUsageWithNoSubscriptionIsAnError(t *testing.T) {
	// What a long-lived inference token answers: success, but no windows.
	raw := `{"type":"control_response","response":{"subtype":"success","request_id":"x","response":{"rate_limits_available":false,"rate_limits":null}}}`
	if _, err := parseClaudeUsage([]byte(raw), "a@b.c", ""); err == nil {
		t.Fatal("want an error")
	}
	refused := `{"type":"control_response","response":{"subtype":"error","request_id":"x","error":"unknown subtype"}}`
	if _, err := parseClaudeUsage([]byte(refused), "a@b.c", ""); err == nil || !strings.Contains(err.Error(), "unknown subtype") {
		t.Fatalf("got %v", err)
	}
}

func TestCodexAccountNeedsAChatGPTLogin(t *testing.T) {
	acct, found, err := parseCodexAccount(fixture(t, "codex-account.json"))
	if err != nil || !found || acct.Email != "person@example.org" || acct.Plan != "plus" {
		t.Fatalf("got %+v %v %v", acct, found, err)
	}
	_, found, err = parseCodexAccount([]byte(`{"id":2,"result":{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}}`))
	if found || err != nil {
		t.Fatalf("an API key is not a subscription: %v %v", found, err)
	}
	_, found, _ = parseCodexAccount([]byte(`{"id":2,"result":{"account":null,"requiresOpenaiAuth":true}}`))
	if found {
		t.Fatal("signed out is not found")
	}
}

func TestCodexLimitsNameWindowsByLength(t *testing.T) {
	limits, err := parseCodexLimits(fixture(t, "codex-limits.json"), codexAccount{Email: "person@example.org", Plan: "plus"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Limit{
		{Provider: "codex", Account: "person@example.org", Plan: "plus", Window: "session", Percent: 42, ResetsAt: 1795223529},
		{Provider: "codex", Account: "person@example.org", Plan: "plus", Window: "weekly", Percent: 17.5, ResetsAt: 1795800000},
		{Provider: "codex", Account: "person@example.org", Plan: "plus", Window: "session", Name: "GPT-5.5-Codex-Spark", Percent: 3},
	}
	if len(limits) != len(want) {
		t.Fatalf("got %+v", limits)
	}
	for i := range want {
		if limits[i] != want[i] {
			t.Errorf("limit %d = %+v, want %+v", i, limits[i], want[i])
		}
	}
	if windowFromMinutes(43200) != "monthly" {
		t.Error("a 30-day window is monthly")
	}
}

func TestAccountsAreFoldersByProviderAndEmail(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"claude/b@example.org", "claude/a@example.org", "codex/a@example.org", "grok/not-an-email", "cursor/a@example.org"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	got, err := accounts(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range got {
		names = append(names, a.Provider+"/"+a.Email)
	}
	if strings.Join(names, " ") != "claude/a@example.org claude/b@example.org codex/a@example.org" {
		t.Errorf("accounts = %v", names)
	}
	if _, err := account(root, "claude", "../escape"); err == nil {
		t.Error("a path is not an email")
	}
	if a, _ := account(root, "codex", " Person@Example.org "); a.Email != "person@example.org" {
		t.Errorf("email = %q, want it trimmed and lowercased", a.Email)
	}
}

func TestGrokEmailReadsOnlyTheEmail(t *testing.T) {
	raw := `{"https://auth.x.ai::client":{"key":"secret","email":"person@example.org","refresh_token":"secret"}}`
	email, err := grokEmail([]byte(raw))
	if err != nil || email != "person@example.org" {
		t.Fatalf("got %q %v", email, err)
	}
	if _, err := grokEmail([]byte(`{}`)); !errors.Is(err, errSignedOut) {
		t.Fatalf("no login is signed out: %v", err)
	}
}

func TestReasonNamesWhyAReadFailed(t *testing.T) {
	cases := map[error]string{
		errSignedOut: reasonSignedOut,
		fmt.Errorf("%w: other@example.org", errWrongAccount): reasonWrongAccount,
		errors.New("claude exited"):                          reasonReadFailed,
	}
	for err, want := range cases {
		if got := reason(err); got != want {
			t.Errorf("reason(%v) = %q, want %q", err, got, want)
		}
	}
}

type payloadBody struct {
	ResourceMetrics []struct {
		Resource struct {
			Attributes []otlpKV `json:"attributes"`
		} `json:"resource"`
		ScopeMetrics []struct {
			Metrics []otlpMetric `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
}

func attrs(p otlpPoint) map[string]string {
	m := map[string]string{}
	for _, a := range p.Attributes {
		m[a.Key] = a.Value.StringValue
	}
	return m
}

func TestPayloadReportsEveryAccount(t *testing.T) {
	ok := Account{Provider: "claude", Email: "a@example.org"}
	out := Account{Provider: "claude", Email: "b@example.org"}
	readings := []Reading{
		{Account: ok, Limits: []Limit{{Provider: "claude", Account: ok.Email, Plan: "max", Window: "weekly", Percent: 61, ResetsAt: 1795200000}}},
		{Account: out, Err: errSignedOut},
	}
	raw, points := buildPayload(readings, nil, time.Unix(1795000000, 0))
	if points != 5 {
		t.Fatalf("points = %d, want used + resets + two successes + one failure", points)
	}
	var body payloadBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	rm := body.ResourceMetrics[0]
	if rm.Resource.Attributes[0].Value.StringValue != "usage-probe" {
		t.Errorf("resource = %+v", rm.Resource.Attributes)
	}
	byName := map[string]otlpMetric{}
	var names []string
	for _, m := range rm.ScopeMetrics[0].Metrics {
		byName[m.Name] = m
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != strings.Join([]string{metricUsed, metricResets, metricSuccess, metricFailure}, ",") {
		t.Errorf("metrics = %v", names)
	}
	for _, p := range byName[metricSuccess].Gauge.DataPoints {
		a := attrs(p)
		if want := map[string]float64{ok.Email: 1, out.Email: 0}[a["account"]]; p.AsDouble != want || a["provider"] != "claude" {
			t.Errorf("success %v = %v, want %v", a, p.AsDouble, want)
		}
	}
	f := byName[metricFailure].Gauge.DataPoints
	if len(f) != 1 || attrs(f[0])["account"] != out.Email || attrs(f[0])["reason"] != reasonSignedOut {
		t.Errorf("failure = %+v", f)
	}
}

func TestPayloadWithNoAccountsIsEmpty(t *testing.T) {
	if raw, _ := buildPayload(nil, nil, time.Now()); raw != nil {
		t.Fatal("no accounts sends nothing")
	}
}

// A CLI killed at its deadline whose child still holds its output open does
// not hold the read past the deadline for long.
func TestCommandReturnsSoonAfterItsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	a := Account{Provider: "grok", Email: "a@example.org", Home: t.TempDir(), homeEnv: "GROK_HOME"}
	start := time.Now()
	_, err := a.command(ctx, "sh", "-c", "sleep 30 & sleep 30").CombinedOutput()
	if err == nil {
		t.Fatal("a command past its deadline succeeded")
	}
	if took := time.Since(start); took > 200*time.Millisecond+commandWaitDelay+time.Second {
		t.Fatalf("the read took %s past a 200ms deadline", took)
	}
}
