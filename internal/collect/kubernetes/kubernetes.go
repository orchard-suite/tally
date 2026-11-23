// Package kubernetes reads what each workload's containers ask for, may
// use, and did use from Prometheus, and says where a request is wrong or a
// container is in trouble. It turns on with PROMETHEUS_URL.
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/azure"
	"github.com/orchard-suite/tally/internal/collect/promapi"
)

// Workload is one deployment, statefulset, daemonset, CronJob, runner set,
// or static pod set that runs now: its pods now and each container's
// sizing. Sizes are one pod's, the size a person writes in the manifest.
type Workload struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Kind      string `json:"kind"`
	// Pods is how many of its pods are running or pending now; a CronJob
	// or a runner set between runs has none.
	Pods int `json:"pods"`
	// Managed says the platform, not a person, sets its sizes: a static
	// pod, or a namespace K8S_MANAGED names.
	Managed    bool        `json:"managed,omitempty"`
	Containers []Container `json:"containers"`
}

// Container is one container of a workload's pods.
type Container struct {
	Name   string  `json:"name"`
	CPU    Measure `json:"cpu"`
	Memory Measure `json:"memory"`
}

// Requests is what the workload's pods ask for now, together.
func (w Workload) Requests() (cpu, memory float64) {
	for _, c := range w.Containers {
		cpu += c.CPU.Request
		memory += c.Memory.Request
	}
	return cpu * float64(w.Pods), memory * float64(w.Pods)
}

// Measure is one resource of one container, in cores or bytes, for the
// busiest of its pods.
type Measure struct {
	Request float64 `json:"request"`
	Limit   float64 `json:"limit"`
	P95     float64 `json:"p95"`
	Peak    float64 `json:"peak"`
	// Kills is how many times the container restarted after the kernel
	// killed it for its memory, over the window (memory only).
	Kills float64 `json:"kills,omitempty"`
	// Throttled is the part of its CPU periods the limit held it back in,
	// over the window (CPU only).
	Throttled float64 `json:"throttled,omitempty"`
	// Recommended is the request that fits; 0 when nothing was used.
	Recommended float64 `json:"recommended"`
	// RecommendedLimit is the limit that stops the trouble; 0 keeps the
	// limit as it is.
	RecommendedLimit float64 `json:"recommended_limit,omitempty"`
	// Verdict is risk, under, unset, over, or empty when it fits.
	Verdict string `json:"verdict,omitempty"`
	// Reason says why a risk is one.
	Reason string `json:"reason,omitempty"`
}

// Verdicts, the most urgent first.
const (
	// Risk is a container in trouble now: killed for memory, near its
	// memory limit, or held back by its CPU limit.
	Risk = "risk"
	// Under is a request below what the container uses.
	Under = "under"
	// Unset is a container that uses the resource with no request at all.
	Unset = "unset"
	// Over is a request well above what the container uses.
	Over = "over"
)

// Verdicts are the verdicts, the most urgent first.
var Verdicts = []string{Risk, Under, Unset, Over}

// Rank orders verdicts: lower is more urgent; a fit is last.
func Rank(verdict string) int {
	for i, v := range Verdicts {
		if v == verdict {
			return i
		}
	}
	return len(Verdicts)
}

// Rule is the right-sizing rule. CPU is sized to its p95: a busy moment
// past it only slows the container. Memory is sized to its peak: past its
// limit the container is killed.
type Rule struct {
	// Headroom above the use the recommended request keeps, as a fraction.
	Headroom float64
	// OverAt and UnderAt are request-to-use ratios past which a request is
	// flagged.
	OverAt, UnderAt float64
}

// DefaultRule flags a request past 2x or under 0.8x of its use and
// recommends the use plus 30 percent.
var DefaultRule = Rule{Headroom: 0.3, OverAt: 2, UnderAt: 0.8}

// The sizes a person writes: no less than the floor, rounded up to the step.
const (
	CPUFloor    = 0.01
	CPUStep     = 0.005
	MemoryFloor = 32 << 20
	MemoryStep  = 16 << 20
)

