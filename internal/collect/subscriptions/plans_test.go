package subscriptions

import (
	"errors"
	"testing"
)

func TestPlansFollowTheReadingsAndKeepTheLastKnown(t *testing.T) {
	latestPlans.Store(nil)
	notePlans([]Reading{
		{Account: Account{Provider: "claude", Email: "Second@Example.org"}, Limits: []Limit{{Plan: "max"}}},
		{Account: Account{Provider: "codex", Email: "a@example.org"}, Limits: []Limit{{Plan: "plus"}}},
		{Account: Account{Provider: "grok", Email: "a@example.org"}},
	})
	got := Plans()
	if len(got) != 3 || got[0].Provider != "claude" || got[0].Account != "second@example.org" || got[0].Plan != "max" || got[1].Plan != "plus" || got[2].Plan != "" {
		t.Fatalf("plans = %+v", got)
	}
	// A failed read keeps the plan the account last reported.
	notePlans([]Reading{{Account: Account{Provider: "claude", Email: "second@example.org"}, Err: errors.New("signed out")}})
	got = Plans()
	if len(got) != 1 || got[0].Plan != "max" {
		t.Errorf("after a failed read = %+v", got)
	}
}
