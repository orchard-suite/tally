package pool

import (
	"math"
	"testing"
	"time"

	"github.com/orchard-suite/tally/internal/collect/promapi"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestBuild(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	hour := func(h float64) time.Time { return now.Add(time.Duration(h * float64(time.Hour))) }
	week := 7 * 24 * time.Hour
	accounts := []Account{
		{
			// Burns 10 points an hour; its weekly reset was at hour -2,
			// which dropped it from 80 to 0.
			Name: "busy@example.com",
			Used: []promapi.Point{{Time: hour(-4), Value: 70}, {Time: hour(-3), Value: 80}, {Time: hour(-2), Value: 0}, {Time: hour(-1), Value: 10}, {Time: hour(0), Value: 20}},
			// Stale: the next reset is a week after the one that happened.
			ResetsAt: hour(-2),
			Period:   week,
		},
		{
			// Idle at 50, resets in six hours.
			Name:     "idle@example.com",
			Used:     []promapi.Point{{Time: hour(-4), Value: 50}},
			ResetsAt: hour(6),
			Period:   week,
		},
	}
	p := Build("claude", "", "weekly", accounts, hour(-4), now, hour(24), time.Hour)

	if p.Capacity != 200 || !near(p.Remaining, 80+50) {
		t.Fatalf("capacity %v remaining %v", p.Capacity, p.Remaining)
	}
	// The first account rose 10 in hour -3 (from 70 to 80 at -3 is before
	// the window), then 10 and 10 after its reset: 20 over three hours.
	if !near(p.Burn, 20.0/3) {
		t.Fatalf("burn %v", p.Burn)
	}
	if len(p.Recorded) != 5 || !near(p.Recorded[0].Value, 30+50) || !near(p.Recorded[2].Value, 100+50) {
		t.Fatalf("recorded %+v", p.Recorded)
	}
	if len(p.Refills) == 0 || p.Refills[0].Projected || !p.Refills[0].At.Equal(hour(-2)) || !near(p.Refills[0].Gain, 80) {
		t.Fatalf("the recorded reset is a refill: %+v", p.Refills)
	}
	// The idle account refills its 50 at hour 6.
	if p.NextReset == nil || !p.NextReset.At.Equal(hour(6)) || !near(p.NextReset.Gain, 50) || !p.NextReset.Projected {
		t.Fatalf("next reset %+v", p.NextReset)
	}
	// The busy one runs out at 20 + 20/3 per hour: 100 after 12 hours.
	if p.FirstEmpty == nil || p.FirstEmpty.Account != "busy@example.com" || !p.FirstEmpty.At.Equal(hour(12)) {
		t.Fatalf("first empty %+v", p.FirstEmpty)
	}
	if p.Empty != nil {
		t.Fatalf("the idle account never runs out, so the pool does not: %v", p.Empty)
	}
	last := p.Projected[len(p.Projected)-1]
	if !last.Time.Equal(hour(24)) || !near(last.Value, 0+100) {
		t.Fatalf("last projected %+v", last)
	}
}

func TestBuildMonthlyAndEmpty(t *testing.T) {
	now := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	a := Account{Name: "a@example.com", Used: []promapi.Point{{Time: now.Add(-3 * time.Hour), Value: 70}, {Time: now, Value: 94}}, ResetsAt: now.Add(48 * time.Hour), Monthly: true}
	p := Build("codex", "", "monthly", []Account{a}, now.Add(-3*time.Hour), now, now.Add(72*time.Hour), time.Hour)
	if p.Empty == nil || !p.Empty.Equal(now.Add(1*time.Hour)) {
		t.Fatalf("empty %v: 94 + 8 an hour passes 99.5 in the first hour", p.Empty)
	}
	if p.NextReset == nil || !p.NextReset.At.Equal(now.Add(48*time.Hour)) || !near(p.NextReset.Gain, 100) {
		t.Fatalf("next reset %+v", p.NextReset)
	}
	if got := advance(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), a); !got.Equal(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly advance %v", got)
	}
}

func TestBuildNoAccounts(t *testing.T) {
	now := time.Now()
	p := Build("x", "", "weekly", nil, now, now, now, time.Hour)
	if p.Accounts != 0 || len(p.Projected) != 0 {
		t.Fatal("no accounts is an empty pool")
	}
}
