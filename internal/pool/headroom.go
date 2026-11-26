package pool

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/orchard-suite/tally/internal/collect/promapi"
)

// Headroom is what a machine's agents can draw on from one provider, for
// the models they run: whether an account can run now, the account in use
// and its own limits, and the pools of every account. Used says, in one
// number, how close the machine is to having nothing, so its caller needs
// no rules of its own.
type Headroom struct {
	Provider string `json:"provider"`
	// Rotating is true when the provider's build accounts rotate, so a
	// machine runs on the account in use and moves on when it is spent.
	Rotating bool `json:"rotating"`
	// Usable is true when an account can run now.
	Usable bool `json:"usable"`
	// FreeAt is when an account can run again, while none can.
	FreeAt *time.Time `json:"free_at,omitempty"`
	// Used is how much of what the machine can draw on is used, 0 to 1:
	// 1 while no account can run, else the tightest pool that applies.
	Used float64 `json:"used"`
	// Binding says, in words, what sets Used.
	Binding string `json:"binding"`
	// Bounds are every limit that applies, the tightest first: the pools,
	// then the account in use, whose own limits the rotation steps past.
	Bounds []Bound `json:"bounds"`
}

// Bound is one limit that applies to a machine's agents.
type Bound struct {
	Window string `json:"window"`
	// Scope is the model the limit is kept for; empty for the whole account.
	Scope string `json:"scope,omitempty"`
	// Of is "pool" for every account added up, "account" for the account
	// in use alone.
	Of   string  `json:"of"`
	Used float64 `json:"used"`
	// ResetsAt is the account's reset, or the pool's next refill.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// The pool's own figures, in points: every account is worth 100.
	Accounts  int        `json:"accounts,omitempty"`
	Capacity  float64    `json:"capacity,omitempty"`
	Remaining float64    `json:"remaining,omitempty"`
	Burn      float64    `json:"burn,omitempty"`
	Empty     *time.Time `json:"empty,omitempty"`
}

// Rotation is what the subscriptions collector last said of a provider's
// build accounts.
type Rotation struct {
	// On is true when the provider's accounts rotate.
	On bool
	// Active is the account in use.
	Active string
	// Exhausted is true when every account is at the rotation bar.
	Exhausted bool
	// FreeAt is when the first account can run again, while exhausted.
	FreeAt time.Time
	// Limits are the active account's own limits.
	Limits []Limit
}

// Limit is one limit of one account.
type Limit struct {
	Window   string
	Scope    string
	Percent  float64
	ResetsAt time.Time
}

// applies is true when a limit kept for scope bounds agents that run
// models: an account-wide limit always does, a model's own only when the
// agents run that model.
func applies(scope string, models []string) bool {
	if scope == "" {
		return true
	}
	return slices.ContainsFunc(models, func(m string) bool {
		return strings.Contains(strings.ToLower(m), strings.ToLower(scope)) ||
			strings.Contains(strings.ToLower(scope), strings.ToLower(m))
	})
}