// Trouble thresholds. A CPU limit that holds a container back in a quarter
// of its periods slows it; a memory peak at 90% of the limit is one spike
// from a kill. A limit raised for trouble gets LimitHeadroom above the
// larger of the limit and the use. ThrottleMinPeriods keeps a container
// that barely ran from reading as throttled.
const (
	ThrottledAt        = 0.25
	LimitAt            = 0.9
	LimitHeadroom      = 0.5
	ThrottleMinPeriods = 600
)

// Size rounds a size up to what a person writes.
func Size(v, floor, step float64) float64 {
	return math.Max(floor, math.Ceil(v/step-1e-9)*step)
}

// JudgeCPU fills a CPU measure's recommendation and verdict.
func (r Rule) JudgeCPU(m Measure, periods float64) Measure {
	m.Recommended, m.RecommendedLimit, m.Verdict, m.Reason = 0, 0, "", ""
	if m.P95 > 0 {
		m.Recommended = Size(m.P95*(1+r.Headroom), CPUFloor, CPUStep)
	}
	if m.Throttled >= ThrottledAt && periods >= ThrottleMinPeriods && m.Limit > 0 {
		m.Verdict = Risk
		m.Reason = fmt.Sprintf("its limit held it back in %.0f%% of its CPU periods", m.Throttled*100)
		m.RecommendedLimit = Size(math.Max(m.Limit, m.Peak)*(1+LimitHeadroom), CPUFloor, CPUStep)
		m.Recommended = math.Max(m.Recommended, m.Request)
		return m
	}
	m.Verdict = r.verdict(m, m.P95)
	return withinLimit(m, CPUFloor, CPUStep)
}

// JudgeMemory fills a memory measure's recommendation and verdict.
func (r Rule) JudgeMemory(m Measure) Measure {
	m.Recommended, m.RecommendedLimit, m.Verdict, m.Reason = 0, 0, "", ""
	if m.Peak > 0 {
		m.Recommended = Size(m.Peak*(1+r.Headroom), MemoryFloor, MemoryStep)
	}
	switch {
	case m.Kills >= 1:
		m.Reason = fmt.Sprintf("killed for memory %s in the window", times(m.Kills))
	case m.Limit > 0 && m.Peak >= LimitAt*m.Limit:
		m.Reason = fmt.Sprintf("peaked at %.0f%% of its limit", m.Peak/m.Limit*100)
	}
	if m.Reason != "" {
		m.Verdict = Risk
		if m.Limit > 0 {
			m.RecommendedLimit = Size(math.Max(m.Limit, m.Peak)*(1+LimitHeadroom), MemoryFloor, MemoryStep)
		}
		m.Recommended = math.Max(m.Recommended, m.Request)
		return withinLimit(m, MemoryFloor, MemoryStep)
	}
	m.Verdict = r.verdict(m, m.Peak)
	return withinLimit(m, MemoryFloor, MemoryStep)
}

// withinLimit raises the limit an advised request would pass: a request
// over its limit is not a pod Kubernetes admits.
func withinLimit(m Measure, floor, step float64) Measure {
	limit := m.Limit
	if m.RecommendedLimit > 0 {
		limit = m.RecommendedLimit
	}
	if m.Verdict != "" && limit > 0 && m.Recommended > limit {
		m.RecommendedLimit = Size(m.Recommended*(1+LimitHeadroom), floor, step)
	}
	return m
}

func (r Rule) verdict(m Measure, use float64) string {
	switch {
	case use <= 0:
		return ""
	case m.Request <= 0:
		return Unset
	case m.Request < r.UnderAt*use:
		return Under
	case m.Request > r.OverAt*use && m.Recommended < m.Request:
		return Over
	}
	return ""
}

func times(n float64) string {
	if k := int(math.Round(n)); k != 1 {
		return strconv.Itoa(k) + " times"
	}
	return "once"
}

// Snapshot is one round's workloads and the cluster's room.
type Snapshot struct {
	At        time.Time  `json:"at"`
	Window    string     `json:"window"`
	Workloads []Workload `json:"workloads"`
	// Allocatable is what the nodes offer pods: cores and bytes.
	Allocatable Capacity `json:"allocatable"`
}

