package subscriptions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// The build accounts: the Claude subscriptions every build service and dev
// computer shares, one at a time. The rotation file lists them, each with
// its long-lived token (`claude setup-token`). After every round the probe
// picks the account that gives the most power next, and hands its token on
// as the active one. An account that must keep its own token is never
// listed.

// rotationAccount is one build account in the rotation file.
type rotationAccount struct {
	Account string `json:"account"`
	Token   string `json:"token"`
}

// active is what the probe hands on: the build account in use and its
// token. Its JSON is the value of the active secret.
type active struct {
	Account string `json:"account"`
	Token   string `json:"token"`
}

// loadRotation reads the rotation file. No file means no rotation.
func loadRotation(path string) ([]rotationAccount, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var accts []rotationAccount
	if err := json.Unmarshal(raw, &accts); err != nil {
		return nil, fmt.Errorf("rotation file: %w", err)
	}
	for i, a := range accts {
		if a.Account == "" || !strings.HasPrefix(a.Token, "sk-ant-") {
			return nil, fmt.Errorf("rotation file: entry %d needs an account and a Claude token", i+1)
		}
	}
	return accts, nil
}

// choice is the account pick picked, why, and where every build account
// stands.
type choice struct {
	active
	// Exhausted is true when no account can be used now; the pick is then
	// the one whose blocking limit resets first.
	Exhausted bool
	Why       string
	// Standings holds every build account, the active one first and then in
	// the order the rotation would take them.
	Standings []standing
	// Pinned is the account picked by hand that the pick honored; empty
	// when the pick is automatic.
	Pinned string
	// Unpin is true when the account picked by hand can no longer be used,
	// so the pick is automatic again.
	Unpin bool
}

// standing is where one build account stands in the rotation.
type standing struct {
	Account string
	// Rank is 1 for the active account, 2 for the next, and so on.
	Rank int
	// Read is false when this round's read of it failed; the times below
	// are then unknown.
	Read bool
	// FreeAt is when it can be used again: 0 when it can now, unknownTime
	// when a limit it hit gave no reset.
	FreeAt int64
	// PreferredAt is when it has the preferred model to use again, by the
	// same rules; unknownTime too when it does not offer the model.
	PreferredAt int64
}

// unknownTime marks a limit with no reset time.
const unknownTime = -1

// usage is what one account's read says, for picking.
type usage struct {
	acct rotationAccount
	// usable: its account-wide session and weekly limits are under the bar,
	// so it can work in the next five hours.
	usable bool
	// preferred: usable, it reports a weekly limit on the preferred model,
	// and that limit is under the bar too. An account that reports no such
	// limit does not offer the model.
	preferred bool
	// reserve: a Pro account. Its week is a small fraction of a Max or
	// Ultra week, so its percents say nothing next to theirs: it runs only
	// when no larger account can.
	reserve bool
	// scoped is the percent used of the preferred model's weekly limit.
	scoped float64
	// weekly is the percent used of the account-wide weekly limit.
	weekly float64
	// free is when the account can be used again, in unix seconds; 0 when
	// it can be used now.
	free int64
	// preferredAt is when it has the preferred model again; 0 when now.
	preferredAt int64
}

const never = int64(1) << 62

func measure(a rotationAccount, r Reading, bar float64, prefer string) usage {
	u := usage{acct: a}
	var over []Limit
	var scopedOver *Limit
	offers := false
	for _, l := range r.Limits {
		if strings.EqualFold(l.Plan, "pro") {
			u.reserve = true
		}
		switch {
		case l.Scope == "" && l.Window == "weekly":
			u.weekly = l.Percent
		case strings.EqualFold(l.Scope, prefer) && l.Window == "weekly":
			u.scoped = l.Percent
			offers = true
			if l.Percent >= bar {
				scopedOver = &l
			}
		}
		if l.Scope == "" && (l.Window == "session" || l.Window == "weekly") && l.Percent >= bar {
			over = append(over, l)
		}
	}
	u.usable = len(over) == 0
	u.preferred = u.usable && offers && scopedOver == nil
	u.free = latestReset(over)
	switch {
	case !offers:
		u.preferredAt = never
	case scopedOver != nil:
		u.preferredAt = latestReset(append(over, *scopedOver))
	default:
		u.preferredAt = u.free
	}
	return u
}

// latestReset is when every one of the limits has reset: 0 for none, never
// when one gives no reset time.
func latestReset(limits []Limit) int64 {
	var at int64
	for _, l := range limits {
		if l.ResetsAt == 0 {
			return never
		}
		at = max(at, l.ResetsAt)
	}
	return at
}

// group orders accounts by the power they give next: a larger account
// before a reserve one, and within each, the preferred model left before
// none. An account that cannot run comes last.
func (u usage) group() int {
	switch {
	case !u.usable:
		return groupSpent
	case u.reserve && u.preferred:
		return 3
	case u.reserve:
		return 4
	case u.preferred:
		return 1
	default:
		return 2
	}
}

const groupSpent = 5

// better is true when a gives more power next than b.
func better(a, b usage) bool {
	if a.group() != b.group() {
		return a.group() < b.group()
	}
	switch a.group() {
	case 1, 3:
		return a.scoped < b.scoped
	case 2, 4:
		return a.weekly > b.weekly
	default:
		return a.free < b.free
	}
}

