package agents

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func day(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }

func estateTable(t *testing.T) Table {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "estate-prices.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := ParseEstatePrices(raw)
	if err != nil {
		t.Fatal(err)
	}
	return Table{Estate: v, Public: Prices{"claude-opus-5-5": {Output: 1}, "claude-haiku-4-5": {Output: 2}}}
}

func TestEstatePricesAreDatedVersions(t *testing.T) {
	tab := estateTable(t)
	if r, _ := tab.Rate("claude-opus-5-5", day(5)); r != (Rate{Input: 5e-6, Output: 25e-6, CacheRead: 0.5e-6, CacheWrite5m: 6.25e-6, CacheWrite1h: 10e-6}) {
		t.Errorf("the version in force on Nov 5, per million made per token: %+v", r)
	}
	if r, _ := tab.Rate("us.anthropic.claude-opus-5-5", day(12).Add(13*time.Hour)); r.Input != 4e-6 || r.CacheWrite1h != 5e-6 {
		t.Errorf("the newer version from its day on, an hour write at the five-minute price when unset: %+v", r)
	}
	if r, _ := tab.Rate("claude-opus-5-5", day(1).Add(-time.Minute)); r != (Rate{Output: 1}) {
		t.Errorf("before the estate's first version the public price holds: %+v", r)
	}
	if r, ok := tab.Rate("claude-haiku-4-5-20251001", day(5)); !ok || r.Output != 2 {
		t.Errorf("a model the estate does not price takes the public price: %+v %v", r, ok)
	}
	if r, ok := tab.Rate("claude-sonnet-5-5", day(5)); !ok || r.Output != 15e-6 {
		t.Errorf("the estate's model names are made bare: %+v %v", r, ok)
	}
	if _, ok := tab.Rate("mystery-1", day(5)); ok {
		t.Error("a model nobody prices has no price")
	}
	for _, bad := range []string{
		"models:\n  m:\n    - from: 2026-11-01\n      input: 1\n      output: 1\n      cache_read: 1\n",
		"models:\n  m:\n    - from: November\n      input: 1\n      output: 1\n      cache_read: 1\n      cache_write: 1\n",
		"models:\n  m:\n    - from: 2026-11-01\n      input: -1\n      output: 1\n      cache_read: 1\n      cache_write: 1\n",
		"models:\n  m:\n    - {from: 2026-11-01, input: 1, output: 1, cache_read: 1, cache_write: 1}\n    - {from: 2026-11-01, input: 2, output: 1, cache_read: 1, cache_write: 1}\n",
		"model:\n  m: []\n",
	} {
		if _, err := ParseEstatePrices([]byte(bad)); err == nil {
			t.Errorf("refused nothing:\n%s", bad)
		}
	}
}

func TestPriceTakesEachDaysPrice(t *testing.T) {
	tab := estateTable(t)
	sums := []Sum{
		{Model: "claude-opus-5-5", Day: day(9), Tokens: Tokens{Output: 1_000_000}},
		{Model: "claude-opus-5-5", Day: day(10), Tokens: Tokens{Output: 1_000_000}},
	}
	if got := Price(sums, tab); !near(got, 25+20) {
		t.Fatalf("a price change mid-month leaves the days before it alone: %v", got)
	}
}

const run = "3f0c9a52-1d7e-4b8a-9c11-2b6f8e4d0a71"

func TestRunOf(t *testing.T) {
	for conv, want := range map[string]string{
		"claude:" + run:               run,
		"claude:" + run + "/agent-a1": run,
		"ion:conv-1":                  "conv-1",
		Bare("claude:" + run):         "",
	} {
		if got := RunOf(conv); got != want {
			t.Errorf("RunOf(%q) = %q, want %q", conv, got, want)
		}
	}
}