// Capacity is an amount of CPU, in cores, and memory, in bytes.
type Capacity struct {
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
}

// The pod-to-owner map of kube-prometheus-stack's recording rules joins a
// pod's series to its workload. Over the window it keeps every pod the
// window saw, so a pod a rollout replaced still counts toward its
// workload.
const (
	owner = `namespace_workload_pod:kube_pod_owner:relabel`
	keys  = `namespace, workload, workload_type, container`
	join  = ` * on (namespace, pod) group_left (workload, workload_type) `
	// mains drops an init container that runs to completion before the
	// others; a sidecar stays. Use needs no such filter: the shaping drops
	// a container with no use or size of its own.
	mains = ` unless on (namespace, pod, container) kube_pod_init_container_info{restart_policy!="Always"}`
)

// Resources is what a pod's containers ask for (kind requests) or may use
// (kind limits) of one resource, over the pods sel picks, one series per
// container. A sidecar, an init container that runs beside the others
// (restartPolicy Always), holds its share for the pod's whole life, so it
// counts; kube-state-metrics reports it under the init container metrics.
// An init container that runs to completion first does not.
func Resources(kind, sel, resource string) string {
	if sel != "" {
		sel += ", "
	}
	sel += `resource="` + resource + `"`
	return fmt.Sprintf(`(kube_pod_container_resource_%[1]s{%[2]s} or (kube_pod_init_container_resource_%[1]s{%[2]s} and on (namespace, pod, container) kube_pod_init_container_info{restart_policy="Always"}))`, kind, sel)
}

// Queries are the round's questions. Use is each pod's own over the
// window, and a workload takes its busiest pod's, so a size is one pod's.
// A size is the one its running pods have, or for a workload with none
// running, such as a CronJob between runs, the last one its pods had.
func Queries(window string) map[string]string {
	owners := fmt.Sprintf(`max_over_time(%s[%s])`, owner, window)
	// use applies fn, such as max_over_time(, to each pod's series.
	use := func(fn, metric string) string {
		return fmt.Sprintf(`max by (`+keys+`) (%s%s[%s])`+join+`%s)`, fn, metric, window, owners)
	}
	last := func(kind, resource string) string {
		return fmt.Sprintf(`(last_over_time(kube_pod_container_resource_%[1]s{resource="%[2]s"}[%[3]s]) or (last_over_time(kube_pod_init_container_resource_%[1]s{resource="%[2]s"}[%[3]s]) and on (namespace, pod, container) last_over_time(kube_pod_init_container_info{restart_policy="Always"}[%[3]s])))`, kind, resource, window)
	}
	size := func(kind, resource string) string {
		return `max by (` + keys + `) (` + Resources(kind, "", resource) + join + owner + `) or max by (` + keys + `) (` + last(kind, resource) + join + owners + `)`
	}
	sum := func(metric string) string {
		return fmt.Sprintf(`sum by (`+keys+`) (((%s)`+mains+`)`+join+`%s)`, metric, owners)
	}
	cpu := `node_namespace_pod_container:container_cpu_usage_seconds_total:sum_irate`
	memory := `node_namespace_pod_container:container_memory_working_set_bytes`
	return map[string]string{
		"cpu_p95":        use("quantile_over_time(0.95, ", cpu),
		"cpu_peak":       use("max_over_time(", cpu),
		"memory_p95":     use("quantile_over_time(0.95, ", memory),
		"memory_peak":    use("max_over_time(", memory),
		"cpu_request":    size("requests", "cpu"),
		"memory_request": size("requests", "memory"),
		"cpu_limit":      size("limits", "cpu"),
		"memory_limit":   size("limits", "memory"),
		"memory_kills":   sum(fmt.Sprintf(`increase(kube_pod_container_status_restarts_total[%[1]s]) and on (namespace, pod, container) (max_over_time(kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}[%[1]s]) == 1)`, window)),
		"cpu_periods":    sum(fmt.Sprintf(`increase(container_cpu_cfs_periods_total{container!=""}[%s])`, window)),
		"cpu_throttled":  sum(fmt.Sprintf(`increase(container_cpu_cfs_throttled_periods_total{container!=""}[%s])`, window)),
		"pods":           `count by (namespace, workload, workload_type) (` + owner + ` * on (namespace, pod) group_left () (max by (namespace, pod) (kube_pod_status_phase{phase=~"Running|Pending"}) == 1))`,
		"cronjobs":       `group by (namespace, cronjob) (kube_cronjob_info)`,
		"job_owners":     fmt.Sprintf(`group by (namespace, job_name, owner_name) (max_over_time(kube_job_owner{owner_kind="CronJob"}[%s]))`, window),
		"allocatable":    `sum by (resource) (kube_node_status_allocatable{resource=~"cpu|memory"})`,
	}
}

