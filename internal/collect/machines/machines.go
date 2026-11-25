// Package machines right-sizes servers, virtual machines most of all, from
// what each reports to Prometheus: memory used against its total, and CPU
// busy, with a core count where node-exporter gives one. CPU's use is its
// 95th percentile; memory's is its peak, since a machine out of memory
// kills. A machine's size is fit when it is under twice its use and its
// use stays under 90% of it; the size that fits is that use plus headroom,
// and never less than what the Kubernetes pods on it ask for.
package machines

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/azure"
	"github.com/orchard-suite/tally/internal/collect/kubernetes"
	"github.com/orchard-suite/tally/internal/collect/promapi"
)

// Verdicts.
const (
	Over  = "over"
	Under = "under"
)

// Units a measure is in.
const (
	Cores   = "cores"
	Bytes   = "bytes"
	Percent = "percent"
)

// Measure is one resource of one machine.
type Measure struct {
	// Size is what the machine has: cores or bytes, or 100 for CPU as a
	// percent when its core count is unknown.
	Size float64 `json:"size"`
	// P95 is its use at the 95th percentile over the window, in the same unit.
	P95 float64 `json:"p95"`
	// Peak is the most it used over the window (memory only).
	Peak float64 `json:"peak,omitempty"`
	// Requested is what the Kubernetes pods on it ask for, with what the
	// node holds back for itself, when the machine is a node.
	Requested float64 `json:"requested,omitempty"`
	// Recommended is the size that fits: the use plus headroom, never under
	// Requested, whole cores for CPU; 0 for CPU in percent, which names no
	// size.
	Recommended float64 `json:"recommended"`
	Unit        string  `json:"unit"`
	// Verdict is over, under, or empty when the size fits.
	Verdict string `json:"verdict,omitempty"`
}

// Machine is one server's sizing.
type Machine struct {
	Host   string  `json:"host"`
	CPU    Measure `json:"cpu"`
	Memory Measure `json:"memory"`
}

// Snapshot is one round's machines.
type Snapshot struct {
	At       time.Time `json:"at"`
	Window   string    `json:"window"`
	Machines []Machine `json:"machines"`
}

// Fit judges a measure by its use: over when the size is more than twice
// the use and the size that fits is smaller, under when the use reaches
// 90% of the size. The size that fits is the use plus headroom, and never
// less than requested.
func Fit(size, use, requested, headroom float64, unit string) Measure {
	m := Measure{Size: size, P95: use, Unit: unit, Requested: requested, Recommended: math.Max(use*(1+headroom), requested)}
	switch unit {
	case Cores:
		m.Recommended = math.Max(1, math.Ceil(m.Recommended-1e-9))
	case Percent:
		m.Recommended = 0
	}
	switch {
	case size <= 0 || use <= 0:
	case size > 2*use && (unit == Percent || m.Recommended < size):
		m.Verdict = Over
	case use >= 0.9*size:
		m.Verdict = Under
	}
	return m
}

// FitMemory judges memory by its peak, keeping its p95 beside it.
func FitMemory(size, p95, peak, requested, headroom float64) Measure {
	m := Fit(size, peak, requested, headroom, Bytes)
	m.P95, m.Peak = p95, peak
	return m
}

// Answers are the queries' samples a round shapes.
type Answers struct {
	// MemTotal, MemP95, MemPeak, and CPUP95 are by host; CPU in percent.
	MemTotal, MemP95, MemPeak, CPUP95 []promapi.Sample
	// Cores is by nodename.
	Cores []promapi.Sample
	// Requested is what a Kubernetes node's pods ask for and the node
	// holds back, by node and resource.
	Requested []promapi.Sample
}

// Shape builds the machines from the answers. A host in skip is left out.
// A host that is a Kubernetes node is never sized under its pods' asks.
func Shape(a Answers, skip map[string]bool, headroom float64, window string, at time.Time) Snapshot {
	by := func(samples []promapi.Sample, label string) map[string]float64 {
		out := map[string]float64{}
		for _, s := range samples {
			if h := s.Labels[label]; h != "" {
				out[h] = s.Value
			}
		}
		return out
	}
	total, used, peak, busy, count := by(a.MemTotal, "host"), by(a.MemP95, "host"), by(a.MemPeak, "host"), by(a.CPUP95, "host"), by(a.Cores, "nodename")
	asked := map[string]map[string]float64{}
	for _, s := range a.Requested {
		n := s.Labels["node"]
		if asked[n] == nil {
			asked[n] = map[string]float64{}
		}
		asked[n][s.Labels["resource"]] += s.Value
	}
	snap := Snapshot{At: at, Window: window, Machines: []Machine{}}
	for host, t := range total {
		if skip[host] {
			continue
		}
		top := peak[host]
		if top == 0 {
			top = used[host]
		}
		m := Machine{Host: host, Memory: FitMemory(t, used[host], top, asked[host]["memory"], headroom)}
		if n := count[host]; n > 0 {
			m.CPU = Fit(n, n*busy[host]/100, asked[host]["cpu"], headroom, Cores)
		} else {
			m.CPU = Fit(100, busy[host], 0, headroom, Percent)
		}
		snap.Machines = append(snap.Machines, m)
	}
	sort.Slice(snap.Machines, func(a, b int) bool { return snap.Machines[a].Host < snap.Machines[b].Host })
	return snap
}

