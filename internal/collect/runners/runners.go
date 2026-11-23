// Package runners measures self-hosted CI runners from Prometheus:
// what each repository's job pods requested and for how long, and what the
// pool they run in costs. It turns on with PROMETHEUS_URL and
// RUNNERS_NAMESPACE.
package runners

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/azure"
	"github.com/orchard-suite/tally/internal/collect/kubernetes"
	"github.com/orchard-suite/tally/internal/collect/promapi"
)

// Forges.
const (
	GitHub = "github"
	GitLab = "gitlab"
)

// podPatterns name each forge's job pods. The one group is the key the
// forge's names are looked up by: an Actions Runner Controller runner
// set's name, or a GitLab project's id.
var podPatterns = map[string]string{
	GitHub: `(.+)-runner-[a-z0-9]+`,
	GitLab: `runner-[a-z0-9]+-project-([0-9]+)-concurrent-[0-9]+-[a-z0-9]+`,
}

// Usage is one repository's job pods over the window.
type Usage struct {
	Forge      string `json:"forge"`
	Repository string `json:"repository"`
	// PodSeconds is how long its job pods ran.
	PodSeconds float64 `json:"pod_seconds"`
	// CPUSeconds and MemorySeconds are what they requested over that
	// time, in core-seconds and byte-seconds.
	CPUSeconds    float64 `json:"cpu_seconds"`
	MemorySeconds float64 `json:"memory_seconds"`
}

// Pool is where the job pods run. A pool on its own nodes (Nodes set) is
// priced by its node hours; a pool that shares the cluster's nodes is
// priced at its namespace's share of the cluster's requests.
type Pool struct {
	Namespace string `json:"namespace"`
	// CPUShare and MemoryShare are the namespace's average part of the
	// cluster's requested CPU and memory over the window.
	CPUShare    float64 `json:"cpu_share"`
	MemoryShare float64 `json:"memory_share"`
	// Nodes is the pattern naming the pool's own nodes, NodeHours how long
	// they ran over the window, and PricePerHour what one node costs.
	Nodes        string  `json:"nodes,omitempty"`
	NodeHours    float64 `json:"node_hours,omitempty"`
	PricePerHour float64 `json:"price_per_hour,omitempty"`
}

// Snapshot is one round.
type Snapshot struct {
	At     time.Time `json:"at"`
	Window string    `json:"window"`
	// Hours is the window's length.
	Hours float64 `json:"hours"`
	Pool  Pool    `json:"pool"`
	Usage []Usage `json:"usage"`
}

// Config is the collector's settings.
type Config struct {
	Namespace    string
	Forge        string
	Nodes        string
	PricePerHour float64
	Window       string
	Hours        float64
}

// Step is the grid job pods are read on: fine enough for a one-minute job.
// ShareStep is the grid the cluster's averages are read on, where a coarse
// grid gives the same average for a fifth of the work. Slice is how many
// hours of the window one per-pod question covers: Azure's managed
// Prometheus refuses a week, or even a day, of every job pod at a
// one-minute grid in one question, and answers six hours of it in a second.
const (
	Step      = time.Minute
	ShareStep = 5 * time.Minute
	Slice     = 6
)

// Sliced is the separator between a question's name and its slice, in the
// names Queries gives: pod_seconds/0 is the newest slice.
const Sliced = "/"

func quote(s string) string { return strconv.Quote(s) }

// Queries are the round's questions, by name. The per-pod ones come in
// slices of the window, newest first, each named <name>/<slice>; the
// slices do not overlap, so their answers add up to the window's.
func Queries(c Config) map[string]string {
	pods := fmt.Sprintf(`namespace=%s, pod=~%s`, quote(c.Namespace), quote(podPatterns[c.Forge]))
	q := map[string]string{}
	for i, end := 0, 0; end < int(c.Hours); i++ {
		span := min(Slice, int(c.Hours)-end)
		offset := ""
		if end > 0 {
			offset = fmt.Sprintf(" offset %dh", end)
		}
		over := func(inner string) string {
			return fmt.Sprintf(`sum_over_time((%s)[%dh:%s]%s) * %d`, inner, span, promDuration(Step), offset, int(Step.Seconds()))
		}
		at := Sliced + strconv.Itoa(i)
		q["pod_seconds"+at] = over(`group by (pod) (` + kubernetes.Resources("requests", pods, "cpu") + `)`)
		q["cpu_seconds"+at] = over(`sum by (pod) (` + kubernetes.Resources("requests", pods, "cpu") + `)`)
		q["memory_seconds"+at] = over(`sum by (pod) (` + kubernetes.Resources("requests", pods, "memory") + `)`)
		end += span
	}
	if c.Nodes != "" {
		q["node_hours"] = fmt.Sprintf(`sum_over_time((count(kube_node_info{node=~%s}) or vector(0))[%s:%s]) / %d`, quote(c.Nodes), c.Window, promDuration(Step), int(time.Hour/Step))
		return q
	}
	avg := func(sel, resource string) string {
		return fmt.Sprintf(`avg_over_time((sum(%s) or vector(0))[%s:%s])`, kubernetes.Resources("requests", sel, resource), c.Window, promDuration(ShareStep))
	}
	ns := `namespace=` + quote(c.Namespace)
	q["ns_cpu"] = avg(ns, "cpu")
	q["ns_memory"] = avg(ns, "memory")
	q["all_cpu"] = avg("", "cpu")
	q["all_memory"] = avg("", "memory")
	return q
}