// Kinds of owner the shaping names itself.
const (
	CronJob   = "CronJob"
	RunnerSet = "RunnerSet"
	StaticPod = "staticpod"
)

// runner is an Actions Runner Controller runner's name: its runner set's
// and a suffix of its own.
var runner = regexp.MustCompile(`^(.+-runner)-[a-z0-9]+$`)

// owned names the owner a person sizes: a Job's CronJob, a runner's
// runner set. A Job no CronJob owns has nothing to size, so ok is false.
func owned(kind, name string, cron map[string]string, namespace string) (string, string, bool) {
	switch strings.ToLower(kind) {
	case "job":
		c, ok := cron[namespace+"/"+name]
		return CronJob, c, ok
	case "ephemeralrunner":
		if m := runner.FindStringSubmatch(name); m != nil {
			return RunnerSet, m[1], true
		}
		return RunnerSet, name, true
	}
	return kind, name, true
}

// Shape joins the answers into the workloads that run now and judges each
// container. A runner set is kept while it ran in the window: its runners
// come and go. managed are the namespace patterns the platform sizes.
func Shape(answers map[string][]promapi.Sample, rule Rule, managed []string) ([]Workload, Capacity) {
	cron := map[string]string{}
	for _, s := range answers["job_owners"] {
		cron[s.Labels["namespace"]+"/"+s.Labels["job_name"]] = s.Labels["owner_name"]
	}
	cronjobs := map[string]bool{}
	for _, s := range answers["cronjobs"] {
		cronjobs[s.Labels["namespace"]+"/"+s.Labels["cronjob"]] = true
	}
	type key struct{ ns, kind, name string }
	pods := map[key]int{}
	for _, s := range answers["pods"] {
		kind, name, ok := owned(s.Labels["workload_type"], s.Labels["workload"], cron, s.Labels["namespace"])
		if ok {
			pods[key{s.Labels["namespace"], kind, name}] += int(s.Value)
		}
	}
	type cell struct {
		Measure
		periods float64
	}
	byKey := map[key]map[string]*[2]cell{}
	for name, samples := range answers {
		for _, s := range samples {
			if s.Labels["workload"] == "" || s.Labels["container"] == "" {
				continue
			}
			kind, wl, ok := owned(s.Labels["workload_type"], s.Labels["workload"], cron, s.Labels["namespace"])
			if !ok {
				continue
			}
			k := key{s.Labels["namespace"], kind, wl}
			cs, ok := byKey[k]
			if !ok {
				cs = map[string]*[2]cell{}
				byKey[k] = cs
			}
			c, ok := cs[s.Labels["container"]]
			if !ok {
				c = &[2]cell{}
				cs[s.Labels["container"]] = c
			}
			cpu, mem := &c[0], &c[1]
			// Runs of one owner fold together: the largest size and use,
			// every kill, every throttled period.
			switch name {
			case "cpu_p95":
				cpu.P95 = math.Max(cpu.P95, s.Value)
			case "cpu_peak":
				cpu.Peak = math.Max(cpu.Peak, s.Value)
			case "cpu_request":
				cpu.Request = math.Max(cpu.Request, s.Value)
			case "cpu_limit":
				cpu.Limit = math.Max(cpu.Limit, s.Value)
			case "cpu_periods":
				cpu.periods += s.Value
			case "cpu_throttled":
				cpu.Throttled += s.Value
			case "memory_p95":
				mem.P95 = math.Max(mem.P95, s.Value)
			case "memory_peak":
				mem.Peak = math.Max(mem.Peak, s.Value)
			case "memory_request":
				mem.Request = math.Max(mem.Request, s.Value)
			case "memory_limit":
				mem.Limit = math.Max(mem.Limit, s.Value)
			case "memory_kills":
				mem.Kills += s.Value
			}
		}
	}
	out := make([]Workload, 0, len(byKey))
	for k, cs := range byKey {
		n := pods[k]
		live := n > 0 || k.kind == RunnerSet || (k.kind == CronJob && cronjobs[k.ns+"/"+k.name])
		if !live {
			continue
		}
		w := Workload{Namespace: k.ns, Workload: k.name, Kind: k.kind, Pods: n, Managed: k.kind == StaticPod || matches(managed, k.ns)}
		for name, c := range cs {
			if c[0].P95 == 0 && c[0].Peak == 0 && c[0].Request == 0 && c[1].Peak == 0 && c[1].Request == 0 {
				continue
			}
			cpu := c[0]
			if cpu.periods > 0 {
				cpu.Throttled /= cpu.periods
			} else {
				cpu.Throttled = 0
			}
			w.Containers = append(w.Containers, Container{Name: name, CPU: rule.JudgeCPU(cpu.Measure, cpu.periods), Memory: rule.JudgeMemory(c[1].Measure)})
		}
		sort.Slice(w.Containers, func(i, j int) bool { return w.Containers[i].Name < w.Containers[j].Name })
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Workload < out[j].Workload
	})
	var room Capacity
	for _, s := range answers["allocatable"] {
		switch s.Labels["resource"] {
		case "cpu":
			room.CPU = s.Value
		case "memory":
			room.Memory = s.Value
		}
	}
	return out, room
}

