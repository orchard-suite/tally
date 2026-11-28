package estate

import (
	"sort"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
	"github.com/orchard-suite/tally/internal/collect/billed"
	"github.com/orchard-suite/tally/internal/collect/gateway"
)

// Three lenses see AI, and each is the truth for one thing:
//
//   - The bill is the truth for what was paid. Nothing below it changes a
//     billed figure.
//   - The AI gateway is the truth for what was used: it sees every call
//     made through it, by every key. Its tokens, priced, are the measured
//     estimate.
//   - A shipper's conversations are detail on top of the gateway: what the
//     work was for. They never set a total. Fewer shipped tokens than the
//     gateway measured is work the shippers did not see; more is a finding,
//     shown and never corrected.
//
// The bill is compared with the gateway on its model part when the
// pipeline writes one (the rest, such as an assistant product billed apart, makes no call
// through the gateway), else on all of AI. The difference is the gap,
// shown, never spread.

// Reconciliation is the three lenses month by month, oldest first. It
// holds no key: what each key used is GatewayKeys, a detail view.
type Reconciliation struct {
	Months []ReconMonth `json:"months"`
}

// ReconMonth is one month's three lenses.
type ReconMonth struct {
	Period string `json:"period"`
	// Billed is the bill's AI; nil before the pipeline has run the month.
	Billed *float64 `json:"billed,omitempty"`
	// Basis is what the gateway is compared with: the bill's model part,
	// or all of its AI when the pipeline wrote no model part; nil with no
	// bill. BasisIsModels says which.
	Basis         *float64 `json:"basis,omitempty"`
	BasisIsModels bool     `json:"basis_is_models"`
	Invoice       string   `json:"invoice,omitempty"`
	Gateway       Lens     `json:"gateway"`
	Shipped       Lens     `json:"shipped"`
	// Gap is the basis less the gateway's priced estimate; nil with no bill.
	Gap *float64 `json:"gap,omitempty"`
	// Coverage is the share of the gateway's tokens the shippers saw, on
	// the models both name; 0 with no gateway token.
	Coverage float64 `json:"coverage"`
	// Over are the models the shippers posted more tokens of than the
	// gateway measured: a finding.
	Over   []string     `json:"over,omitempty"`
	Models []ReconModel `json:"models"`
}

// Lens is one lens's month: its tokens, and what the priced ones cost at
// API prices. Unpriced are tokens with no model or no price for it.
type Lens struct {
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	Unpriced int64   `json:"unpriced"`
}

// ReconModel is one model's month through the gateway and the shippers.
type ReconModel struct {
	// Model is the bare name; empty for gateway tokens from before the
	// gateway tagged them with the model.
	Model       string  `json:"model"`
	Gateway     int64   `json:"gateway"`
	GatewayCost float64 `json:"gateway_cost"`
	Shipped     int64   `json:"shipped"`
	ShippedCost float64 `json:"shipped_cost"`
	Priced      bool    `json:"priced"`
}

