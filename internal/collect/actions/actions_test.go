package actions

import (
	"math"
	"os"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestParseKeepsActionsOnly(t *testing.T) {
	raw, err := os.ReadFile("testdata/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	items, err := Parse("example", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("Copilot is not Actions: %+v", items)
	}
	if items[0].Minutes != 120 || items[0].Net != 0 || !near(items[0].Gross, 0.72) {
		t.Errorf("included minutes cost nothing net: %+v", items[0])
	}
	if !items[0].Date.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date = %v", items[0].Date)
	}
	if items[2].Repository != "example/web" {
		t.Errorf("a bare repository takes the account as its owner: %q", items[2].Repository)
	}
	if items[3].Minutes != 0 || !near(items[3].Net, 0.0006) {
		t.Errorf("storage is money, not minutes: %+v", items[3])
	}
}

func TestParseRefusesAnAnswerWithoutItems(t *testing.T) {
	if _, err := Parse("example", []byte(`{"message":"Not Found"}`)); err == nil {
		t.Error("an answer with no usageItems is a failed read")
	}
	if items, err := Parse("example", []byte(`{"usageItems":[]}`)); err != nil || len(items) != 0 {
		t.Errorf("a month with no use is an empty read: %v, %v", items, err)
	}
}

func TestParseAccounts(t *testing.T) {
	got, err := ParseAccounts("user:example, org:acme")
	if err != nil || len(got) != 2 || got[1] != (Account{Kind: Org, Name: "acme"}) {
		t.Errorf("got %+v, %v", got, err)
	}
	for _, bad := range []string{"", "example", "team:x", "user:"} {
		if _, err := ParseAccounts(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
