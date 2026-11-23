package subscriptions

import (
	"sort"
	"strings"
	"sync/atomic"
)

// Plan is which subscription one signed-in account is on, as its CLI last
// said: claude max, codex plus. An account whose read failed keeps the plan
// it last reported; one never read has none.
type Plan struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Plan     string `json:"plan"`
}

var latestPlans atomic.Pointer[[]Plan]

// Plans is every account's plan from the last round, sorted by provider and
// account. Empty before the first round.
func Plans() []Plan {
	if p := latestPlans.Load(); p != nil {
		return *p
	}
	return nil
}

// notePlans keeps each account's plan from a round's readings, carrying the
// last known plan forward for an account whose read failed this round.
func notePlans(readings []Reading) {
	known := map[string]string{}
	for _, p := range Plans() {
		known[p.Provider+":"+p.Account] = p.Plan
	}
	var out []Plan
	for _, r := range readings {
		account := strings.ToLower(r.Account.Email)
		key := r.Account.Provider + ":" + account
		plan := known[key]
		for _, l := range r.Limits {
			if l.Plan != "" {
				plan = l.Plan
				break
			}
		}
		out = append(out, Plan{Provider: r.Account.Provider, Account: account, Plan: plan})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Account < out[j].Account
	})
	latestPlans.Store(&out)
}