// Keys turns the per-pod answers into per-key sums: the group the forge's
// pattern captures from each pod's name.
func Keys(forge string, answers map[string][]promapi.Sample) map[string]*Usage {
	re := regexp.MustCompile(`^` + podPatterns[forge] + `$`)
	out := map[string]*Usage{}
	for _, name := range []string{"pod_seconds", "cpu_seconds", "memory_seconds"} {
		for _, s := range answers[name] {
			m := re.FindStringSubmatch(s.Labels["pod"])
			if m == nil {
				continue
			}
			u, ok := out[m[1]]
			if !ok {
				u = &Usage{Forge: forge, Repository: m[1]}
				out[m[1]] = u
			}
			switch name {
			case "pod_seconds":
				u.PodSeconds += s.Value
			case "cpu_seconds":
				u.CPUSeconds += s.Value
			case "memory_seconds":
				u.MemorySeconds += s.Value
			}
		}
	}
	return out
}

// Name gives each key its repository's name, and adds up keys that name
// the same repository. A key with no name keeps the key, with a GitLab
// project id shown as "project <id>".
func Name(forge string, keys map[string]*Usage, names map[string]string) []Usage {
	byRepo := map[string]*Usage{}
	for key, u := range keys {
		repo := names[key]
		if repo == "" {
			repo = key
			if forge == GitLab {
				repo = "project " + key
			}
		}
		r, ok := byRepo[repo]
		if !ok {
			r = &Usage{Forge: forge, Repository: repo}
			byRepo[repo] = r
		}
		r.PodSeconds += u.PodSeconds
		r.CPUSeconds += u.CPUSeconds
		r.MemorySeconds += u.MemorySeconds
	}
	out := make([]Usage, 0, len(byRepo))
	for _, u := range byRepo {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PodSeconds != out[j].PodSeconds {
			return out[i].PodSeconds > out[j].PodSeconds
		}
		return out[i].Repository < out[j].Repository
	})
	return out
}

// RunnerSetNames maps each Actions Runner Controller runner set to its
// repository, owner/name, from the listeners' gauges.
func RunnerSetNames(samples []promapi.Sample) map[string]string {
	out := map[string]string{}
	for _, s := range samples {
		set, org, repo := s.Labels["name"], s.Labels["organization"], s.Labels["repository"]
		if set == "" || org == "" {
			continue
		}
		if repo == "" {
			out[set] = org
			continue
		}
		out[set] = org + "/" + repo
	}
	return out
}

// PoolFrom reads the pool's figures from the round's answers.
func PoolFrom(c Config, answers map[string][]promapi.Sample) (Pool, error) {
	p := Pool{Namespace: c.Namespace, Nodes: c.Nodes, PricePerHour: c.PricePerHour}
	one := func(name string) (float64, error) {
		s := answers[name]
		if len(s) != 1 {
			return 0, fmt.Errorf("%s: want one value, got %d", name, len(s))
		}
		return s[0].Value, nil
	}
	if c.Nodes != "" {
		h, err := one("node_hours")
		p.NodeHours = h
		return p, err
	}
	var v [4]float64
	for i, name := range []string{"ns_cpu", "all_cpu", "ns_memory", "all_memory"} {
		x, err := one(name)
		if err != nil {
			return p, err
		}
		v[i] = x
	}
	if v[1] <= 0 || v[3] <= 0 {
		return p, errors.New("the cluster requested no CPU or memory over the window; the Prometheus lacks kube-state-metrics")
	}
	p.CPUShare, p.MemoryShare = v[0]/v[1], v[2]/v[3]
	return p, nil
}

// Collector is the runners collector.
type Collector struct {
	cfg      Config
	prom     *promapi.Client
	gitlab   *GitLabNames
	interval time.Duration
	log      *slog.Logger
	inst     *collect.Instruments
	latest   atomic.Pointer[Snapshot]
}

