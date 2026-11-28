package agents

import (
	"sort"
	"strings"
	"time"
)

// Currency is what every price in the table is in.
const Currency = "USD"

// RunCost is what one run cost, in the shape a Vantage run record holds:
// an amount, its currency, and the tokens behind it.
type RunCost struct {
	Run    string  `json:"run"`
	Amount float64 `json:"amount"`
	// Currency is always Currency.
	Currency      string `json:"currency"`
	Tokens        int64  `json:"tokens"`
	Conversations int    `json:"conversations"`
	// Through is the end of the last minute priced, when any was.
	Through *time.Time `json:"through,omitempty"`
}

// PriceRun is what run cost over rows, each minute at its day's price. A row
// of another run adds nothing.
func PriceRun(run string, rows []Row, prices Table) RunCost {
	out := RunCost{Run: run, Currency: Currency}
	convs := map[string]struct{}{}
	for _, r := range rows {
		if RunOf(r.Conversation) != run {
			continue
		}
		convs[r.Conversation] = struct{}{}
		out.Tokens += r.Total()
		if rate, ok := prices.Rate(r.Model, r.Minute); ok {
			out.Amount += rate.Cost(r.Tokens)
		}
		if end := r.Minute.Add(time.Minute); out.Through == nil || end.After(*out.Through) {
			out.Through = &end
		}
	}
	out.Conversations = len(convs)
	return out
}

// Spend lines.
type (
	// SpendReport is agent work over a span, rolled up through the Vantage
	// items its runs worked: by item, by scope (the item's program, and each
	// department above it), and by agent. It names no person.
	SpendReport struct {
		From     time.Time   `json:"from"`
		To       time.Time   `json:"to"`
		Currency string      `json:"currency"`
		Total    SpendLine   `json:"total"`
		Items    []ItemSpend `json:"items"`
		// Scopes are every scope with spend in it or under it, the
		// organization's top first.
		Scopes []ScopeSpend `json:"scopes"`
		Agents []AgentSpend `json:"agents"`
		// Unlinked is the work of runs that claimed no item.
		Unlinked SpendLine `json:"unlinked"`
		// ShowsHosts says whether an agent is named with its host.
		ShowsHosts bool `json:"shows_hosts"`
		// Unpriced are models the table does not price.
		Unpriced []string `json:"unpriced"`
	}
	// SpendLine is a cost, its tokens, and how many runs it came from.
	SpendLine struct {
		Cost   float64 `json:"cost"`
		Tokens int64   `json:"tokens"`
		Runs   int     `json:"runs"`
	}
	ItemSpend struct {
		Item string `json:"item"`
		// Scope is the item's program, empty for the whole estate or one
		// whose scope is not read yet.
		Scope string `json:"scope"`
		SpendLine
	}
	ScopeSpend struct {
		Scope  string `json:"scope"`
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Parent string `json:"parent,omitempty"`
		// Own is the items in this scope; Total adds every scope under it.
		Own   SpendLine `json:"own"`
		Total SpendLine `json:"total"`
	}
	AgentSpend struct {
		Agent string `json:"agent"`
		SpendLine
	}
)

// spendAcc adds costs up and counts each run once.
type spendAcc struct {
	line SpendLine
	runs map[string]struct{}
}

func (a *spendAcc) add(run string, cost float64, tokens int64) {
	if a.runs == nil {
		a.runs = map[string]struct{}{}
	}
	a.line.Cost += cost
	a.line.Tokens += tokens
	a.runs[run] = struct{}{}
}

func (a *spendAcc) done() SpendLine {
	a.line.Runs = len(a.runs)
	return a.line
}

// itemAt is the item a run worked in minute: the one it claimed most
// recently at or before that minute, or its first for a minute before any
// claim. claims are the run's, oldest first.
func itemAt(claims []Claim, minute time.Time) Claim {
	at := claims[0]
	for _, c := range claims[1:] {
		if c.At.Truncate(time.Minute).After(minute) {
			break
		}
		at = c
	}
	return at
}

