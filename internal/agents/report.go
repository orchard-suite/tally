package agents

import (
	"sort"
	"time"
)

// Report is what agent work cost over a span: totals, every work with its
// activity, and the cost by model. Cost is an estimate at API prices.
type Report struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Totals Totals    `json:"totals"`
	// Billing is how the work is paid for: subscription or api.
	Billing string `json:"billing"`
	// Zone is the zone the report's days begin in.
	Zone string `json:"zone"`
	// ShowsUsers says whether Machines is filled: who spent what.
	ShowsUsers bool `json:"shows_users"`
	// ShowsWorks says whether Works and Children are filled: the work and
	// conversations themselves.
	ShowsWorks bool          `json:"shows_works"`
	Works      []WorkLine    `json:"works"`
	Machines   []MachineLine `json:"machines"`
	Models     []ModelLine   `json:"models"`
	Children   ChildLine     `json:"children"`
	// Unpriced are models the price table does not list; their tokens
	// count, their cost does not.
	Unpriced []string `json:"unpriced"`
}

// Totals are the whole span's.
type Totals struct {
	Tokens
	Total int64   `json:"total"`
	Cost  float64 `json:"cost"`
	// AgentHours counts every conversation's active minutes, so agents
	// working at once each count.
	AgentHours float64 `json:"agent_hours"`
	// ActiveHours is the time any agent was active.
	ActiveHours float64    `json:"active_hours"`
	First       *time.Time `json:"first,omitempty"`
	Last        *time.Time `json:"last,omitempty"`
}