// FromEnv builds the collector, or nil when PROMETHEUS_URL or
// RUNNERS_NAMESPACE is unset.
func FromEnv(log *slog.Logger, inst *collect.Instruments) (*Collector, error) {
	base, ns := os.Getenv("PROMETHEUS_URL"), os.Getenv("RUNNERS_NAMESPACE")
	if base == "" || ns == "" {
		return nil, nil
	}
	cfg := Config{Namespace: ns, Forge: os.Getenv("RUNNERS_FORGE"), Nodes: os.Getenv("RUNNERS_NODES"), Window: os.Getenv("RUNNERS_WINDOW")}
	if _, ok := podPatterns[cfg.Forge]; !ok {
		return nil, fmt.Errorf("RUNNERS_FORGE must be %s or %s, got %q", GitHub, GitLab, cfg.Forge)
	}
	if cfg.Window == "" {
		cfg.Window = "7d"
	}
	hours, err := WindowHours(cfg.Window)
	if err != nil {
		return nil, err
	}
	cfg.Hours = hours
	price := os.Getenv("RUNNERS_NODE_PRICE")
	if (cfg.Nodes == "") != (price == "") {
		return nil, errors.New("RUNNERS_NODES and RUNNERS_NODE_PRICE go together: the pool's own nodes and what one costs an hour")
	}
	if price != "" {
		if cfg.PricePerHour, err = strconv.ParseFloat(price, 64); err != nil || cfg.PricePerHour <= 0 {
			return nil, fmt.Errorf("RUNNERS_NODE_PRICE must be a price an hour, such as 0.384, got %q", price)
		}
		if _, err := regexp.Compile(cfg.Nodes); err != nil {
			return nil, fmt.Errorf("RUNNERS_NODES: %w", err)
		}
	}
	prom, err := azure.Prometheus(base)
	if err != nil {
		return nil, err
	}
	c := &Collector{cfg: cfg, prom: prom, interval: collect.Interval(os.Getenv("RUNNERS_INTERVAL"), time.Hour), log: log, inst: inst}
	if cfg.Forge == GitLab {
		c.gitlab = GitLabFromEnv(inst)
		if c.gitlab == nil {
			log.Info("runners: GITLAB_URL and GITLAB_TOKEN are not both set; GitLab projects show by id")
		}
	}
	return c, nil
}

// WindowHours reads a window such as 7d or 36h.
func WindowHours(w string) (float64, error) {
	if n, ok := strings.CutSuffix(w, "d"); ok {
		d, err := strconv.Atoi(n)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("RUNNERS_WINDOW %q: want days, such as 7d, or hours, such as 36h", w)
		}
		return float64(d * 24), nil
	}
	d, err := time.ParseDuration(w)
	if err != nil || d < time.Hour || d%time.Hour != 0 {
		return 0, fmt.Errorf("RUNNERS_WINDOW %q: want days, such as 7d, or hours, such as 36h", w)
	}
	return d.Hours(), nil
}

func promDuration(d time.Duration) string { return strconv.Itoa(int(d.Seconds())) + "s" }

func (c *Collector) Name() string { return "runners" }

// Latest is the last round, or nil before the first.
func (c *Collector) Latest() *Snapshot { return c.latest.Load() }

func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.round)
}

func (c *Collector) query(ctx context.Context, expr string) ([]promapi.Sample, error) {
	s, err := c.prom.Query(ctx, expr)
	if err != nil {
		c.inst.Upstream(ctx, "prometheus", collect.Failed)
		return nil, err
	}
	c.inst.Upstream(ctx, "prometheus", collect.OK)
	return s, nil
}

func (c *Collector) round(ctx context.Context) error {
	answers := map[string][]promapi.Sample{}
	for name, expr := range Queries(c.cfg) {
		s, err := c.query(ctx, expr)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		base, _, _ := strings.Cut(name, Sliced)
		answers[base] = append(answers[base], s...)
	}
	pool, err := PoolFrom(c.cfg, answers)
	if err != nil {
		return err
	}
	keys := Keys(c.cfg.Forge, answers)
	names := map[string]string{}
	switch {
	case c.cfg.Forge == GitHub:
		s, err := c.query(ctx, `group by (name, organization, repository) (gha_max_runners{exported_namespace=`+quote(c.cfg.Namespace)+`})`)
		if err != nil {
			return fmt.Errorf("runner set names: %w", err)
		}
		names = RunnerSetNames(s)
	case c.gitlab != nil:
		ids := make([]string, 0, len(keys))
		for id := range keys {
			ids = append(ids, id)
		}
		if names, err = c.gitlab.Names(ctx, ids); err != nil {
			return err
		}
	}
	snap := &Snapshot{At: time.Now().UTC(), Window: c.cfg.Window, Hours: c.cfg.Hours, Pool: pool, Usage: Name(c.cfg.Forge, keys, names)}
	c.latest.Store(snap)
	c.log.InfoContext(ctx, "runners measured", "repositories", len(snap.Usage), "window", c.cfg.Window, "namespace", c.cfg.Namespace)
	return nil
}