// BuildSpend rolls rows up through links. Each minute of a run goes whole to
// one item (itemAt), and a conversation's cost is never split across
// conversations.
func BuildSpend(rows []Row, links Links, prices Table, from, to time.Time) SpendReport {
	rep := SpendReport{From: from, To: to, Currency: Currency, ShowsHosts: true, Items: []ItemSpend{}, Scopes: []ScopeSpend{}, Agents: []AgentSpend{}, Unpriced: []string{}}
	byRun := map[string][]Claim{}
	for _, c := range links.Claims {
		byRun[c.Run] = append(byRun[c.Run], c)
	}
	for _, cs := range byRun {
		sort.Slice(cs, func(a, b int) bool { return cs[a].At.Before(cs[b].At) })
	}
	var total, unlinked spendAcc
	items := map[string]*spendAcc{}
	agents := map[string]*spendAcc{}
	unpriced := map[string]bool{}
	for _, r := range rows {
		cost := 0.0
		if rate, ok := prices.Rate(r.Model, r.Minute); ok {
			cost = rate.Cost(r.Tokens)
		} else {
			unpriced[Model(r.Model)] = true
		}
		run := RunOf(r.Conversation)
		if run == "" {
			run = r.Machine + "\x00" + r.Conversation
		}
		tokens := r.Total()
		total.add(run, cost, tokens)
		claims := byRun[run]
		if len(claims) == 0 {
			unlinked.add(run, cost, tokens)
			continue
		}
		c := itemAt(claims, r.Minute)
		if items[c.Item] == nil {
			items[c.Item] = &spendAcc{}
		}
		items[c.Item].add(run, cost, tokens)
		if agents[c.Agent] == nil {
			agents[c.Agent] = &spendAcc{}
		}
		agents[c.Agent].add(run, cost, tokens)
	}
	rep.Total, rep.Unlinked = total.done(), unlinked.done()

	tree := map[string]ScopeNode{}
	for _, n := range links.Tree {
		tree[n.ID] = n
	}
	own := map[string]*spendAcc{}
	under := map[string]*spendAcc{}
	for item, a := range items {
		scope := links.Scopes[item]
		rep.Items = append(rep.Items, ItemSpend{Item: item, Scope: scope, SpendLine: a.done()})
		if scope == "" {
			continue
		}
		if own[scope] == nil {
			own[scope] = &spendAcc{}
		}
		merge(own[scope], a)
		seen := map[string]bool{}
		for s := scope; s != "" && !seen[s]; s = tree[s].Parent {
			seen[s] = true
			if under[s] == nil {
				under[s] = &spendAcc{}
			}
			merge(under[s], a)
		}
	}
	for s, a := range under {
		n := tree[s]
		line := ScopeSpend{Scope: s, Name: n.Name, Kind: n.Kind, Parent: n.Parent, Total: a.done()}
		if o := own[s]; o != nil {
			line.Own = o.done()
		}
		if line.Name == "" {
			line.Name = s
		}
		rep.Scopes = append(rep.Scopes, line)
	}
	depth := func(s string) int {
		d := 0
		seen := map[string]bool{}
		for p := tree[s].Parent; p != "" && !seen[p]; p = tree[p].Parent {
			seen[p] = true
			d++
		}
		return d
	}
	sort.Slice(rep.Scopes, func(a, b int) bool {
		x, y := rep.Scopes[a], rep.Scopes[b]
		if dx, dy := depth(x.Scope), depth(y.Scope); dx != dy {
			return dx < dy
		}
		if x.Total.Cost != y.Total.Cost {
			return x.Total.Cost > y.Total.Cost
		}
		return x.Scope < y.Scope
	})
	sort.Slice(rep.Items, func(a, b int) bool {
		x, y := rep.Items[a], rep.Items[b]
		if x.Cost != y.Cost {
			return x.Cost > y.Cost
		}
		return x.Item < y.Item
	})
	for agent, a := range agents {
		rep.Agents = append(rep.Agents, AgentSpend{Agent: agent, SpendLine: a.done()})
	}
	sortAgents(rep.Agents)
	for m := range unpriced {
		rep.Unpriced = append(rep.Unpriced, m)
	}
	sort.Strings(rep.Unpriced)
	return rep
}

// merge adds b's cost, tokens, and runs into a.
func merge(a, b *spendAcc) {
	if a.runs == nil {
		a.runs = map[string]struct{}{}
	}
	a.line.Cost += b.line.Cost
	a.line.Tokens += b.line.Tokens
	for r := range b.runs {
		a.runs[r] = struct{}{}
	}
}

func sortAgents(as []AgentSpend) {
	sort.Slice(as, func(a, b int) bool {
		x, y := as[a], as[b]
		if x.Cost != y.Cost {
			return x.Cost > y.Cost
		}
		return x.Agent < y.Agent
	})
}

// AgentName is an agent as Vantage names it, without its host: "claude-code
// on laptop-1" is "claude-code". A host is a machine, and a machine is a
// person's.
func AgentName(label string) string {
	if i := strings.LastIndex(label, " on "); i > 0 {
		return label[:i]
	}
	return label
}

// Limit is the report as one person may see it: an agent is named with its
// host only with hosts, and agents of one name are one line without.
func (r SpendReport) Limit(hosts bool) SpendReport {
	r.ShowsHosts = hosts
	if hosts {
		return r
	}
	// Runs of one name on two hosts are two runs, so counts add.
	by := map[string]*AgentSpend{}
	for _, a := range r.Agents {
		name := AgentName(a.Agent)
		x := by[name]
		if x == nil {
			x = &AgentSpend{Agent: name}
			by[name] = x
		}
		x.Cost += a.Cost
		x.Tokens += a.Tokens
		x.Runs += a.Runs
	}
	r.Agents = make([]AgentSpend, 0, len(by))
	for _, a := range by {
		r.Agents = append(r.Agents, *a)
	}
	sortAgents(r.Agents)
	return r
}
