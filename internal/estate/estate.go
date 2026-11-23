// Package estate adds the collectors up: cost per month by category, each
// namespace's share of the cluster's power, and the right-sizing.
package estate

import (
	"context"
	"math"
	"sort"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/orchard-suite/tally/internal/agents"
	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/actions"
	"github.com/orchard-suite/tally/internal/collect/advisor"
	"github.com/orchard-suite/tally/internal/collect/aispend"
	"github.com/orchard-suite/tally/internal/collect/azure"
	"github.com/orchard-suite/tally/internal/collect/billed"
	"github.com/orchard-suite/tally/internal/collect/gateway"
	"github.com/orchard-suite/tally/internal/collect/kubernetes"
	"github.com/orchard-suite/tally/internal/collect/machines"
	"github.com/orchard-suite/tally/internal/collect/power"
	"github.com/orchard-suite/tally/internal/collect/runners"
)

// HoursPerMonth turns an hourly cost into a monthly one.
const HoursPerMonth = 730

// Categories.
const (
	Power = "power"
	Cloud = "cloud"
	AI    = "ai"
	// Builds is what hosted CI runners owe; self-hosted runners are already
	// in power or cloud, where their pool's cost is.
	Builds = "ci"
)

// Sources are the collectors' latest snapshots; a source that is off is nil.
type Sources struct {
	Power     func() *power.Snapshot
	Workloads func() *kubernetes.Snapshot
	Cloud     func() *azure.Snapshot
	AI        func() *aispend.Snapshot
	// Agents is agent work billed by the token, nil when a plan pays for it.
	Agents func() *agents.Spend
	// Machines is each server's CPU and memory against its use.
	Machines func() *machines.Snapshot
	// Advisor is Azure Advisor's right-sizing advice for VMs.
	Advisor func() *advisor.Snapshot
	// Runners is self-hosted CI runners: each repository's job pods
	// and the pool they run in.
	Runners func() *runners.Snapshot
	// Actions is GitHub's bill for Actions, by repository.
	Actions func() *actions.Snapshot
	// Billed is the bill as the billing pipeline wrote it, month by month.
	// When it has a month, it is the truth for cloud and AI.
	Billed func() *billed.Snapshot
	// Gateway is what the AI gateway measured, by day, key, and model: the
	// truth for how much AI was used.
	Gateway func() *gateway.Snapshot
	// Shipped is the agent work machines posted, by month and model, and
	// Prices the table it and the gateway's tokens are priced at.
	Shipped func() []agents.MonthModel
	Prices  func() agents.Prices
	// Zone is where calendar months begin; nil is UTC.
	Zone *time.Location
	// ClusterHosts are the powered hosts whose cost the namespaces share.
	// Empty means every host.
	ClusterHosts []string
}

// Costs is the board and GET /v1/costs.
type Costs struct {
	GeneratedAt   time.Time         `json:"generated_at"`
	Currency      string            `json:"currency"`
	TotalPerMonth float64           `json:"total_per_month"`
	Categories    []Category        `json:"categories"`
	Hosts         []Host            `json:"hosts"`
	Namespaces    []Namespace       `json:"namespaces"`
	AI            []aispend.Account `json:"ai"`
	Cloud         []CloudLine       `json:"cloud"`
	Rightsizing   Rightsizing       `json:"rightsizing"`
	// CI is what builds cost by repository, when runners or a forge's bill
	// are read.
	CI *CI `json:"ci,omitempty"`
	// AgentSpend is agent work billed by the token: part of the ai
	// category, unless the bill is read, when it is what the machines
	// collected beside the billed truth.
	AgentSpend *agents.Spend `json:"agent_spend,omitempty"`
	// Billed is the newest billed months, oldest first, with AI by program.
	Billed []billed.Month `json:"billed,omitempty"`
	// Reconciliation is AI through three lenses, month by month, when the
	// gateway is read: the bill, the gateway, and the shippers.
	Reconciliation *Reconciliation `json:"reconciliation,omitempty"`
	// Headline is what the board's header shows: the newest billed
	// months, or this month so far and last month, or the rate a month.
	Headline  []Figure `json:"headline"`
	Workloads int      `json:"workloads"`
	Window    string   `json:"window,omitempty"`
}