// Machine works out a provider's headroom for agents that run models,
// from the pools and the rotation. It is pure.
func Machine(provider string, models []string, pools []Pool, rot Rotation) Headroom {
	h := Headroom{Provider: provider, Rotating: rot.On, Usable: !rot.Exhausted, Bounds: []Bound{}}
	for _, p := range pools {
		if p.Provider != provider || !applies(p.Scope, models) || p.Capacity <= 0 {
			continue
		}
		b := Bound{Window: p.Window, Scope: p.Scope, Of: "pool", Used: clamp((p.Capacity - p.Remaining) / p.Capacity),
			Accounts: p.Accounts, Capacity: p.Capacity, Remaining: p.Remaining, Burn: p.Burn, Empty: p.Empty}
		if p.NextReset != nil {
			at := p.NextReset.At
			b.ResetsAt = &at
		}
		h.Bounds = append(h.Bounds, b)
	}
	slices.SortStableFunc(h.Bounds, func(a, b Bound) int {
		switch {
		case a.Used > b.Used:
			return -1
		case a.Used < b.Used:
			return 1
		}
		return 0
	})
	if len(h.Bounds) > 0 {
		t := h.Bounds[0]
		h.Used = t.Used
		h.Binding = fmt.Sprintf("%.0f%% of the %s pool used (%d accounts)", t.Used*100, name(provider, t), t.Accounts)
	}
	for _, l := range rot.Limits {
		if !applies(l.Scope, models) {
			continue
		}
		b := Bound{Window: l.Window, Scope: l.Scope, Of: "account", Used: clamp(l.Percent / 100)}
		if !l.ResetsAt.IsZero() {
			at := l.ResetsAt
			b.ResetsAt = &at
		}
		h.Bounds = append(h.Bounds, b)
	}
	if rot.On && rot.Exhausted {
		h.Used = 1
		h.Binding = "every account is spent"
		if !rot.FreeAt.IsZero() {
			at := rot.FreeAt
			h.FreeAt = &at
		}
	}
	if h.Binding == "" {
		h.Binding = "no limit read"
	}
	return h
}

func clamp(v float64) float64 { return max(0, min(1, v)) }

func name(provider string, b Bound) string {
	n := strings.ToUpper(provider[:1]) + provider[1:] + " " + b.Window
	if b.Scope != "" {
		n += ", " + b.Scope
	}
	return n
}

// QueryRotation reads a provider's rotation from the subscriptions
// collector's metrics: the account in use, whether every account is
// spent, when the first frees, and the active account's own limits. A
// provider whose accounts do not rotate reads as off.
func QueryRotation(ctx context.Context, prom *promapi.Client, provider string) (Rotation, error) {
	sel := fmt.Sprintf(`{provider=%q}`, provider)
	exhausted, err := prom.Query(ctx, `max(llm_rotation_exhausted`+sel+`)`)
	if err != nil {
		return Rotation{}, err
	}
	if len(exhausted) == 0 {
		return Rotation{}, nil
	}
	r := Rotation{On: true, Exhausted: exhausted[0].Value >= 1}
	active, err := prom.Query(ctx, `max by (account) (llm_rotation_active`+sel+`) == 1`)
	if err != nil {
		return Rotation{}, err
	}
	if len(active) > 0 {
		r.Active = active[0].Labels["account"]
	}
	if r.Exhausted {
		free, err := prom.Query(ctx, `min(llm_rotation_free_at_seconds`+sel+` > 0)`)
		if err != nil {
			return Rotation{}, err
		}
		if len(free) > 0 {
			r.FreeAt = time.Unix(int64(free[0].Value), 0).UTC()
		}
	}
	if r.Active == "" {
		return r, nil
	}
	who := fmt.Sprintf(`{provider=%q,account=%q}`, provider, r.Active)
	used, err := prom.Query(ctx, `max by (window, scope) (llm_quota_used_percent`+who+`)`)
	if err != nil {
		return Rotation{}, err
	}
	resets, err := prom.Query(ctx, `max by (window, scope) (llm_quota_resets_at_seconds`+who+`)`)
	if err != nil {
		return Rotation{}, err
	}
	at := map[string]time.Time{}
	for _, s := range resets {
		if s.Value > 0 {
			at[s.Labels["window"]+"\x00"+s.Labels["scope"]] = time.Unix(int64(s.Value), 0).UTC()
		}
	}
	for _, s := range used {
		w, sc := s.Labels["window"], s.Labels["scope"]
		r.Limits = append(r.Limits, Limit{Window: w, Scope: sc, Percent: s.Value, ResetsAt: at[w+"\x00"+sc]})
	}
	slices.SortFunc(r.Limits, func(a, b Limit) int { return strings.Compare(a.Window+a.Scope, b.Window+b.Scope) })
	return r, nil
}
