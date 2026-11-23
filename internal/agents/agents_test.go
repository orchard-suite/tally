package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/collect"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestModelIsTheBareName(t *testing.T) {
	for raw, want := range map[string]string{
		"claude-opus-5-5":                          "claude-opus-5-5",
		"us.anthropic.claude-opus-5-5":             "claude-opus-5-5",
		"anthropic/claude-opus-5-5":                "claude-opus-5-5",
		"openrouter/anthropic/claude-sonnet-5-5":   "claude-sonnet-5-5",
		"claude-haiku-4-5-20251001":                "claude-haiku-4-5",
		"anthropic.claude-haiku-4-5-20251001-v1:0": "claude-haiku-4-5",
		"vertex_ai/claude-haiku-4-5@20251001":      "claude-haiku-4-5",
		"gpt-4o-2024-08-06":                        "gpt-4o",
		"Claude-Fable-5-1":                         "claude-fable-5-1",
		"global.anthropic.claude-fable-5-1":        "claude-fable-5-1",
	} {
		if got := Model(raw); got != want {
			t.Errorf("Model(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParsePricesPrefersTheBareEntry(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "prices.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePrices(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r := p["claude-opus-5-5"]; r != (Rate{Input: 4e-06, CacheWrite5m: 5e-06, CacheWrite1h: 8e-06, CacheRead: 2e-07, Output: 2e-05}) {
		t.Errorf("opus %+v: the bare entry wins over a region's", r)
	}
	if r := p["claude-sonnet-5-5"]; r != (Rate{Input: 2e-06, CacheWrite5m: 2e-06, CacheWrite1h: 2e-06, CacheRead: 2e-06, Output: 1e-05}) {
		t.Errorf("sonnet %+v: first route by name, missing cache prices at the input price", r)
	}
	if r, ok := p.Rate("claude-haiku-4-5-20251001"); !ok || r.CacheWrite1h != 1.25e-06 {
		t.Errorf("haiku %+v %v", r, ok)
	}
	if _, ok := p["text-embedding-3-small"]; ok {
		t.Error("a model with no output price is not an agent model")
	}
}

func TestBuildReport(t *testing.T) {
	loc := time.FixedZone("PDT", -7*3600)
	from := time.Date(2026, 11, 4, 0, 0, 0, 0, loc)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 4, h, m, 0, 0, loc).UTC() }
	prices := Prices{"claude-opus-5-5": {Input: 1e-6, CacheWrite5m: 2e-6, CacheWrite1h: 3e-6, CacheRead: 1e-7, Output: 1e-5}}
	row := func(conv, role, work string, m time.Time, model string, tok Tokens) Row {
		return Row{Machine: "m1", Conversation: conv, Role: role, Work: work, WorkTitle: "Title " + work, Project: "proj", Model: model, Minute: m, Tokens: tok}
	}
	one := Tokens{Requests: 1, Input: 100, CacheWrite1h: 1000, CacheRead: 10000, Output: 10}
	rows := []Row{
		// Work A: a lead active 09:00-09:02 and again at 09:10, a child
		// at 09:01 alongside it.
		row("lead-a", Lead, "a", at(9, 0), "claude-opus-5-5", one),
		row("lead-a", Lead, "a", at(9, 1), "claude-opus-5-5", one),
		row("lead-a", Lead, "a", at(9, 4), "claude-opus-5-5", one),
		row("lead-a", Lead, "a", at(9, 10), "claude-opus-5-5", one),
		row("kid-a", Child, "a", at(9, 1), "us.anthropic.claude-opus-5-5", one),
		// Work B: one minute of a model with no price.
		row("lead-b", Lead, "b", at(8, 0), "mystery-1", Tokens{Requests: 1, Output: 5}),
	}
	rep := BuildReport(rows, prices, from, from.AddDate(0, 0, 1), at(9, 13))

	cost := one.Input*1 + one.CacheWrite1h*3 + one.CacheRead/10 + one.Output*10
	perRow := float64(cost) * 1e-6
	if !near(rep.Totals.Cost, 5*perRow) {
		t.Errorf("cost %v want %v", rep.Totals.Cost, 5*perRow)
	}
	if rep.Totals.Total != 5*one.Total()+5 {
		t.Errorf("total %d", rep.Totals.Total)
	}
	// Six conversation-minutes, five distinct minutes.
	if !near(rep.Totals.AgentHours, 6.0/60) || !near(rep.Totals.ActiveHours, 5.0/60) {
		t.Errorf("hours %v %v", rep.Totals.AgentHours, rep.Totals.ActiveHours)
	}
	if rep.Children.Conversations != 1 || !near(rep.Children.Cost, perRow) {
		t.Errorf("children %+v", rep.Children)
	}
	if len(rep.Works) != 2 || rep.Works[0].ID != "b" || rep.Works[1].ID != "a" {
		t.Fatalf("works by first activity: %+v", rep.Works)
	}
	a := rep.Works[1]
	if a.Children != 1 || !a.Ongoing || !near(a.AgentHours, 5.0/60) || !a.First.Equal(at(9, 0)) || !a.Last.Equal(at(9, 11)) {
		t.Errorf("work a %+v", a)
	}
	// 09:00, 09:01, 09:04 join across a two-minute quiet; 09:10 is apart.
	if want := []Span{{540, 545}, {550, 551}}; len(a.Lead) != 2 || a.Lead[0] != want[0] || a.Lead[1] != want[1] {
		t.Errorf("lead spans %+v", a.Lead)
	}
	if want := (Span{541, 542}); len(a.Child) != 1 || a.Child[0] != want {
		t.Errorf("child spans %+v", a.Child)
	}
	if rep.Works[0].Ongoing {
		t.Error("work b ended an hour ago")
	}
	if len(rep.Models) != 2 || rep.Models[0].Model != "claude-opus-5-5" || !near(rep.Models[0].Share, 1) || rep.Models[1].Priced {
		t.Errorf("models %+v: one opus row across two routes", rep.Models)
	}
	if len(rep.Unpriced) != 1 || rep.Unpriced[0] != "mystery-1" {
		t.Errorf("unpriced %v", rep.Unpriced)
	}
	if m := rep.Machines; len(m) != 1 || m[0].Machine != "m1" || m[0].Works != 2 || !near(m[0].AgentHours, 6.0/60) || !near(m[0].Cost, rep.Totals.Cost) {
		t.Errorf("machines %+v", rep.Machines)
	}
	if l := rep.Limit(true, false); l.ShowsWorks || !l.ShowsUsers || len(l.Works) != 0 || l.Children != (ChildLine{}) || len(l.Machines) != 1 || l.Totals != rep.Totals {
		t.Errorf("by user keeps totals and machines, and drops the works and children: %+v", l)
	}
	if l := rep.Limit(false, false); l.ShowsUsers || len(l.Machines) != 0 || len(l.Models) != len(rep.Models) || l.Totals != rep.Totals {
		t.Errorf("totals alone keep the models and drop the machines: %+v", l)
	}
	if !rep.ShowsUsers || !rep.ShowsWorks || len(rep.Works) != 2 {
		t.Error("Limit leaves the report whole")
	}
}

func testCollector(t *testing.T, token string) *Collector {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	inst, err := collect.NewInstruments()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	machines, err := ParseTokens("laptop=" + hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	c := &Collector{store: store, log: slog.New(slog.NewTextHandler(io.Discard, nil)), inst: inst, machines: machines}
	if err := c.register(); err != nil {
		t.Fatal(err)
	}
	return c
}

const batch = `{"works":[{"id":"t3:w1","title":"Polish","project":"tally"}],
"conversations":[{"id":"claude:s1","work":"t3:w1","agent":"claude","role":"lead","title":"Polish"},
                 {"id":"claude:s1/a1","work":"t3:w1","parent":"claude:s1","agent":"claude","role":"child","title":"Look"}],
"buckets":[{"conversation":"claude:s1","model":"claude-opus-5-5","minute":"2026-11-04T16:00:00Z","requests":2,"input":10,"cache_write_5m":0,"cache_write_1h":100,"cache_read":1000,"output":5},
           {"conversation":"claude:s1/a1","model":"claude-opus-5-5","minute":"2026-11-04T16:01:00Z","requests":1,"input":1,"cache_write_5m":0,"cache_write_1h":0,"cache_read":0,"output":1}]}`

func post(c *Collector, auth, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/usage", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	c.Ingest(rec, req)
	return rec.Code
}

func TestIngestTakesABatchOnceUnderTheTokensMachine(t *testing.T) {
	c := testCollector(t, "secret")
	if code := post(c, "", batch); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code := post(c, "Bearer wrong", batch); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	if code := post(c, "Bearer secret", `{"buckets":[{"conversation":"x","model":"m","minute":"2026-11-04T16:00:30Z"}]}`); code != http.StatusBadRequest {
		t.Fatalf("a minute that is not whole: %d", code)
	}
	for range 2 {
		if code := post(c, "Bearer secret", batch); code != http.StatusNoContent {
			t.Fatalf("good batch: %d", code)
		}
	}
	from := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	rows, err := c.store.Rows(context.Background(), from, from.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("a batch sent twice stores once: %d rows", len(rows))
	}
	for _, r := range rows {
		if r.Machine != "laptop" || r.Work != "t3:w1" || r.WorkTitle != "Polish" || r.Project != "tally" {
			t.Errorf("row %+v", r)
		}
	}
	sums, err := c.store.Sums(context.Background(), from, from.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 2 {
		t.Fatalf("sums by role %+v", sums)
	}
	posts, err := c.store.LastPosts(context.Background())
	if err != nil || posts["laptop"].IsZero() {
		t.Fatalf("last post %v %v", posts, err)
	}
}

func TestWithdrawDeletesOnlyTheTokensMachine(t *testing.T) {
	c := testCollector(t, "secret")
	ctx := context.Background()
	if err := c.store.Apply(ctx, "desk", Batch{Buckets: []Bucket{{Conversation: "ion:d", Model: "m", Minute: time.Date(2026, 11, 4, 16, 0, 0, 0, time.UTC), Tokens: Tokens{Output: 1}}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := post(c, "Bearer secret", batch); code != http.StatusNoContent {
		t.Fatalf("good batch: %d", code)
	}
	withdraw := func(auth string) int {
		req := httptest.NewRequest(http.MethodDelete, "/v1/usage", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		c.Withdraw(rec, req)
		return rec.Code
	}
	if code := withdraw("Bearer wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	if code := withdraw("Bearer secret"); code != http.StatusNoContent {
		t.Fatalf("withdraw: %d", code)
	}
	from := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	rows, err := c.store.Rows(ctx, from, from.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Machine != "desk" {
		t.Fatalf("only the other machine's work is left: %+v", rows)
	}
	var left int
	if err := c.store.db.QueryRow(`SELECT (SELECT count(*) FROM works WHERE machine = 'laptop') + (SELECT count(*) FROM conversations WHERE machine = 'laptop') + (SELECT count(*) FROM machines WHERE machine = 'laptop')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("laptop's works, conversations, and last post are gone: %d %v", left, err)
	}
}

func TestParseTokensRefusesAPlainToken(t *testing.T) {
	if _, err := ParseTokens("laptop=secret"); err == nil {
		t.Fatal("SHIP_TOKENS holds hashes, never tokens")
	}
	m, err := ParseTokens(" ")
	if err != nil || len(m) != 0 {
		t.Fatal("empty is no machines")
	}
}

func TestValidate(t *testing.T) {
	ok := Batch{Buckets: []Bucket{{Conversation: "c", Model: "m", Minute: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Batch{
		{},
		{Conversations: []Conversation{{ID: "c", Work: "w", Agent: "claude", Role: "boss"}}},
		{Conversations: []Conversation{{ID: "c", Agent: "claude", Role: Lead}}},
		{Buckets: []Bucket{{Conversation: "c", Model: "m", Minute: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Tokens: Tokens{Output: -1}}}},
		{Works: []Work{{ID: strings.Repeat("x", 201)}}},
	}
	for i, b := range bad {
		if b.Validate() == nil {
			t.Errorf("batch %d passed", i)
		}
	}
}

func TestBuildDays(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	// Daylight saving ends on Nov 1 2026: that day is 25 hours long.
	from := time.Date(2026, 10, 31, 0, 0, 0, 0, loc)
	to := time.Date(2026, 11, 3, 0, 0, 0, 0, loc)
	prices := Prices{"claude-opus-5-5": {Output: 1}}
	at := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, loc).UTC() }
	row := func(conv, work string, m time.Time, model string, out int64) Row {
		return Row{Machine: "m1", Conversation: conv, Work: work, Model: model, Minute: m, Tokens: Tokens{Requests: 1, Output: out}}
	}
	rows := []Row{
		row("c1", "w1", at(1, 23, 30), "claude-opus-5-5", 2),
		row("c1", "w1", at(1, 23, 30), "mystery-1", 5),
		row("c2", "w1", at(1, 23, 30), "claude-opus-5-5", 1),
		row("c3", "w2", at(2, 0, 10), "us.anthropic.claude-opus-5-5", 4),
		// Before the span: not counted.
		row("c4", "w3", from.Add(-time.Minute), "claude-opus-5-5", 9),
	}
	days := BuildDays(rows, prices, from, to, loc)
	if len(days) != 3 || days[0].Day != "2026-10-31" || days[2].Day != "2026-11-02" {
		t.Fatalf("one entry a day, empty days too: %+v", days)
	}
	if days[0] != (Day{Day: "2026-10-31"}) {
		t.Errorf("an empty day %+v", days[0])
	}
	// Late on the long day is still that day; two conversations in one
	// minute are two agent minutes; the model with no price adds tokens only.
	if d := days[1]; d.Total != 8 || !near(d.Cost, 3) || !near(d.AgentHours, 2.0/60) || d.Works != 1 {
		t.Errorf("Nov 1 %+v", d)
	}
	if d := days[2]; d.Total != 4 || !near(d.Cost, 4) || d.Works != 1 {
		t.Errorf("Nov 2 %+v", d)
	}
}

func TestPriceAndSpend(t *testing.T) {
	prices := Prices{"claude-opus-5-5": {Output: 1}}
	sums := []Sum{{Model: "us.anthropic.claude-opus-5-5", Tokens: Tokens{Output: 3}}, {Model: "mystery-1", Tokens: Tokens{Output: 9}}}
	if got := Price(sums, prices); !near(got, 3) {
		t.Fatalf("price %v: bare names priced, unpriced models add nothing", got)
	}
	c := &Collector{billing: Subscription, zone: time.UTC, prices: prices, snap: snapshot{spend: Spend{Trailing: 1}}}
	if c.Spend() != nil {
		t.Fatal("work a plan pays for is not spend")
	}
	c.billing = API
	if s := c.Spend(); s == nil || s.Trailing != 1 {
		t.Fatalf("work billed by the token is spend: %v", s)
	}
	c.prices = nil
	if c.Spend() != nil {
		t.Fatal("before the prices are read there is no spend, never a zero")
	}
}
