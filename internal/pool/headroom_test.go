package pool

import (
	"testing"
	"time"
)

func headroomPools() []Pool {
	refill := time.Date(2026, 11, 28, 15, 0, 0, 0, time.UTC)
	return []Pool{
		{Provider: "claude", Window: "weekly", Accounts: 4, Capacity: 400, Remaining: 320, Burn: 1.5, NextReset: &Refill{At: refill, Gain: 100}},
		{Provider: "claude", Scope: "Fable", Window: "weekly", Accounts: 3, Capacity: 300, Remaining: 30},
		{Provider: "codex", Window: "weekly", Accounts: 1, Capacity: 100, Remaining: 5},
	}
}

func TestHeadroomCountsOnlyWhatTheModelsDrawOn(t *testing.T) {
	rot := Rotation{On: true, Active: "build@example.com", Limits: []Limit{
		{Window: "weekly", Percent: 94},
		{Window: "weekly", Scope: "Fable", Percent: 99},
	}}
	h := Machine("claude", []string{"opus", "sonnet"}, headroomPools(), rot)
	if !h.Usable || !h.Rotating {
		t.Fatalf("an account can run: %+v", h)
	}
	// The Fable pool is nearly spent, but these agents never run Fable,
	// and the account in use at 94% is the rotation's to step past.
	if h.Used != 0.2 || h.Binding != "20% of the Claude weekly pool used (4 accounts)" {
		t.Fatalf("the account-wide pool binds: %v %q", h.Used, h.Binding)
	}
	if len(h.Bounds) != 2 || h.Bounds[0].Of != "pool" || h.Bounds[1].Of != "account" || h.Bounds[1].Used != 0.94 {
		t.Fatalf("the pool, then the account in use: %+v", h.Bounds)
	}
	if h.Bounds[0].ResetsAt == nil || h.Bounds[0].Burn != 1.5 {
		t.Fatalf("the pool keeps its refill and burn: %+v", h.Bounds[0])
	}
	// Agents that run Fable are bound by its pool.
	f := Machine("claude", []string{"claude-fable-5-2"}, headroomPools(), rot)
	if f.Used != 0.9 || f.Bounds[0].Scope != "Fable" {
		t.Fatalf("the Fable pool binds Fable agents: %v %+v", f.Used, f.Bounds)
	}
}

func TestHeadroomIsSpentWhileNoAccountCanRun(t *testing.T) {
	free := time.Date(2026, 11, 26, 20, 0, 0, 0, time.UTC)
	h := Machine("claude", []string{"opus"}, headroomPools(), Rotation{On: true, Active: "a@example.com", Exhausted: true, FreeAt: free})
	if h.Usable || h.Used != 1 || h.FreeAt == nil || !h.FreeAt.Equal(free) || h.Binding != "every account is spent" {
		t.Fatalf("spent until the first account frees: %+v", h)
	}
}

func TestHeadroomWithNoRotationIsThePools(t *testing.T) {
	h := Machine("codex", nil, headroomPools(), Rotation{})
	if h.Rotating || !h.Usable || h.Used != 0.95 || len(h.Bounds) != 1 {
		t.Fatalf("no rotation: the pool alone: %+v", h)
	}
	none := Machine("grok", nil, headroomPools(), Rotation{})
	if none.Used != 0 || none.Binding != "no limit read" || len(none.Bounds) != 0 {
		t.Fatalf("nothing read: %+v", none)
	}
}
