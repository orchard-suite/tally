// Package catalog writes each namespace's monthly cost onto its entity in an
// Orrery hub. It turns on with ORRERY_URL and ORRERY_TOKEN, a source token.
// Every write is in enrich mode: it adds fields to namespaces the hub already
// has and never creates one or keeps one alive.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/orchard-suite/platformkit/orrery"

	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/estate"
)

// The blueprint and fields tally writes.
const (
	Blueprint   = "k8s_namespace"
	CostField   = "cost_per_month"
	UpdateField = "cost_updated"
	// Scope is the scope of tally's runs on the hub.
	Scope = "tally"
	// Upstream is the name the hub's requests are counted under.
	Upstream = "orrery"
)

// Inputs are the collectors a namespace's cost is built from.
var Inputs = []string{"kubernetes", "power"}

// Blueprints are the fields tally offers on the namespace blueprint. The
// hub adds the ones it lacks and leaves the rest as it is.
func Blueprints() []orrery.Blueprint {
	return []orrery.Blueprint{{
		"id":    Blueprint,
		"title": "Kubernetes namespace",
		"fields": map[string]any{
			CostField:   map[string]any{"type": "number", "title": "Cost a month", "description": "The namespace's share of the cluster hosts' power cost a month, in USD, by requested CPU and memory. Written by tally."},
			UpdateField: map[string]any{"type": "datetime", "title": "Cost updated", "description": "When tally last wrote the cost."},
		},
	}}
}

// Entities shapes the estate's namespace costs into enrich writes, one per
// namespace, stamped at the time the estate was built. The catalog keys a
// namespace under its cluster, as cluster/namespace.
func Entities(c estate.Costs, cluster string) []orrery.Entity {
	updated := c.GeneratedAt.UTC().Format(time.RFC3339)
	out := make([]orrery.Entity, 0, len(c.Namespaces))
	for _, n := range c.Namespaces {
		if n.Namespace == "" {
			continue
		}
		out = append(out, orrery.Entity{
			Blueprint: Blueprint,
			Key:       cluster + "/" + n.Namespace,
			Fields:    map[string]any{CostField: n.PerMonth, UpdateField: updated},
		})
	}
	return out
}

// Stale says why a round's inputs cannot be written, or "" when they can.
// Each input collector must have finished its last round, and the cluster's
// power must have been read, so a failed read is never written as a zero.
func Stale(statuses []collect.Status, c estate.Costs) string {
	byName := map[string]collect.Status{}
	for _, s := range statuses {
		byName[s.Name] = s
	}
	for _, name := range Inputs {
		s, ok := byName[name]
		switch {
		case !ok:
			return name + " is off"
		case s.LastSuccess == nil:
			return name + " has not finished a round"
		case s.LastRun != nil && s.LastRun.After(*s.LastSuccess):
			return name + "'s last round failed"
		}
	}
	var cluster float64
	for _, h := range c.Hosts {
		if h.Cluster {
			cluster += h.PerMonth
		}
	}
	if cluster <= 0 {
		return "no cluster host's power was read"
	}
	if len(c.Namespaces) == 0 {
		return "no namespace was measured"
	}
	return ""
}

// ran is whether every input has finished a round, worked or not.
func ran(statuses []collect.Status) bool {
	n := 0
	for _, s := range statuses {
		for _, name := range Inputs {
			if s.Name == name && s.LastRun != nil {
				n++
			}
		}
	}
	return n == len(Inputs)
}

// Writer is the catalog writer, run like a collector.
type Writer struct {
	hub      *orrery.Client
	sources  estate.Sources
	inst     *collect.Instruments
	interval time.Duration
	log      *slog.Logger
	cluster  string
	offered  bool
	// statuses is where every collector stands.
	statuses func() []collect.Status
}

// FromEnv builds the writer, or nil when ORRERY_URL or ORRERY_TOKEN is unset.
func FromEnv(log *slog.Logger, inst *collect.Instruments, sources estate.Sources) *Writer {
	url, token := os.Getenv("ORRERY_URL"), os.Getenv("ORRERY_TOKEN")
	if url == "" || token == "" {
		log.Info("catalog writer off", "reason", "ORRERY_URL and ORRERY_TOKEN are not both set")
		return nil
	}
	cluster := os.Getenv("ORRERY_CLUSTER")
	if cluster == "" {
		log.Info("catalog writer off", "reason", "ORRERY_CLUSTER, the cluster name the catalog keys namespaces under, is unset")
		return nil
	}
	if sources.Workloads == nil || sources.Power == nil {
		log.Info("catalog writer off", "reason", "it needs the kubernetes and power collectors")
		return nil
	}
	return New(orrery.New(url, token, orrery.WithLogger(log)), log, inst, sources, cluster, collect.Interval(os.Getenv("CATALOG_INTERVAL"), 15*time.Minute))
}

// New builds the writer on a hub client.
func New(hub *orrery.Client, log *slog.Logger, inst *collect.Instruments, sources estate.Sources, cluster string, interval time.Duration) *Writer {
	return &Writer{hub: hub, sources: sources, inst: inst, cluster: cluster, interval: interval, log: log, statuses: inst.Statuses}
}

func (w *Writer) Name() string { return "catalog" }

// Interval is how often a round runs when the last one worked.
func (w *Writer) Interval() time.Duration { return w.interval }

// Run waits for the inputs' first rounds, then writes every interval.
func (w *Writer) Run(ctx context.Context, done func(ok bool)) error {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for !ran(w.statuses()) {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
	return collect.Every(ctx, w.Name(), w.interval, w.log, done, w.Round)
}

// Round builds the estate and writes every namespace's cost onto the hub.
// A round whose inputs are stale writes nothing and fails.
func (w *Writer) Round(ctx context.Context) error {
	costs := estate.Build(w.sources, time.Now())
	if reason := Stale(w.statuses(), costs); reason != "" {
		w.log.WarnContext(ctx, "catalog round skipped", "reason", reason)
		return fmt.Errorf("inputs stale: %s", reason)
	}
	if !w.offered {
		_, err := w.hub.OfferBlueprints(ctx, Blueprints())
		w.inst.Upstream(ctx, Upstream, outcome(err))
		if err != nil {
			return fmt.Errorf("offer blueprints: %w", err)
		}
		w.offered = true
	}
	entities := Entities(costs, w.cluster)
	res, err := w.hub.Incremental(ctx, Scope, func(add func(orrery.Entity) error) error {
		for _, e := range entities {
			if err := add(e); err != nil {
				return err
			}
		}
		return nil
	}, orrery.EnrichOnly(), orrery.RunObservedAt(costs.GeneratedAt))
	w.inst.Upstream(ctx, Upstream, outcome(err))
	if err != nil {
		return err
	}
	w.log.InfoContext(ctx, "catalog costs written", "namespaces", len(entities), "updated", res.Counts.Updated, "unchanged", res.Counts.Unchanged, "rejected", res.Counts.Rejected)
	return nil
}

// outcome is a hub request's budget outcome: refused for a rate limit,
// failed for any other error.
func outcome(err error) string {
	var api *orrery.APIError
	switch {
	case err == nil:
		return collect.OK
	case errors.As(err, &api) && api.Status == http.StatusTooManyRequests:
		return collect.Refused
	default:
		return collect.Failed
	}
}