// Figure is one labeled amount.
type Figure struct {
	Label  string  `json:"label"`
	Amount float64 `json:"amount"`
	// Note says where the amount comes from, such as billed.
	Note string `json:"note,omitempty"`
}

// BilledMonths is how many of the newest billed months the board shows.
const BilledMonths = 6

// Category is one of power, cloud, or ai, per month. Cloud's PerMonth is
// the last 30 days, a run rate that means the same on the 1st as on the
// 28th; MonthToDate and Previous are the calendar months beside it. AI is
// the plans' prices, plus agent work billed by the token over the last 30
// days, with its calendar months beside it.
type Category struct {
	Category string  `json:"category"`
	PerMonth float64 `json:"per_month"`
	// MonthToDate is this calendar month so far, where the source knows it (cloud).
	MonthToDate *float64 `json:"month_to_date,omitempty"`
	// Previous is last month's figure where the source knows it (cloud).
	Previous *float64 `json:"previous,omitempty"`
	// Label names PerMonth when it is one billed month, such as
	// "September 2026, billed".
	Label string `json:"label,omitempty"`
	On    bool   `json:"on"`
}

// Host is a powered host and what it costs a month.
type Host struct {
	power.Host
	PerMonth float64 `json:"per_month"`
	Cluster  bool    `json:"cluster"`
}

// Namespace is a namespace's share of the cluster's power cost.
type Namespace struct {
	Namespace     string  `json:"namespace"`
	CPURequest    float64 `json:"cpu_request_cores"`
	MemoryRequest float64 `json:"memory_request_bytes"`
	Share         float64 `json:"share"`
	PerMonth      float64 `json:"per_month"`
}

// CloudLine is one resource group and service: the last 30 days, this
// month so far, and last month.
type CloudLine struct {
	Provider string  `json:"provider"`
	Account  string  `json:"account"`
	Group    string  `json:"group"`
	Service  string  `json:"service"`
	Currency string  `json:"currency"`
	Trailing float64 `json:"trailing"`
	Current  float64 `json:"current"`
	Previous float64 `json:"previous"`
}

// Rightsizing is everything whose size does not fit its use: the
// cluster's workloads, the servers, and the VMs Azure Advisor reads.
type Rightsizing struct {
	// Window is what the workloads' use is read over.
	Window string `json:"window,omitempty"`
	// Cluster is the cluster's room and what the changes do to it, when
	// the workloads are read.
	Cluster *Cluster `json:"cluster,omitempty"`
	// Workloads are the workloads with a container that does not fit, the
	// most urgent verdict first, then the most each change frees or adds.
	Workloads []WorkloadFit    `json:"workloads"`
	Machines  []Recommendation `json:"machines"`
	Advisor   []Recommendation `json:"advisor"`
}

// Cluster is the cluster's room and what the workloads' changes do to it,
// workloads the platform sizes left out.
type Cluster struct {
	// Allocatable is what the nodes offer pods.
	Allocatable kubernetes.Capacity `json:"allocatable"`
	// Requested is what the running pods ask for now.
	Requested kubernetes.Capacity `json:"requested"`
	// Frees is what shrinking every over request frees now.
	Frees kubernetes.Capacity `json:"frees"`
	// Adds is what fixing every risk, under, and unset request adds now.
	Adds kubernetes.Capacity `json:"adds"`
	// PerMonth is what the cluster costs a month, when its hosts are priced.
	PerMonth *float64 `json:"per_month,omitempty"`
}

// WorkloadFit is one workload with a container that does not fit.
type WorkloadFit struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Kind      string `json:"kind"`
	Pods      int    `json:"pods"`
	Managed   bool   `json:"managed,omitempty"`
	// Verdict is its most urgent container verdict.
	Verdict string `json:"verdict"`
	// Frees is what its changes free now, across its pods: negative where
	// they add.
	Frees kubernetes.Capacity `json:"frees"`
	// Share is Frees as a part of the cluster's room, half by CPU and half
	// by memory, the split that shares the cluster's cost.
	Share float64 `json:"share"`
	// PerMonth is Share of the cluster's cost a month, when it is priced.
	PerMonth   *float64               `json:"per_month,omitempty"`
	Containers []kubernetes.Container `json:"containers"`
}