func TestPriceRunAddsItsSubagents(t *testing.T) {
	tab := Prices{"claude-opus-5-5": {Output: 1}}.Table()
	at := time.Date(2026, 11, 20, 15, 5, 0, 0, time.UTC)
	rows := []Row{
		{Conversation: "claude:" + run, Model: "claude-opus-5-5", Minute: at, Tokens: Tokens{Output: 3, Input: 10}},
		{Conversation: "claude:" + run + "/agent-a1", Model: "claude-opus-5-5", Minute: at.Add(4 * time.Minute), Tokens: Tokens{Output: 2}},
		{Conversation: "claude:" + run + "x", Model: "claude-opus-5-5", Minute: at, Tokens: Tokens{Output: 100}},
	}
	c := PriceRun(run, rows, tab)
	if !near(c.Amount, 5) || c.Tokens != 15 || c.Conversations != 2 || c.Currency != "USD" || !c.Through.Equal(at.Add(5*time.Minute)) {
		t.Fatalf("a run is its conversation and its subagents', no other run's: %+v", c)
	}
}

func TestBuildSpendRollsUpThroughItems(t *testing.T) {
	tab := Prices{"claude-opus-5-5": {Output: 1}}.Table()
	at := func(m int) time.Time { return time.Date(2026, 11, 20, 15, m, 0, 0, time.UTC) }
	row := func(conv string, m int, out int64) Row {
		return Row{Machine: "m1", Conversation: conv, Model: "claude-opus-5-5", Minute: at(m), Tokens: Tokens{Output: out}}
	}
	other := "9d1e2f30-0000-4000-8000-000000000002"
	links := Links{
		Claims: []Claim{
			{Run: run, Item: "ACME-9", Agent: "claude-code on laptop-1", At: at(30).Add(10 * time.Second)},
			{Run: run, Item: "ACME-7", Agent: "claude-code on laptop-1", At: at(4).Add(30 * time.Second)},
			{Run: other, Item: "ACME-3", Agent: "claude-code on laptop-2", At: at(0)},
		},
		Scopes: map[string]string{"ACME-7": "billing", "ACME-9": "payments", "ACME-3": "billing"},
		Tree: []ScopeNode{
			{ID: "acme", Name: "Acme", Kind: "organization"},
			{ID: "finance", Name: "Finance", Kind: "department", Parent: "acme"},
			{ID: "billing", Name: "Billing", Kind: "program", Parent: "finance"},
			{ID: "payments", Name: "Payments", Kind: "program", Parent: "finance"},
		},
	}
	rows := []Row{
		row("claude:"+run, 1, 1),              // before any claim: its first item, ACME-7
		row("claude:"+run, 4, 2),              // the minute of the claim: ACME-7
		row("claude:"+run+"/agent-a1", 20, 4), // a subagent's minute: ACME-7
		row("claude:"+run, 30, 8),             // the minute the second claim came: ACME-9
		row("claude:"+other, 2, 16),           // another run, on ACME-3
		row("claude:lonely", 2, 32),           // a run that claimed nothing
	}
	rep := BuildSpend(rows, links, tab, at(0), at(60))
	if rep.Total.Cost != 63 || rep.Total.Runs != 3 || rep.Unlinked.Cost != 32 || rep.Unlinked.Runs != 1 {
		t.Fatalf("total %+v unlinked %+v", rep.Total, rep.Unlinked)
	}
	items := map[string]ItemSpend{}
	for _, i := range rep.Items {
		items[i.Item] = i
	}
	if i := items["ACME-7"]; i.Cost != 7 || i.Runs != 1 || i.Scope != "billing" {
		t.Errorf("ACME-7 %+v", i)
	}
	if i := items["ACME-9"]; i.Cost != 8 || i.Runs != 1 || i.Scope != "payments" {
		t.Errorf("ACME-9 %+v", i)
	}
	scopes := map[string]ScopeSpend{}
	for _, s := range rep.Scopes {
		scopes[s.Scope] = s
	}
	if s := scopes["billing"]; s.Own.Cost != 23 || s.Own.Runs != 2 || s.Total.Cost != 23 {
		t.Errorf("a program is its items: %+v", s)
	}
	if s := scopes["finance"]; s.Own.Cost != 0 || s.Total.Cost != 31 || s.Total.Runs != 2 || s.Kind != "department" {
		t.Errorf("a department is every program under it, a run counted once: %+v", s)
	}
	if rep.Scopes[0].Scope != "acme" || rep.Scopes[0].Total.Cost != 31 {
		t.Errorf("the top of the tree first: %+v", rep.Scopes[0])
	}
	if len(rep.Agents) != 2 || rep.Agents[0].Agent != "claude-code on laptop-2" || rep.Agents[0].Cost != 16 {
		t.Errorf("agents by cost: %+v", rep.Agents)
	}
	lim := rep.Limit(false)
	if lim.ShowsHosts || len(lim.Agents) != 1 || lim.Agents[0] != (AgentSpend{Agent: "claude-code", SpendLine: SpendLine{Cost: 31, Tokens: 31, Runs: 2}}) {
		t.Errorf("without hosts an agent is its name: %+v", lim.Agents)
	}
}