// matches says a namespace matches one of the patterns, such as aks-*.
func matches(patterns []string, namespace string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, namespace); ok {
			return true
		}
	}
	return false
}

// DefaultManaged are the namespaces the platform sizes when K8S_MANAGED is
// unset: the cluster's own and a managed cluster's add-ons.
const DefaultManaged = "kube-system,aks-*"

// Collector is the kubernetes collector.
type Collector struct {
	prom     *promapi.Client
	window   string
	interval time.Duration
	rule     Rule
	managed  []string
	log      *slog.Logger
	inst     *collect.Instruments
	latest   atomic.Pointer[Snapshot]
}

// FromEnv builds the collector, or nil when PROMETHEUS_URL is unset.
func FromEnv(log *slog.Logger, inst *collect.Instruments) (*Collector, error) {
	base := os.Getenv("PROMETHEUS_URL")
	if base == "" {
		return nil, nil
	}
	rule := DefaultRule
	if v := os.Getenv("K8S_HEADROOM"); v != "" {
		h, err := strconv.ParseFloat(v, 64)
		if err != nil || h < 0 {
			return nil, errors.New("K8S_HEADROOM must be a fraction such as 0.3")
		}
		rule.Headroom = h
	}
	window := os.Getenv("K8S_WINDOW")
	if window == "" {
		window = "7d"
	}
	list := os.Getenv("K8S_MANAGED")
	if list == "" {
		list = DefaultManaged
	}
	var managed []string
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if _, err := path.Match(p, ""); err != nil {
				return nil, fmt.Errorf("K8S_MANAGED pattern %q: %w", p, err)
			}
			managed = append(managed, p)
		}
	}
	prom, err := azure.Prometheus(base)
	if err != nil {
		return nil, err
	}
	c := &Collector{
		prom:     prom,
		window:   window,
		interval: collect.Interval(os.Getenv("K8S_INTERVAL"), 15*time.Minute),
		rule:     rule,
		managed:  managed,
		log:      log,
		inst:     inst,
	}
	return c, c.register()
}

func (c *Collector) Name() string { return "kubernetes" }

// Latest is the last round's workloads, or nil before the first.
func (c *Collector) Latest() *Snapshot { return c.latest.Load() }

func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.round)
}