func (u usage) standing() standing {
	at := func(t int64) int64 {
		if t == never {
			return unknownTime
		}
		return t
	}
	return standing{Account: u.acct.Account, Read: true, FreeAt: at(u.free), PreferredAt: at(u.preferredAt)}
}

// pick takes the account that gives the most power next:
//
//  1. Of the accounts usable in the next five hours (session and weekly
//     limits under the bar), one with the preferred model left, the one
//     with the most of it left.
//  2. With none of those, the usable account with the most of its week
//     used, so the week that resets first is spent first.
//  3. Reserve (Pro) accounts only when no larger account is usable, by
//     the same two rules.
//  4. With nothing usable, the account that frees first.
//
// The current account stays while it is still in the best group anyone is
// in, so the pick does not flip back and forth as usage moves. An account
// picked by hand (pinned) stays while it is usable at all. Ties go to the
// rotation file's order. With no account read at all, the pinned or current
// account stands. Otherwise an account whose read failed is passed over:
// nothing says it has room. The current or pinned account is the exception
// when its read failed for a passing reason: nothing says it hit a limit
// either, and a switch costs every build its prompt cache.
func pick(order []rotationAccount, readings []Reading, bar float64, prefer, current, pinned string) choice {
	byAccount := map[string]Reading{}
	for _, r := range readings {
		if r.Account.Provider == "claude" {
			byAccount[strings.ToLower(r.Account.Email)] = r
		}
	}
	var all []usage
	var unread []rotationAccount
	var failed []rotationAccount
	for _, a := range order {
		r, ok := byAccount[strings.ToLower(a.Account)]
		if !ok || r.Err != nil || len(r.Limits) == 0 {
			unread = append(unread, a)
			if ok && r.Err != nil && reason(r.Err) == reasonReadFailed {
				failed = append(failed, a)
			}
			continue
		}
		all = append(all, measure(a, r, bar, prefer))
	}
	is := func(name string) func(rotationAccount) bool {
		return func(a rotationAccount) bool { return name != "" && strings.EqualFold(a.Account, name) }
	}
	// A pin holds while its account is usable, or its read failed for a
	// passing reason, or no account could be read at all: then nothing
	// says the pinned account hit a limit. Otherwise the pick is automatic
	// again.
	blind := len(all) == 0
	pinHolds := slices.ContainsFunc(failed, is(pinned)) ||
		slices.ContainsFunc(all, func(u usage) bool { return is(pinned)(u.acct) && u.usable }) ||
		(blind && slices.ContainsFunc(order, is(pinned)))
	keep := current
	if pinHolds {
		keep = pinned
	}
	var held *rotationAccount
	if i := slices.IndexFunc(failed, is(keep)); i >= 0 {
		held = &failed[i]
	}

	// Rank by power next; a stable sort keeps the file's order for ties.
	ranked := slices.Clone(all)
	slices.SortStableFunc(ranked, func(a, b usage) int {
		switch {
		case better(a, b):
			return -1
		case better(b, a):
			return 1
		}
		return 0
	})

	var ch choice
	switch {
	case held != nil:
		ch = choice{active: active{Account: held.Account, Token: held.Token}, Why: "its read failed this round; it stays"}
	case len(ranked) == 0:
		if len(order) == 0 {
			return choice{}
		}
		// With nothing read, the pinned or current account stands: falling
		// back to the first could hand builds an account that is spent.
		if i := slices.IndexFunc(order, is(keep)); i >= 0 {
			ch = choice{active: active{Account: order[i].Account, Token: order[i].Token}, Exhausted: true, Why: "no account could be read; it stays"}
			break
		}
		ch = choice{active: active{Account: order[0].Account, Token: order[0].Token}, Exhausted: true, Why: "no account could be read; the first one stands"}
	default:
		best := ranked[0]
		why := map[int]string{
			1:          "usable now with the most " + prefer + " left",
			2:          "no account usable now has " + prefer + " left; usable now with the most of its week used",
			3:          "no larger account is usable now; a reserve account with the most " + prefer + " left",
			4:          "no larger account is usable now; a reserve account with the most of its week used",
			groupSpent: "no account is usable now; this one frees first",
		}[best.group()]
		for _, u := range ranked {
			switch {
			case pinHolds && is(pinned)(u.acct):
				best, why = u, "picked by hand"
			case !pinHolds && is(current)(u.acct) && u.group() == best.group() && u.usable:
				best = u
			}
		}
		ch = choice{active: active{Account: best.acct.Account, Token: best.acct.Token}, Exhausted: !best.usable, Why: why}
	}
	if pinHolds {
		ch.Pinned = ch.Account
	} else if pinned != "" {
		ch.Unpin = true
	}

	rank := 1
	add := func(s standing) {
		if strings.EqualFold(s.Account, ch.Account) {
			s.Rank = 1
		} else {
			rank++
			s.Rank = rank
		}
		ch.Standings = append(ch.Standings, s)
	}
	for _, u := range ranked {
		add(u.standing())
	}
	for _, a := range unread {
		add(standing{Account: a.Account})
	}
	slices.SortStableFunc(ch.Standings, func(a, b standing) int { return a.Rank - b.Rank })
	return ch
}