func TestStoreKeepsLinksAndARunsRows(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 11, 20, 15, 5, 0, 0, time.UTC)
	b := Batch{
		Conversations: []Conversation{{ID: "claude:" + run, Work: "w", Agent: "claude", Role: Lead}, {ID: "claude:" + run + "/agent-a1", Work: "w", Agent: "claude", Role: Child, Parent: "claude:" + run}},
		Buckets: []Bucket{
			{Conversation: "claude:" + run, Model: "claude-opus-5-5", Minute: at, Tokens: Tokens{Output: 3}},
			{Conversation: "claude:" + run + "/agent-a1", Model: "claude-opus-5-5", Minute: at, Tokens: Tokens{Output: 2}},
			{Conversation: "claude:other_run", Model: "claude-opus-5-5", Minute: at, Tokens: Tokens{Output: 9}},
		},
	}
	if err := s.Apply(ctx, "m1", b, at); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, "m2", Batch{Buckets: []Bucket{{Conversation: "claude:" + run, Model: "claude-opus-5-5", Minute: at, Tokens: Tokens{Output: 50}}}}, at); err != nil {
		t.Fatal(err)
	}
	rows, err := s.RunRows(ctx, "m1", run)
	if err != nil {
		t.Fatal(err)
	}
	if c := PriceRun(run, rows, Prices{"claude-opus-5-5": {Output: 1}}.Table()); c.Amount != 5 || c.Conversations != 2 {
		t.Fatalf("a machine's run is its own conversations: %+v", c)
	}
	if rows, _ := s.RunRows(ctx, "m1", "other%"); len(rows) != 0 {
		t.Fatalf("a run id is matched as it is, never as a pattern: %+v", rows)
	}

	if p, _ := s.FeedPosition(ctx); p != 0 {
		t.Fatalf("an empty feed starts at zero: %d", p)
	}
	claim := Claim{Run: run, Item: "ACME-7", Agent: "claude-code on laptop-1", At: at}
	if err := s.TakeFeed(ctx, []Claim{claim, claim}, nil, 12); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.FeedPosition(ctx); p != 12 {
		t.Fatalf("position %d", p)
	}
	items, _ := s.UnscopedItems(ctx, at, 10)
	if len(items) != 1 || items[0] != "ACME-7" {
		t.Fatalf("a claimed item with no scope read: %v", items)
	}
	if err := s.SetItemScope(ctx, "ACME-7", "billing", at); err != nil {
		t.Fatal(err)
	}
	if items, _ := s.UnscopedItems(ctx, at.Add(-time.Hour), 10); len(items) != 0 {
		t.Fatalf("a scope read lately is not read again: %v", items)
	}
	if err := s.TakeFeed(ctx, nil, []string{"ACME-7"}, 13); err != nil {
		t.Fatal(err)
	}
	if items, _ := s.UnscopedItems(ctx, at.Add(-time.Hour), 10); len(items) != 1 {
		t.Fatalf("an item scoped again is read again: %v", items)
	}
	if err := s.SetScopes(ctx, []ScopeNode{{ID: "billing", Name: "Billing", Kind: "program", Parent: "finance"}}); err != nil {
		t.Fatal(err)
	}
	l, err := s.Links(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Claims) != 1 || l.Claims[0] != claim || l.Scopes["ACME-7"] != "billing" || len(l.Tree) != 1 || l.Tree[0].Parent != "finance" {
		t.Fatalf("links %+v", l)
	}
}
