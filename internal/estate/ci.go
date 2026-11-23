package estate

import (
	"sort"
	"strings"
	"time"

	"github.com/orchard-suite/tally/internal/collect/actions"
	"github.com/orchard-suite/tally/internal/collect/runners"
)

// CI is what builds cost, by repository: on self-hosted runners, at
// their pool's cost, and on a forge's hosted runners, at its bill.
type CI struct {
	// Window is the span minutes are counted over, such as 7d. A month is
	// that span's rate held for 730 hours.
	Window string `json:"window"`
	// Pool is the self-hosted runners, when they are measured.
	Pool *CIPool `json:"pool,omitempty"`
	// Hosted is whether a forge's bill is read; HostedPerMonth is what its
	// hosted runners owe a month at the window's rate, after the plan's
	// included minutes.
	Hosted         bool           `json:"hosted"`
	HostedPerMonth float64        `json:"hosted_per_month"`
	Repositories   []CIRepository `json:"repositories"`
}

// CIPool is where the self-hosted job pods run and what it costs.
type CIPool struct {
	Namespace string `json:"namespace"`
	// Nodes names the pool's own nodes, priced by NodeHours; without it
	// the pool is Share of the cluster hosts' cost.
	Nodes     string  `json:"nodes,omitempty"`
	NodeHours float64 `json:"node_hours,omitempty"`
	Share     float64 `json:"share,omitempty"`
	// PerMonth is nil when the pool cannot be priced: it shares the
	// cluster and the hosts' power is not read.
	PerMonth *float64 `json:"per_month"`
}

// CIRepository is one repository's builds.
type CIRepository struct {
	Forge      string `json:"forge"`
	Repository string `json:"repository"`
	// SelfHostedMinutes is its job pods' run time on self-hosted runners over
	// the window; SelfHostedPerMonth its share of the pool's cost, by requested
	// CPU and memory, nil when the pool is not priced.
	SelfHostedMinutes  float64  `json:"self_hosted_minutes"`
	SelfHostedPerMonth *float64 `json:"self_hosted_per_month"`
	// HostedMinutes is its minutes on hosted runners over the window, and
	// HostedPerMonth what they owe a month (net) and list at (gross).
	HostedMinutes       float64 `json:"hosted_minutes"`
	HostedPerMonth      float64 `json:"hosted_per_month"`
	HostedGrossPerMonth float64 `json:"hosted_gross_per_month"`
}

// DefaultCIWindow is the window hosted minutes are counted over when
// self-hosted runners are not measured.
const DefaultCIWindow = 7 * 24

// BuildCI joins the runners and the bill at now. clusterMonth is the
// cluster hosts' cost a month, read when powerOn. It is nil when neither
// source is on or neither has finished a round.
func BuildCI(r *runners.Snapshot, a *actions.Snapshot, clusterMonth float64, powerOn bool, now time.Time) *CI {
	if r == nil && a == nil {
		return nil
	}
	ci := &CI{Window: "7d", Repositories: []CIRepository{}}
	hours := float64(DefaultCIWindow)
	if r != nil {
		ci.Window, hours = r.Window, r.Hours
	}
	scale := HoursPerMonth / hours
	repos := map[string]*CIRepository{}
	get := func(forge, repo string) *CIRepository {
		key := forge + "|" + strings.ToLower(repo)
		x, ok := repos[key]
		if !ok {
			x = &CIRepository{Forge: forge, Repository: repo}
			repos[key] = x
		}
		return x
	}

	if r != nil {
		pool := &CIPool{Namespace: r.Pool.Namespace, Nodes: r.Pool.Nodes, NodeHours: r.Pool.NodeHours}
		switch {
		case r.Pool.Nodes != "":
			v := r.Pool.NodeHours * r.Pool.PricePerHour * scale
			pool.PerMonth = &v
		default:
			pool.Share = r.Pool.CPUShare/2 + r.Pool.MemoryShare/2
			if powerOn {
				v := pool.Share * clusterMonth
				pool.PerMonth = &v
			}
		}
		ci.Pool = pool
		var cpu, mem float64
		for _, u := range r.Usage {
			cpu += u.CPUSeconds
			mem += u.MemorySeconds
		}
		for _, u := range r.Usage {
			x := get(u.Forge, u.Repository)
			x.SelfHostedMinutes += u.PodSeconds / 60
			if pool.PerMonth != nil {
				v := *pool.PerMonth * weight(u.CPUSeconds, cpu, u.MemorySeconds, mem)
				if x.SelfHostedPerMonth != nil {
					v += *x.SelfHostedPerMonth
				}
				x.SelfHostedPerMonth = &v
			}
		}
	}

	if a != nil {
		ci.Hosted = true
		// Whole days only: the window's days before today.
		today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
		days := int(hours / 24)
		if days < 1 {
			days = 1
		}
		from := today.AddDate(0, 0, -days)
		hostedScale := HoursPerMonth / float64(days*24)
		for _, it := range a.Items {
			if it.Date.Before(from) || !it.Date.Before(today) {
				continue
			}
			ci.HostedPerMonth += it.Net * hostedScale
			if it.Repository == "" {
				continue
			}
			x := get("github", it.Repository)
			x.HostedMinutes += it.Minutes
			x.HostedPerMonth += it.Net * hostedScale
			x.HostedGrossPerMonth += it.Gross * hostedScale
		}
	}

	for _, x := range repos {
		ci.Repositories = append(ci.Repositories, *x)
	}
	sort.Slice(ci.Repositories, func(i, j int) bool {
		a, b := ci.Repositories[i], ci.Repositories[j]
		if ca, cb := a.cost(), b.cost(); ca != cb {
			return ca > cb
		}
		if ma, mb := a.SelfHostedMinutes+a.HostedMinutes, b.SelfHostedMinutes+b.HostedMinutes; ma != mb {
			return ma > mb
		}
		return a.Repository < b.Repository
	})
	return ci
}

func (x CIRepository) cost() float64 {
	v := x.HostedPerMonth
	if x.SelfHostedPerMonth != nil {
		v += *x.SelfHostedPerMonth
	}
	return v
}

// weight is one share of a pool by requested CPU and memory, half each, as
// namespaces share the cluster; with only one requested, that one decides.
func weight(cpu, cpuTotal, mem, memTotal float64) float64 {
	switch {
	case cpuTotal > 0 && memTotal > 0:
		return cpu/cpuTotal/2 + mem/memTotal/2
	case cpuTotal > 0:
		return cpu / cpuTotal
	case memTotal > 0:
		return mem / memTotal
	}
	return 0
}

// HostedMonths is the hosted runners' bill, net: the last 30 whole days,
// this calendar month so far, and last month, in UTC as GitHub bills.
func HostedMonths(a *actions.Snapshot, now time.Time) (trailing, current, previous float64) {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	this := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := this.AddDate(0, -1, 0)
	from := today.AddDate(0, 0, -30)
	for _, it := range a.Items {
		if !it.Date.Before(from) && it.Date.Before(today) {
			trailing += it.Net
		}
		switch {
		case !it.Date.Before(this):
			current += it.Net
		case !it.Date.Before(last):
			previous += it.Net
		}
	}
	return trailing * HoursPerMonth / (30 * 24), current, previous
}