// Recommendation is a server's CPU or memory, or a VM Azure Advisor
// advises resizing, whose size does not fit its use.
type Recommendation struct {
	// Source is where the advice comes from: machines or advisor.
	Source string `json:"source"`
	// Namespace is an Azure resource group; empty for a machine.
	Namespace string `json:"namespace"`
	// Workload is a host or a VM.
	Workload string `json:"workload"`
	Kind     string `json:"kind"`
	// Resource is cpu, memory, or size (a VM's whole size, from Advisor).
	Resource string `json:"resource"`
	// Verdict is over, under, or shutdown.
	Verdict string `json:"verdict"`
	// Request is the size it has now.
	Request float64 `json:"request"`
	// P95 is its use at the 95th percentile; for Advisor's advice, CPU in
	// percent of the size.
	P95 float64 `json:"p95"`
	// Peak is the most memory it used, which memory is sized to.
	Peak float64 `json:"peak,omitempty"`
	// Requested is what the Kubernetes pods on a node ask for, with what
	// the node holds back: the size that fits never drops under it.
	Requested   float64 `json:"requested,omitempty"`
	Recommended float64 `json:"recommended"`
	// Unit is what Request, P95, Peak, Requested, and Recommended are in:
	// cores, bytes, or percent; empty for Advisor's advice, which names
	// sizes instead.
	Unit string `json:"unit,omitempty"`
	// MemoryP95 is Advisor's memory use at the 95th percentile, in percent
	// of the size.
	MemoryP95 float64 `json:"memory_p95,omitempty"`
	// From and To are the VM sizes Advisor names; To is empty for a shutdown.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// SavingsPerYear is what Advisor says the change saves, in dollars.
	SavingsPerYear float64 `json:"savings_per_year,omitempty"`
	// Change is the kind of change Advisor advises: shrink, burstable,
	// family, or shutdown.
	Change string `json:"change,omitempty"`
	// Decide marks advice whose kind of change the savings do not count
	// (ADVISOR_COUNT): it waits on a person's decision.
	Decide bool `json:"decide,omitempty"`
}