// WorkLine is one work's share of the span.
type WorkLine struct {
	ID         string    `json:"id"`
	Machine    string    `json:"machine"`
	Title      string    `json:"title"`
	Project    string    `json:"project"`
	Total      int64     `json:"total"`
	Cost       float64   `json:"cost"`
	AgentHours float64   `json:"agent_hours"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	Children   int       `json:"children"`
	Ongoing    bool      `json:"ongoing"`
	// Lead and Child are when the lead and its children were active, in
	// minutes from the span's start.
	Lead  []Span `json:"lead"`
	Child []Span `json:"child"`
}

// Span is a run of active minutes, [Start, End) from the report's start.
type Span struct {
	Start int `json:"s"`
	End   int `json:"e"`
}

// MachineLine is one machine's share: who spent what.
type MachineLine struct {
	Machine string `json:"machine"`
	// Owner is who the machine belongs to, when one is set.
	Owner      string  `json:"owner,omitempty"`
	Total      int64   `json:"total"`
	Cost       float64 `json:"cost"`
	AgentHours float64 `json:"agent_hours"`
	Works      int     `json:"works"`
}

// Limit is the report as one person may see it: totals and each model
// always, each machine only with users, and the work, its conversations,
// and its children only with works.
func (r Report) Limit(users, works bool) Report {
	r.ShowsUsers, r.ShowsWorks = users, works
	if !users {
		r.Machines = []MachineLine{}
	}
	if !works {
		r.Works = []WorkLine{}
		r.Children = ChildLine{}
	}
	return r
}

// ModelLine is one model's share.
type ModelLine struct {
	Model  string  `json:"model"`
	Total  int64   `json:"total"`
	Cost   float64 `json:"cost"`
	Share  float64 `json:"share"`
	Priced bool    `json:"priced"`
}

// ChildLine is what every child conversation added up to.
type ChildLine struct {
	Conversations int     `json:"conversations"`
	Total         int64   `json:"total"`
	Cost          float64 `json:"cost"`
}

// Bridge is the longest quiet stretch, in minutes, that still counts as
// one run of activity on the timeline: a build or a long tool call.
const Bridge = 3

// ongoing is how recently a work must have been active to be ongoing.
const ongoing = 5 * time.Minute

// BuildReport adds rows up from from to to. A cost belongs to the
// conversation that spent it and is never split.
func BuildReport(rows []Row, prices Table, from, to, now time.Time) Report {
	rep := Report{From: from, To: to, ShowsUsers: true, ShowsWorks: true, Works: []WorkLine{}, Machines: []MachineLine{}, Models: []ModelLine{}, Unpriced: []string{}}
	type convKey struct{ machine, id string }
	type minuteKey struct {
		conv   convKey
		minute int64
	}
	type workAcc struct {
		line     WorkLine
		minutes  map[minuteKey]struct{}
		lead     map[int]struct{}
		child    map[int]struct{}
		children map[convKey]struct{}
	}
	works := map[convKey]*workAcc{}
	type machineAcc struct {
		line    MachineLine
		minutes map[minuteKey]struct{}
		works   map[string]struct{}
	}
	machines := map[string]*machineAcc{}
	models := map[string]*ModelLine{}
	unpriced := map[string]bool{}
	convMinutes := map[minuteKey]struct{}{}
	anyMinute := map[int64]struct{}{}
	childConvs := map[convKey]struct{}{}
	var first, last time.Time

	for _, r := range rows {
		model := Model(r.Model)
		cost := 0.0
		rate, priced := prices.Rate(model, r.Minute)
		if priced {
			cost = rate.Cost(r.Tokens)
		} else {
			unpriced[model] = true
		}
		total := r.Total()

		rep.Totals.Add(r.Tokens)
		rep.Totals.Total += total
		rep.Totals.Cost += cost

		m := models[model]
		if m == nil {
			m = &ModelLine{Model: model, Priced: priced}
			models[model] = m
		}
		m.Total += total
		m.Cost += cost

		ck := convKey{r.Machine, r.Conversation}
		mk := minuteKey{ck, r.Minute.Unix()}
		ma := machines[r.Machine]
		if ma == nil {
			ma = &machineAcc{line: MachineLine{Machine: r.Machine}, minutes: map[minuteKey]struct{}{}, works: map[string]struct{}{}}
			machines[r.Machine] = ma
		}
		ma.line.Total += total
		ma.line.Cost += cost
		ma.minutes[mk] = struct{}{}
		ma.works[r.Work] = struct{}{}
		convMinutes[mk] = struct{}{}
		anyMinute[r.Minute.Unix()] = struct{}{}
		if r.Role == Child {
			childConvs[ck] = struct{}{}
			rep.Children.Total += total
			rep.Children.Cost += cost
		}
		if first.IsZero() || r.Minute.Before(first) {
			first = r.Minute
		}
		if end := r.Minute.Add(time.Minute); end.After(last) {
			last = end
		}

		wk := convKey{r.Machine, r.Work}
		w := works[wk]
		if w == nil {
			w = &workAcc{line: WorkLine{ID: r.Work, Machine: r.Machine, Title: r.WorkTitle, Project: r.Project, First: r.Minute},
				minutes: map[minuteKey]struct{}{}, lead: map[int]struct{}{}, child: map[int]struct{}{}, children: map[convKey]struct{}{}}
			works[wk] = w
		}
		w.line.Total += total
		w.line.Cost += cost
		w.minutes[mk] = struct{}{}
		if r.Minute.Before(w.line.First) {
			w.line.First = r.Minute
		}
		if end := r.Minute.Add(time.Minute); end.After(w.line.Last) {
			w.line.Last = end
		}
		offset := int(r.Minute.Sub(from) / time.Minute)
		if r.Role == Child {
			w.child[offset] = struct{}{}
			w.children[ck] = struct{}{}
		} else {
			w.lead[offset] = struct{}{}
		}
	}

	rep.Totals.AgentHours = float64(len(convMinutes)) / 60
	rep.Totals.ActiveHours = float64(len(anyMinute)) / 60
	if !first.IsZero() {
		rep.Totals.First, rep.Totals.Last = &first, &last
	}
	rep.Children.Conversations = len(childConvs)

	for _, w := range works {
		w.line.AgentHours = float64(len(w.minutes)) / 60
		w.line.Children = len(w.children)
		w.line.Ongoing = now.Sub(w.line.Last) < ongoing
		w.line.Lead = spans(w.lead)
		w.line.Child = spans(w.child)
		rep.Works = append(rep.Works, w.line)
	}
	sort.Slice(rep.Works, func(a, b int) bool {
		x, y := rep.Works[a], rep.Works[b]
		if !x.First.Equal(y.First) {
			return x.First.Before(y.First)
		}
		return x.ID < y.ID
	})

	for _, m := range machines {
		m.line.AgentHours = float64(len(m.minutes)) / 60
		m.line.Works = len(m.works)
		rep.Machines = append(rep.Machines, m.line)
	}
	sort.Slice(rep.Machines, func(a, b int) bool {
		x, y := rep.Machines[a], rep.Machines[b]
		if x.Cost != y.Cost {
			return x.Cost > y.Cost
		}
		return x.Machine < y.Machine
	})

	for _, m := range models {
		if rep.Totals.Cost > 0 {
			m.Share = m.Cost / rep.Totals.Cost
		}
		rep.Models = append(rep.Models, *m)
	}
	sort.Slice(rep.Models, func(a, b int) bool {
		x, y := rep.Models[a], rep.Models[b]
		if x.Cost != y.Cost {
			return x.Cost > y.Cost
		}
		if x.Total != y.Total {
			return x.Total > y.Total
		}
		return x.Model < y.Model
	})
	for m := range unpriced {
		rep.Unpriced = append(rep.Unpriced, m)
	}
	sort.Strings(rep.Unpriced)
	return rep
}

// spans joins active minutes into runs, bridging quiet stretches of up to
// Bridge minutes.
func spans(minutes map[int]struct{}) []Span {
	out := []Span{}
	if len(minutes) == 0 {
		return out
	}
	ms := make([]int, 0, len(minutes))
	for m := range minutes {
		ms = append(ms, m)
	}
	sort.Ints(ms)
	cur := Span{Start: ms[0], End: ms[0] + 1}
	for _, m := range ms[1:] {
		if m-cur.End <= Bridge {
			cur.End = m + 1
			continue
		}
		out = append(out, cur)
		cur = Span{Start: m, End: m + 1}
	}
	return append(out, cur)
}