// The queries. The device vitals carry a host label; node-exporter's core
// count is joined to the host's name through node_uname_info. A Kubernetes
// node is the host of its name; what it asks for is its running and
// pending pods' requests and what it holds back from pods.
const (
	qMemTotal = `max by (host) (device_memory_total_bytes{role="server"})`
	qMemP95   = `max by (host) (quantile_over_time(0.95, device_memory_used_bytes{role="server"}[%s]))`
	qMemPeak  = `max by (host) (max_over_time(device_memory_used_bytes{role="server"}[%s]))`
	qCPUP95   = `max by (host) (quantile_over_time(0.95, device_cpu_busy_percent{role="server"}[%s]))`
	qCores    = `max by (nodename) (count by (instance) (node_cpu_seconds_total{mode="idle"}) * on (instance) group_left (nodename) max by (instance, nodename) (node_uname_info))`
)

// qRequested is each node's asks: its running and pending pods' requests,
// and apart from them, what it holds back from pods. Shape adds the two.
var qRequested = `sum by (node, resource) ((` + kubernetes.Resources("requests", "", "cpu") + ` or ` + kubernetes.Resources("requests", "", "memory") + `) and on (namespace, pod) (max by (namespace, pod) (kube_pod_status_phase{phase=~"Running|Pending"}) == 1))` +
	` or label_replace(sum by (node, resource) (kube_node_status_capacity{resource=~"cpu|memory"} - kube_node_status_allocatable{resource=~"cpu|memory"}), "part", "reserved", "", "")`

// Collector reads the machines every interval.
type Collector struct {
	prom     *promapi.Client
	log      *slog.Logger
	inst     *collect.Instruments
	skip     map[string]bool
	window   string
	headroom float64
	interval time.Duration
	latest   atomic.Pointer[Snapshot]
}

// FromEnv turns the collector on with PROMETHEUS_URL and MACHINES=on.
// MACHINES_SKIP names hosts to leave out, such as physical hosts and
// people's own machines; MACHINES_WINDOW (7d) and K8S_HEADROOM (0.3) set the rule.
func FromEnv(log *slog.Logger, inst *collect.Instruments) (*Collector, error) {
	url := os.Getenv("PROMETHEUS_URL")
	if url == "" || os.Getenv("MACHINES") != "on" {
		log.Info("machines collector off: PROMETHEUS_URL and MACHINES=on are not both set")
		return nil, nil
	}
	headroom := 0.3
	if v := os.Getenv("K8S_HEADROOM"); v != "" {
		if _, err := fmt.Sscanf(v, "%g", &headroom); err != nil || headroom < 0 {
			return nil, fmt.Errorf("K8S_HEADROOM must be a fraction such as 0.3, got %q", v)
		}
	}
	window := os.Getenv("MACHINES_WINDOW")
	if window == "" {
		window = "7d"
	}
	skip := map[string]bool{}
	for _, h := range strings.Split(os.Getenv("MACHINES_SKIP"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			skip[h] = true
		}
	}
	prom, err := azure.Prometheus(url)
	if err != nil {
		return nil, err
	}
	c := &Collector{prom: prom, log: log, inst: inst, skip: skip, window: window, headroom: headroom,
		interval: collect.Interval(os.Getenv("MACHINES_INTERVAL"), 15*time.Minute)}
	return c, c.register()
}

// Name is the collector's name.
func (c *Collector) Name() string { return "machines" }

// Interval is how often a round runs when the last one worked.
func (c *Collector) Interval() time.Duration { return c.interval }

// Latest is the last round's machines, nil before the first round worked.
func (c *Collector) Latest() *Snapshot { return c.latest.Load() }

// Run reads every interval until ctx ends.
func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.round)
}

func (c *Collector) round(ctx context.Context) error {
	var res [6][]promapi.Sample
	for i, q := range []string{qMemTotal, fmt.Sprintf(qMemP95, c.window), fmt.Sprintf(qMemPeak, c.window), fmt.Sprintf(qCPUP95, c.window), qCores, qRequested} {
		s, err := c.prom.Query(ctx, q)
		if err != nil {
			c.inst.Upstream(ctx, "prometheus", collect.Failed)
			return err
		}
		c.inst.Upstream(ctx, "prometheus", collect.OK)
		res[i] = s
	}
	snap := Shape(Answers{MemTotal: res[0], MemP95: res[1], MemPeak: res[2], CPUP95: res[3], Cores: res[4], Requested: res[5]}, c.skip, c.headroom, c.window, time.Now().UTC())
	if len(snap.Machines) == 0 {
		return fmt.Errorf("no server reports its memory")
	}
	c.latest.Store(&snap)
	return nil
}

func (c *Collector) register() error {
	g, err := collect.Meter().Int64ObservableGauge("tally.machines.recommendation", metric.WithDescription("1 for each machine resource whose size does not fit its use, by host, resource (cpu or memory), and kind (over or under)."))
	if err != nil {
		return err
	}
	_, err = collect.Meter().RegisterCallback(func(_ context.Context, o metric.Observer) error {
		snap := c.Latest()
		if snap == nil {
			return nil
		}
		for _, m := range snap.Machines {
			for resource, ms := range map[string]Measure{"cpu": m.CPU, "memory": m.Memory} {
				if ms.Verdict != "" {
					o.ObserveInt64(g, 1, metric.WithAttributes(attribute.String("host", m.Host), attribute.String("resource", resource), attribute.String("kind", ms.Verdict)))
				}
			}
		}
		return nil
	}, g)
	return err
}