// Allocate shares the cluster's monthly power cost among namespaces by their
// requested CPU and memory: half the cost by CPU share, half by memory
// share. A namespace that requests nothing gets nothing.
func Allocate(workloads []kubernetes.Workload, clusterPerMonth float64) []Namespace {
	byNS := map[string]*Namespace{}
	var cpuTotal, memTotal float64
	for _, w := range workloads {
		cpu, mem := w.Requests()
		ns, ok := byNS[w.Namespace]
		if !ok {
			ns = &Namespace{Namespace: w.Namespace}
			byNS[w.Namespace] = ns
		}
		ns.CPURequest += cpu
		ns.MemoryRequest += mem
		cpuTotal += cpu
		memTotal += mem
	}
	out := make([]Namespace, 0, len(byNS))
	for _, ns := range byNS {
		var share float64
		if cpuTotal > 0 {
			share += ns.CPURequest / cpuTotal / 2
		}
		if memTotal > 0 {
			share += ns.MemoryRequest / memTotal / 2
		}
		// With only one of the two requested anywhere, that one carries the whole cost.
		if cpuTotal == 0 || memTotal == 0 {
			share *= 2
		}
		ns.Share = share
		ns.PerMonth = share * clusterPerMonth
		out = append(out, *ns)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PerMonth != out[j].PerMonth {
			return out[i].PerMonth > out[j].PerMonth
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out
}

// Workloads lists every workload with a container that does not fit, the
// most urgent first and then the most each frees or adds, and the
// cluster's room. perMonth is the cluster's cost a month, 0 when it is not
// priced.
func Workloads(snap *kubernetes.Snapshot, perMonth float64) ([]WorkloadFit, *Cluster) {
	cl := &Cluster{Allocatable: snap.Allocatable}
	if perMonth > 0 {
		cl.PerMonth = &perMonth
	}
	share := func(c kubernetes.Capacity) float64 {
		var s float64
		if snap.Allocatable.CPU > 0 {
			s += c.CPU / snap.Allocatable.CPU / 2
		}
		if snap.Allocatable.Memory > 0 {
			s += c.Memory / snap.Allocatable.Memory / 2
		}
		return s
	}
	out := []WorkloadFit{}
	for _, w := range snap.Workloads {
		cpu, mem := w.Requests()
		if !w.Managed {
			cl.Requested.CPU += cpu
			cl.Requested.Memory += mem
		}
		f := WorkloadFit{Namespace: w.Namespace, Workload: w.Workload, Kind: w.Kind, Pods: w.Pods, Managed: w.Managed, Containers: w.Containers}
		rank := kubernetes.Rank("")
		for _, c := range w.Containers {
			for _, m := range []struct {
				m   kubernetes.Measure
				add *float64
			}{{c.CPU, &f.Frees.CPU}, {c.Memory, &f.Frees.Memory}} {
				if m.m.Verdict == "" {
					continue
				}
				if r := kubernetes.Rank(m.m.Verdict); r < rank {
					rank = r
				}
				*m.add += (m.m.Request - m.m.Recommended) * float64(w.Pods)
			}
		}
		if rank == kubernetes.Rank("") {
			continue
		}
		f.Verdict = kubernetes.Verdicts[rank]
		f.Share = share(f.Frees)
		if perMonth > 0 {
			v := f.Share * perMonth
			f.PerMonth = &v
		}
		if !w.Managed {
			for _, part := range []struct{ v, frees, adds *float64 }{{&f.Frees.CPU, &cl.Frees.CPU, &cl.Adds.CPU}, {&f.Frees.Memory, &cl.Frees.Memory, &cl.Adds.Memory}} {
				if *part.v > 0 {
					*part.frees += *part.v
				} else {
					*part.adds -= *part.v
				}
			}
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := kubernetes.Rank(a.Verdict), kubernetes.Rank(b.Verdict); ra != rb {
			return ra < rb
		}
		if sa, sb := math.Abs(a.Share), math.Abs(b.Share); sa != sb {
			return sa > sb
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Workload < b.Workload
	})
	return out, cl
}

// MachineSizing lists every machine resource whose size does not fit its
// use, the furthest first.
func MachineSizing(snap *machines.Snapshot) []Recommendation {
	var out []Recommendation
	if snap == nil {
		return out
	}
	for _, m := range snap.Machines {
		for _, r := range []struct {
			name string
			m    machines.Measure
		}{{"cpu", m.CPU}, {"memory", m.Memory}} {
			if r.m.Verdict == "" {
				continue
			}
			out = append(out, Recommendation{Source: "machines", Workload: m.Host, Kind: "machine", Resource: r.name, Verdict: r.m.Verdict,
				Request: r.m.Size, P95: r.m.P95, Peak: r.m.Peak, Requested: r.m.Requested, Recommended: r.m.Recommended, Unit: r.m.Unit})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := gap(out[i]), gap(out[j])
		if a != b {
			return a > b
		}
		if out[i].Workload != out[j].Workload {
			return out[i].Workload < out[j].Workload
		}
		return out[i].Resource < out[j].Resource
	})
	return out
}

// AdvisorSizing lists Azure Advisor's advice, the largest saving first.
func AdvisorSizing(snap *advisor.Snapshot) []Recommendation {
	var out []Recommendation
	if snap == nil {
		return out
	}
	for _, a := range snap.Advice {
		verdict := "over"
		if a.Kind == advisor.Shutdown {
			verdict = "shutdown"
		}
		out = append(out, Recommendation{Source: "advisor", Namespace: a.ResourceGroup, Workload: a.VM, Kind: "vm", Resource: "size", Verdict: verdict,
			From: a.From, To: a.To, SavingsPerYear: a.SavingsPerYear, P95: a.CPUP95, MemoryP95: a.MemoryP95, Change: a.Change, Decide: !a.Counted})
	}
	return out
}

// gap is how far a size is from fitting its use, as a ratio above 1.
// Memory's use is its peak.
func gap(r Recommendation) float64 {
	use := r.P95
	if r.Peak > 0 {
		use = r.Peak
	}
	if r.Request == 0 || use == 0 {
		return 1e9
	}
	if r.Request > use {
		return r.Request / use
	}
	return use / r.Request
}

// Build adds everything up at now.
func Build(s Sources, now time.Time) Costs {
	c := Costs{GeneratedAt: now.UTC(), Currency: "USD"}
	cluster := map[string]bool{}
	for _, h := range s.ClusterHosts {
		cluster[h] = true
	}

	var powerMonth, clusterMonth float64
	powerOn := false
	if s.Power != nil {
		if snap := s.Power(); snap != nil {
			powerOn = true
			for _, h := range snap.Hosts {
				month := h.CostPerHour * HoursPerMonth
				in := len(cluster) == 0 || cluster[h.Host]
				c.Hosts = append(c.Hosts, Host{Host: h, PerMonth: month, Cluster: in})
				powerMonth += month
				if in {
					clusterMonth += month
				}
			}
		}
	}

	var cloudTrailing, cloudNow, cloudPrev float64
	cloudOn := false
	if s.Cloud != nil {
		if snap := s.Cloud(); snap != nil {
			cloudOn = true
			lines := map[string]*CloudLine{}
			var order []string
			for _, r := range snap.Costs {
				key := r.Provider + "|" + r.Account + "|" + r.Group + "|" + r.Service + "|" + r.Currency
				l, ok := lines[key]
				if !ok {
					l = &CloudLine{Provider: r.Provider, Account: r.Account, Group: r.Group, Service: r.Service, Currency: r.Currency}
					lines[key] = l
					order = append(order, key)
				}
				switch r.Period {
				case azure.Trailing:
					l.Trailing += r.Cost
					cloudTrailing += r.Cost
				case azure.Current:
					l.Current += r.Cost
					cloudNow += r.Cost
				default:
					l.Previous += r.Cost
					cloudPrev += r.Cost
				}
				if r.Currency != "" {
					c.Currency = r.Currency
				}
			}
			for _, k := range order {
				c.Cloud = append(c.Cloud, *lines[k])
			}
			sort.SliceStable(c.Cloud, func(i, j int) bool { return c.Cloud[i].Trailing > c.Cloud[j].Trailing })
		}
	}

	var aiMonth float64
	aiOn := false
	if s.AI != nil {
		if snap := s.AI(); snap != nil {
			aiOn = true
			c.AI = snap.Accounts
			for _, a := range snap.Accounts {
				aiMonth += a.CostPerMonth
			}
		}
	}
	var agentSpend *agents.Spend
	if s.Agents != nil {
		if agentSpend = s.Agents(); agentSpend != nil {
			aiOn = true
			aiMonth += agentSpend.Trailing
			c.AgentSpend = agentSpend
		}
	}

	c.Rightsizing = Rightsizing{Workloads: []WorkloadFit{}, Machines: []Recommendation{}, Advisor: []Recommendation{}}
	if s.Workloads != nil {
		if snap := s.Workloads(); snap != nil {
			c.Workloads = len(snap.Workloads)
			c.Window = snap.Window
			c.Namespaces = Allocate(snap.Workloads, clusterMonth)
			c.Rightsizing.Window = snap.Window
			c.Rightsizing.Workloads, c.Rightsizing.Cluster = Workloads(snap, clusterMonth)
		}
	}

	var run *runners.Snapshot
	if s.Runners != nil {
		run = s.Runners()
	}
	var bill *actions.Snapshot
	if s.Actions != nil {
		bill = s.Actions()
	}
	c.CI = BuildCI(run, bill, clusterMonth, powerOn, now)

	c.Categories = []Category{
		{Category: Power, PerMonth: powerMonth, On: powerOn},
		{Category: Cloud, PerMonth: cloudTrailing, On: cloudOn},
		{Category: AI, PerMonth: aiMonth, On: aiOn},
	}
	var ciMonth float64
	if bill != nil {
		trailing, current, previous := HostedMonths(bill, now)
		ciMonth = trailing
		c.Categories = append(c.Categories, Category{Category: Builds, PerMonth: trailing, MonthToDate: &current, Previous: &previous, On: true})
	}
	if cloudOn {
		now, prev := cloudNow, cloudPrev
		c.Categories[1].MonthToDate = &now
		c.Categories[1].Previous = &prev
	}
	if agentSpend != nil {
		now, prev := agentSpend.MonthToDate, agentSpend.Previous
		c.Categories[2].MonthToDate = &now
		c.Categories[2].Previous = &prev
	}

	if s.Machines != nil {
		c.Rightsizing.Machines = MachineSizing(s.Machines())
	}
	if s.Advisor != nil {
		c.Rightsizing.Advisor = AdvisorSizing(s.Advisor())
	}

	var snap *billed.Snapshot
	if s.Billed != nil {
		snap = s.Billed()
	}
	if latest := snap.Latest(); latest != nil {
		// The bill is the truth: cloud is its usage less AI, and AI is its
		// AI. Collected agent work stays beside it, never added in.
		zone := s.Zone
		if zone == nil {
			zone = time.UTC
		}
		at := now.In(zone)
		this := at.Format("2006-01")
		last := time.Date(at.Year(), at.Month()-1, 1, 0, 0, 0, 0, zone).Format("2006-01")
		label := monthName(latest.Period) + ", billed"
		c.Categories[1] = Category{Category: Cloud, PerMonth: latest.Usage - latest.AI, Label: label, On: true,
			MonthToDate: billedPart(snap, this, func(m *billed.Month) float64 { return m.Usage - m.AI }),
			Previous:    billedPart(snap, last, func(m *billed.Month) float64 { return m.Usage - m.AI })}
		c.Categories[2] = Category{Category: AI, PerMonth: latest.AI, Label: label, On: true,
			MonthToDate: billedPart(snap, this, func(m *billed.Month) float64 { return m.AI }),
			Previous:    billedPart(snap, last, func(m *billed.Month) float64 { return m.AI })}
		cloudTrailing, aiMonth = latest.Usage-latest.AI, latest.AI
		from := max(0, len(snap.Months)-BilledMonths)
		c.Billed = append([]billed.Month(nil), snap.Months[from:]...)
		for i := len(snap.Months) - 1; i >= 0 && i >= len(snap.Months)-2; i-- {
			m := snap.Months[i]
			c.Headline = append(c.Headline, Figure{Label: monthName(m.Period), Amount: powerMonth + m.Usage + hostedIn(bill, m.Period), Note: "billed"})
		}
	}
	if gw := s.gatewaySnapshot(); gw != nil {
		r := Reconcile(snap, gw, s.shipped(), s.prices())
		c.Reconciliation = &r
	}
	c.TotalPerMonth = powerMonth + cloudTrailing + aiMonth + ciMonth
	if c.Headline == nil {
		c.Headline = headline(c.Categories, c.TotalPerMonth)
	}
	return c
}

func (s Sources) gatewaySnapshot() *gateway.Snapshot {
	if s.Gateway == nil {
		return nil
	}
	return s.Gateway()
}

func (s Sources) shipped() []agents.MonthModel {
	if s.Shipped == nil {
		return nil
	}
	return s.Shipped()
}

func (s Sources) prices() agents.Prices {
	if s.Prices == nil {
		return nil
	}
	return s.Prices()
}

// Keys is what every gateway key used in month (such as 2026-10), with its
// share of the bill's basis once the month is billed. It names people:
// only a detail view shows it.
func (s Sources) Keys(month string) []KeyLine {
	var basis *float64
	if gw := s.gatewaySnapshot(); gw != nil {
		var bill *billed.Snapshot
		if s.Billed != nil {
			bill = s.Billed()
		}
		for _, m := range Reconcile(bill, gw, nil, s.prices()).Months {
			if m.Period == month {
				basis = m.Basis
			}
		}
	}
	return GatewayKeys(s.gatewaySnapshot(), month, s.prices(), basis)
}

// hostedIn is the hosted runners' net bill for one month, such as
// 2026-09, or 0 when it is not read.
func hostedIn(a *actions.Snapshot, period string) float64 {
	if a == nil {
		return 0
	}
	var v float64
	for _, it := range a.Items {
		if it.Date.Format("2006-01") == period {
			v += it.Net
		}
	}
	return v
}

// headline is the header's figures without a bill: this month so far and
// last month when every category that is on knows its calendar months, or
// else the rate a month.
func headline(cats []Category, perMonth float64) []Figure {
	var now, prev float64
	on := 0
	for _, k := range cats {
		if !k.On {
			continue
		}
		on++
		if k.MonthToDate == nil || k.Previous == nil {
			return []Figure{{Label: "Estate per month", Amount: perMonth}}
		}
		now += *k.MonthToDate
		prev += *k.Previous
	}
	if on == 0 {
		return []Figure{{Label: "Estate per month", Amount: perMonth}}
	}
	return []Figure{{Label: "This month so far", Amount: now}, {Label: "Last month", Amount: prev}}
}

// billedPart is one part of a billed month, or nil when the month is not
// billed yet.
func billedPart(s *billed.Snapshot, period string, part func(*billed.Month) float64) *float64 {
	m := s.Month(period)
	if m == nil {
		return nil
	}
	v := part(m)
	return &v
}

// monthName is a period such as 2026-09 as "September 2026".
func monthName(period string) string {
	t, err := time.Parse("2006-01", period)
	if err != nil {
		return period
	}
	return t.Format("January 2006")
}

// Register exports tally.estate.cost_per_month by category and
// tally.estate.namespace_cost_per_month by namespace, built at each export.
func Register(s Sources) error {
	m := collect.Meter()
	cat, err := m.Float64ObservableGauge("tally.estate.cost_per_month", metric.WithDescription("The estate's cost a month by category: power (hosts' draw at the electricity price), cloud (the last 30 days), ai (subscription prices, and agent work billed by the token over the last 30 days), ci (hosted CI runners' bill, net, over the last 30 days)."))
	if err != nil {
		return err
	}
	ns, err := m.Float64ObservableGauge("tally.estate.namespace_cost_per_month", metric.WithDescription("A namespace's share of the cluster hosts' power cost a month, by requested CPU and memory."))
	if err != nil {
		return err
	}
	ciCost, err := m.Float64ObservableGauge("tally.ci.cost_per_month", metric.WithDescription("A repository's builds a month at the CI window's rate, by forge and runner: self-hosted (its share of the self-hosted runners' pool, by requested CPU and memory) or hosted (the forge's bill, net)."))
	if err != nil {
		return err
	}
	ciMinutes, err := m.Float64ObservableGauge("tally.ci.minutes", metric.WithUnit("min"), metric.WithDescription("A repository's build minutes over the CI window, by forge and runner: self-hosted (its job pods' run time) or hosted (the forge's billed minutes)."))
	if err != nil {
		return err
	}
	ciPool, err := m.Float64ObservableGauge("tally.ci.pool_cost_per_month", metric.WithDescription("The self-hosted runners' pool a month at the CI window's rate: its own nodes' hours at their price, or its namespace's share of the cluster hosts' cost."))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		c := Build(s, time.Now())
		if ci := c.CI; ci != nil {
			if ci.Pool != nil && ci.Pool.PerMonth != nil {
				o.ObserveFloat64(ciPool, *ci.Pool.PerMonth, metric.WithAttributes(attribute.String("namespace", ci.Pool.Namespace)))
			}
			for _, r := range ci.Repositories {
				who := func(runner string) metric.MeasurementOption {
					return metric.WithAttributes(attribute.String("forge", r.Forge), attribute.String("repository", r.Repository), attribute.String("runner", runner))
				}
				if ci.Pool != nil {
					o.ObserveFloat64(ciMinutes, r.SelfHostedMinutes, who("self-hosted"))
					if r.SelfHostedPerMonth != nil {
						o.ObserveFloat64(ciCost, *r.SelfHostedPerMonth, who("self-hosted"))
					}
				}
				if ci.Hosted && r.Forge == "github" {
					o.ObserveFloat64(ciMinutes, r.HostedMinutes, who("hosted"))
					o.ObserveFloat64(ciCost, r.HostedPerMonth, who("hosted"))
				}
			}
		}
		for _, k := range c.Categories {
			if k.On {
				o.ObserveFloat64(cat, k.PerMonth, metric.WithAttributes(attribute.String("category", k.Category)))
			}
		}
		for _, n := range c.Namespaces {
			o.ObserveFloat64(ns, n.PerMonth, metric.WithAttributes(attribute.String("namespace", n.Namespace)))
		}
		return nil
	}, cat, ns, ciCost, ciMinutes, ciPool)
	return err
}