// Round reads every query once and keeps the result.
func (c *Collector) round(ctx context.Context) error {
	answers := map[string][]promapi.Sample{}
	for name, expr := range Queries(c.window) {
		samples, err := c.prom.Query(ctx, expr)
		if err != nil {
			c.inst.Upstream(ctx, "prometheus", collect.Failed)
			return fmt.Errorf("%s: %w", name, err)
		}
		c.inst.Upstream(ctx, "prometheus", collect.OK)
		answers[name] = samples
	}
	workloads, room := Shape(answers, c.rule, c.managed)
	if len(workloads) == 0 {
		return fmt.Errorf("no workload has requests or use; the Prometheus lacks the Kubernetes recording rules (%s)", owner)
	}
	snap := &Snapshot{At: time.Now().UTC(), Window: c.window, Workloads: workloads, Allocatable: room}
	c.latest.Store(snap)
	c.log.InfoContext(ctx, "workloads measured", "workloads", len(snap.Workloads), "window", c.window)
	return nil
}

// register exports the last round as gauges, read at each export, by
// namespace, workload, and container.
func (c *Collector) register() error {
	m := collect.Meter()
	type gauge struct {
		name, unit, desc string
		read             func(Container) float64
	}
	gauges := []gauge{
		{"tally.k8s.cpu.request_cores", "{core}", "CPU one of the workload's pods requests for the container, in cores.", func(c Container) float64 { return c.CPU.Request }},
		{"tally.k8s.cpu.p95_cores", "{core}", "CPU the container used at the 95th percentile over the window, in its busiest pod, in cores.", func(c Container) float64 { return c.CPU.P95 }},
		{"tally.k8s.cpu.limit_cores", "{core}", "CPU limit on the container, in cores.", func(c Container) float64 { return c.CPU.Limit }},
		{"tally.k8s.memory.request_bytes", "By", "Memory one of the workload's pods requests for the container.", func(c Container) float64 { return c.Memory.Request }},
		{"tally.k8s.memory.p95_bytes", "By", "Memory the container held at the 95th percentile over the window, in its busiest pod.", func(c Container) float64 { return c.Memory.P95 }},
		{"tally.k8s.memory.peak_bytes", "By", "The most memory the container held over the window, in its busiest pod.", func(c Container) float64 { return c.Memory.Peak }},
		{"tally.k8s.memory.limit_bytes", "By", "Memory limit on the container.", func(c Container) float64 { return c.Memory.Limit }},
	}
	instruments := make([]metric.Float64ObservableGauge, len(gauges))
	observables := make([]metric.Observable, 0, len(gauges)+1)
	for i, g := range gauges {
		inst, err := m.Float64ObservableGauge(g.name, metric.WithUnit(g.unit), metric.WithDescription(g.desc))
		if err != nil {
			return err
		}
		instruments[i] = inst
		observables = append(observables, inst)
	}
	rec, err := m.Int64ObservableGauge("tally.k8s.recommendation", metric.WithDescription("1 for a container resource that does not fit, by kind: risk (killed for memory, near its memory limit, or held back by its CPU limit), under (request under 0.8x its use), unset (no request), or over (request over 2x its use). Use is CPU at p95 and memory at its peak. Workloads the platform sizes are left out."))
	if err != nil {
		return err
	}
	observables = append(observables, rec)
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		snap := c.latest.Load()
		if snap == nil {
			return nil
		}
		for _, w := range snap.Workloads {
			for _, ct := range w.Containers {
				who := []attribute.KeyValue{attribute.String("namespace", w.Namespace), attribute.String("workload", w.Workload), attribute.String("container", ct.Name)}
				for i, g := range gauges {
					o.ObserveFloat64(instruments[i], g.read(ct), metric.WithAttributes(who...))
				}
				if w.Managed {
					continue
				}
				for _, r := range []struct {
					name string
					m    Measure
				}{{"cpu", ct.CPU}, {"memory", ct.Memory}} {
					if r.m.Verdict != "" {
						o.ObserveInt64(rec, 1, metric.WithAttributes(append(who, attribute.String("kind", r.m.Verdict), attribute.String("resource", r.name))...))
					}
				}
			}
		}
		return nil
	}, observables...)
	return err
}
