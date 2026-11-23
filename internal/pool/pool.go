// Package pool adds a provider's subscription accounts into one pool of
// quota, each account worth 100%, and projects it forward: every account
// keeps burning at its rate of the last three hours and refills to full at
// each of its resets.
package pool

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/orchard-suite/tally/internal/collect/promapi"
)

// Account is one account's record in one pool.
type Account struct {
	Name string
	// Used is the account's used percent over time, oldest first.
	Used []promapi.Point
	// ResetsAt is the account's next reset.
	ResetsAt time.Time
	// Period is how long one window lasts.
	Period time.Duration
	// Monthly windows reset on the same day each month instead.
	Monthly bool
}

// Pool is one provider's pool, recorded and projected.
type Pool struct {
	Provider string `json:"provider"`
	// Scope is a model a limit is kept for, empty for the account-wide one.
	Scope    string  `json:"scope,omitempty"`
	Window   string  `json:"window"`
	Accounts int     `json:"accounts"`
	Capacity float64 `json:"capacity"`
	// Remaining is the pool left now, in percent.
	Remaining float64 `json:"remaining"`
	// Burn is percent of the pool used an hour, over the last BurnOver.
	Burn      float64 `json:"burn"`
	Recorded  []Point `json:"recorded"`
	Projected []Point `json:"projected"`
	// Refills are the jumps up, recorded and projected.
	Refills []Refill `json:"refills"`
	// Empty is when the projection runs the whole pool out, if it does.
	Empty *time.Time `json:"empty,omitempty"`
	// FirstEmpty is the first account the projection runs out.
	FirstEmpty *AccountAt `json:"first_empty,omitempty"`
	NextReset  *Refill    `json:"next_reset,omitempty"`
}

// Point is the pool's remaining percent at a time.
type Point struct {
	Time  time.Time `json:"t"`
	Value float64   `json:"v"`
}

// Refill is the pool rising by Gain percent at At.
type Refill struct {
	At        time.Time `json:"at"`
	Gain      float64   `json:"gain"`
	Projected bool      `json:"projected"`
}

// AccountAt is an account and a time.
type AccountAt struct {
	Account string    `json:"account"`
	At      time.Time `json:"at"`
}

// Settings of the projection.
const (
	BurnOver = 3 * time.Hour
	// refillAtLeast is the smallest recorded rise drawn as a refill.
	refillAtLeast = 5.0
	// full is a used percent counted as out.
	full = 99.5
)

// Build records the pool from from to now at step and projects it to
// until at step. accounts must be the same provider, scope, and window.
func Build(provider, scope, window string, accounts []Account, from, now, until time.Time, step time.Duration) Pool {
	p := Pool{Provider: provider, Scope: scope, Window: window, Accounts: len(accounts), Capacity: 100 * float64(len(accounts)), Recorded: []Point{}, Projected: []Point{}, Refills: []Refill{}}
	if len(accounts) == 0 {
		return p
	}

	prev := -1.0
	for t := from; !t.After(now); t = t.Add(step) {
		v := 0.0
		for _, a := range accounts {
			v += 100 - at(a.Used, t)
		}
		if prev >= 0 && v-prev >= refillAtLeast {
			p.Refills = append(p.Refills, Refill{At: t, Gain: v - prev})
		}
		p.Recorded = append(p.Recorded, Point{Time: t, Value: v})
		prev = v
	}

	used := make([]float64, len(accounts))
	rate := make([]float64, len(accounts))
	next := make([]time.Time, len(accounts))
	for i, a := range accounts {
		used[i] = at(a.Used, now)
		rate[i] = burned(a.Used, now.Add(-BurnOver), now) / BurnOver.Hours()
		p.Burn += rate[i]
		next[i] = a.ResetsAt
		for !next[i].IsZero() && !next[i].After(now) {
			next[i] = advance(next[i], a)
		}
	}
	remaining := func() float64 {
		v := 0.0
		for _, u := range used {
			v += 100 - u
		}
		return v
	}
	p.Remaining = remaining()
	for i, a := range accounts {
		if used[i] >= full && p.FirstEmpty == nil {
			p.FirstEmpty = &AccountAt{Account: a.Name, At: now}
		}
	}
	p.Projected = append(p.Projected, Point{Time: now, Value: p.Remaining})
	for t := now.Add(step); !t.After(until); t = t.Add(step) {
		hours := step.Hours()
		for i, a := range accounts {
			used[i] = min(100, used[i]+rate[i]*hours)
			if used[i] >= full && p.FirstEmpty == nil {
				p.FirstEmpty = &AccountAt{Account: a.Name, At: t}
			}
			for !next[i].IsZero() && !next[i].After(t) {
				r := Refill{At: next[i], Gain: used[i], Projected: true}
				if r.Gain > 0 {
					p.Refills = append(p.Refills, r)
					if p.NextReset == nil || r.At.Before(p.NextReset.At) {
						nr := r
						p.NextReset = &nr
					}
				}
				used[i] = 0
				next[i] = advance(next[i], a)
			}
		}
		v := remaining()
		if v < 100-full && p.Empty == nil {
			e := t
			p.Empty = &e
		}
		p.Projected = append(p.Projected, Point{Time: t, Value: v})
	}
	sort.SliceStable(p.Refills, func(a, b int) bool { return p.Refills[a].At.Before(p.Refills[b].At) })
	return p
}

