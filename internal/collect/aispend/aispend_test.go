package aispend

import (
	"os"
	"testing"

	"github.com/orchard-suite/tally/internal/collect/subscriptions"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParsePrices(t *testing.T) {
	got, err := ParsePrices("claude:Person@Example.org=200, codex:other@example.org=20")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Provider != "claude" || got[0].Account != "person@example.org" || got[0].CostPerMonth != 200 || got[1].CostPerMonth != 20 {
		t.Errorf("prices = %+v", got)
	}
	for _, bad := range []string{"claude=200", "claude:a@b.c=lots", ":x=1"} {
		if _, err := ParsePrices(bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
	if got, err := ParsePrices(""); err != nil || len(got) != 0 {
		t.Errorf("empty = %+v, %v", got, err)
	}
}

func TestOpenAIUsageSumsBuckets(t *testing.T) {
	tokens, next, err := ParseOpenAIUsage(fixture(t, "openai-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if tokens["input"] != 2000 || tokens["output"] != 500 || next != "" {
		t.Errorf("tokens = %v, next %q", tokens, next)
	}
}

func TestAnthropicUsageCountsEveryInputKind(t *testing.T) {
	tokens, next, err := ParseAnthropicUsage(fixture(t, "anthropic-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if tokens["input"] != 5200 || tokens["output"] != 700 || next != "page_abc" {
		t.Errorf("tokens = %v, next %q", tokens, next)
	}
}

func TestMergeJoinsPricesAndTokens(t *testing.T) {
	priced := []Account{{Provider: "openai", Account: "api", CostPerMonth: 50}, {Provider: "claude", Account: "a@example.org", CostPerMonth: 200}}
	usage := []Account{{Provider: "openai", Account: "api", Tokens: map[string]int64{"input": 1}}, {Provider: "anthropic", Account: "api", Tokens: map[string]int64{"output": 2}}}
	got := Merge(priced, usage)
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Provider != "anthropic" || got[0].CostPerMonth != 0 || got[0].Tokens["output"] != 2 {
		t.Errorf("unpriced usage account: %+v", got[0])
	}
	if got[1].Provider != "claude" || got[1].Tokens != nil {
		t.Errorf("priced only: %+v", got[1])
	}
	if got[2].Provider != "openai" || got[2].CostPerMonth != 50 || got[2].Tokens["input"] != 1 {
		t.Errorf("both: %+v", got[2])
	}
}

func TestPlanPricesPriceEverySignedInAccountOnThePlan(t *testing.T) {
	prices, err := ParsePlanPrices("claude:Max=200, codex:plus=20")
	if err != nil {
		t.Fatal(err)
	}
	if len(prices) != 2 || prices[0].Plan != "max" || prices[0].CostPerMonth != 200 {
		t.Fatalf("prices = %+v", prices)
	}
	plans := []subscriptions.Plan{
		{Provider: "claude", Account: "a@example.org", Plan: "max"},
		{Provider: "claude", Account: "b@example.org", Plan: "max"},
		{Provider: "codex", Account: "a@example.org", Plan: "free"},
		{Provider: "grok", Account: "a@example.org", Plan: ""},
	}
	named := []Account{{Provider: "claude", Account: "b@example.org", CostPerMonth: 100}}
	got := PriceByPlan(named, plans, prices)
	if len(got) != 4 {
		t.Fatalf("accounts = %+v", got)
	}
	if got[0].Account != "b@example.org" || got[0].CostPerMonth != 100 || got[0].Plan != "" {
		t.Errorf("a price by name wins: %+v", got[0])
	}
	if got[1].Account != "a@example.org" || got[1].Plan != "max" || got[1].CostPerMonth != 200 {
		t.Errorf("priced by plan: %+v", got[1])
	}
	if got[2].Provider != "codex" || got[2].Plan != "free" || got[2].CostPerMonth != 0 {
		t.Errorf("a plan with no price is listed unpriced: %+v", got[2])
	}
	for _, bad := range []string{"claude=200", "claude:max=lots"} {
		if _, err := ParsePlanPrices(bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}