// Reconcile lines the lenses up for every month the gateway read, oldest
// first, with the bill's months and the shippers' months beside them.
func Reconcile(bill *billed.Snapshot, gw *gateway.Snapshot, shipped []agents.MonthModel, prices agents.Table) Reconciliation {
	type pair struct{ gateway, shipped agents.Tokens }
	byMonth := map[string]map[string]*pair{}
	at := func(month, model string) *pair {
		if byMonth[month] == nil {
			byMonth[month] = map[string]*pair{}
		}
		p := byMonth[month][model]
		if p == nil {
			p = &pair{}
			byMonth[month][model] = p
		}
		return p
	}
	if gw != nil {
		for _, r := range gw.Rows {
			if len(r.Day) >= 7 {
				at(r.Day[:7], r.Model).gateway.Add(r.Tokens)
			}
		}
	}
	for _, s := range shipped {
		if byMonth[s.Month] != nil {
			at(s.Month, s.Model).shipped.Add(s.Tokens)
		}
	}
	periods := make([]string, 0, len(byMonth))
	for p := range byMonth {
		periods = append(periods, p)
	}
	sort.Strings(periods)

	out := Reconciliation{Months: []ReconMonth{}}
	for _, period := range periods {
		m := ReconMonth{Period: period, Models: []ReconModel{}}
		var seen int64
		// A month's tokens take the prices in force on its first day.
		first, _ := time.Parse("2006-01", period)
		for model, p := range byMonth[period] {
			rate, priced := prices.Rate(model, first)
			priced = priced && model != ""
			line := ReconModel{Model: model, Gateway: p.gateway.Total(), Shipped: p.shipped.Total(), Priced: priced}
			if priced {
				line.GatewayCost, line.ShippedCost = rate.Cost(p.gateway), rate.Cost(p.shipped)
				m.Gateway.Cost += line.GatewayCost
				m.Shipped.Cost += line.ShippedCost
			} else {
				m.Gateway.Unpriced += line.Gateway
				m.Shipped.Unpriced += line.Shipped
			}
			m.Gateway.Tokens += line.Gateway
			m.Shipped.Tokens += line.Shipped
			if line.Gateway > 0 {
				seen += min(line.Shipped, line.Gateway)
			}
			if line.Shipped > line.Gateway {
				m.Over = append(m.Over, model)
			}
			m.Models = append(m.Models, line)
		}
		sort.Strings(m.Over)
		sort.Slice(m.Models, func(a, b int) bool {
			x, y := m.Models[a], m.Models[b]
			if x.GatewayCost != y.GatewayCost {
				return x.GatewayCost > y.GatewayCost
			}
			if x.Gateway != y.Gateway {
				return x.Gateway > y.Gateway
			}
			return x.Model < y.Model
		})
		if m.Gateway.Tokens > 0 {
			m.Coverage = float64(seen) / float64(m.Gateway.Tokens)
		}
		if b := bill.Month(period); b != nil {
			ai := b.AI
			m.Billed, m.Invoice = &ai, b.Invoice
			basis := ai
			if b.AIModels != nil {
				basis, m.BasisIsModels = *b.AIModels, true
			}
			gap := basis - m.Gateway.Cost
			m.Basis, m.Gap = &basis, &gap
		}
		out.Months = append(out.Months, m)
	}
	return out
}

// KeyLine is what one gateway key used in a month: its tokens, their cost
// at API prices, and, once the month is billed, its share of the bill's
// basis in proportion to that cost. It names a person or an app, so it is
// detail.
type KeyLine struct {
	Key     string  `json:"key"`
	Product string  `json:"product"`
	Tokens  int64   `json:"tokens"`
	Cost    float64 `json:"cost"`
	// Unpriced are tokens with no model or no price for it.
	Unpriced int64 `json:"unpriced"`
	// Billed is the key's share of the bill's basis; nil before the month
	// is billed or when nothing in it is priced.
	Billed *float64 `json:"billed,omitempty"`
}

// GatewayKeys is what every key used in month (such as 2026-10), the
// costliest first. basis is the bill's basis for the month, or nil.
func GatewayKeys(gw *gateway.Snapshot, month string, prices agents.Table, basis *float64) []KeyLine {
	out := []KeyLine{}
	if gw == nil {
		return out
	}
	by := map[[2]string]*KeyLine{}
	var total float64
	for _, r := range gw.Rows {
		if len(r.Day) < 7 || r.Day[:7] != month {
			continue
		}
		k := [2]string{r.Key, r.Product}
		line := by[k]
		if line == nil {
			line = &KeyLine{Key: r.Key, Product: r.Product}
			by[k] = line
		}
		line.Tokens += r.Total()
		day, _ := time.Parse(time.DateOnly, r.Day)
		if rate, ok := prices.Rate(r.Model, day); ok && r.Model != "" {
			c := rate.Cost(r.Tokens)
			line.Cost += c
			total += c
		} else {
			line.Unpriced += r.Total()
		}
	}
	for _, line := range by {
		if basis != nil && total > 0 {
			share := *basis * line.Cost / total
			line.Billed = &share
		}
		out = append(out, *line)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Cost != out[b].Cost {
			return out[a].Cost > out[b].Cost
		}
		if out[a].Tokens != out[b].Tokens {
			return out[a].Tokens > out[b].Tokens
		}
		return out[a].Key < out[b].Key
	})
	return out
}