func advance(t time.Time, a Account) time.Time {
	if a.Monthly {
		return t.AddDate(0, 1, 0)
	}
	if a.Period <= 0 {
		return time.Time{}
	}
	return t.Add(a.Period)
}

// at is the used percent at t: the last reading at or before t, or the
// first reading when t is before them all.
func at(points []promapi.Point, t time.Time) float64 {
	if len(points) == 0 {
		return 0
	}
	i := sort.Search(len(points), func(i int) bool { return points[i].Time.After(t) })
	if i == 0 {
		return points[0].Value
	}
	return points[i-1].Value
}

// burned is how much used percent rose from from to to, counting only
// rises, so a reset in between is not a negative burn.
func burned(points []promapi.Point, from, to time.Time) float64 {
	sum := 0.0
	last := at(points, from)
	for _, p := range points {
		if !p.Time.After(from) || p.Time.After(to) {
			continue
		}
		if d := p.Value - last; d > 0 {
			sum += d
		}
		last = p.Value
	}
	return sum
}

// Query reads every pool from Prometheus: the weekly and monthly limits of
// every account, by provider and scope.
func Query(ctx context.Context, prom *promapi.Client, now time.Time) ([]Pool, error) {
	const by = `max by (provider, account, scope, window) `
	const sel = `{window=~"weekly|monthly"}`
	from := now.Add(-72 * time.Hour)
	series, err := prom.QueryRange(ctx, by+`(llm_quota_used_percent`+sel+`)`, from, now, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	resets, err := prom.Query(ctx, by+`(llm_quota_resets_at_seconds`+sel+`)`)
	if err != nil {
		return nil, err
	}
	key := func(l map[string]string) string { return l["provider"] + "\x00" + l["scope"] + "\x00" + l["window"] }
	resetAt := map[string]time.Time{}
	for _, s := range resets {
		if s.Value > 0 {
			resetAt[key(s.Labels)+"\x00"+s.Labels["account"]] = time.Unix(int64(s.Value), 0).UTC()
		}
	}
	groups := map[string][]Account{}
	labels := map[string]map[string]string{}
	for _, s := range series {
		k := key(s.Labels)
		a := Account{Name: s.Labels["account"], Used: s.Points, ResetsAt: resetAt[k+"\x00"+s.Labels["account"]]}
		switch s.Labels["window"] {
		case "weekly":
			a.Period = 7 * 24 * time.Hour
		case "monthly":
			a.Monthly = true
		}
		groups[k] = append(groups[k], a)
		labels[k] = s.Labels
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Pool, 0, len(keys))
	for _, k := range keys {
		accs := groups[k]
		sort.Slice(accs, func(a, b int) bool { return strings.Compare(accs[a].Name, accs[b].Name) < 0 })
		l := labels[k]
		out = append(out, Build(l["provider"], l["scope"], l["window"], accs, from, now, now.Add(7*24*time.Hour), 15*time.Minute))
	}
	return out, nil
}
