package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/orrery"

	"github.com/orchard-suite/tally/internal/agents"
	"github.com/orchard-suite/tally/internal/collect"
)

// The fields the uses writer offers and writes on each blueprint it targets.
const (
	UsesField      = "uses_30d"
	ComputersField = "computers_30d"
	UsedField      = "usage_updated"
	// InstalledField is on how many computers the plugin is installed.
	InstalledField = "installed_computers"
	// UsesScope is the scope of the uses writer's runs on the hub.
	UsesScope = "tally-uses"
	// UsesWindow is the span the counts cover.
	UsesWindow = 30 * 24 * time.Hour
)

// Target is one blueprint the uses writer counts onto, and how it keys an
// entity from a plugin, and from a skill for a skill blueprint:
// claude-code/{plugin} or claude-code/{plugin}/{skill}.
type Target struct {
	Blueprint string
	Key       string
}

// Skill says whether the target keys a skill rather than a plugin.
func (t Target) Skill() bool { return strings.Contains(t.Key, "{skill}") }

// KeyOf is the entity key of a plugin, or of a skill named <plugin>:<skill>.
func (t Target) KeyOf(name string) string {
	plugin, skill, _ := strings.Cut(name, ":")
	return strings.NewReplacer("{plugin}", plugin, "{skill}", skill).Replace(t.Key)
}

// Keys says whether a key is of this target's shape: it starts as the
// template does before {plugin}. A key of another shape, such as an agent
// another assistant runs, is not one tally counts, so it is left alone.
func (t Target) Keys(key string) bool {
	prefix, _, _ := strings.Cut(t.Key, "{plugin}")
	return strings.HasPrefix(key, prefix) && len(key) > len(prefix)
}

// ParseTargets reads ORRERY_USES: blueprint=key template, comma separated.
// A template names {plugin}, and a skill's also {skill}.
func ParseTargets(s string) ([]Target, error) {
	var out []Target
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bp, key, ok := strings.Cut(part, "=")
		if !ok || bp == "" || !strings.Contains(key, "{plugin}") {
			return nil, fmt.Errorf("ORRERY_USES: %q is not blueprint=template with {plugin}", part)
		}
		out = append(out, Target{Blueprint: bp, Key: key})
	}
	return out, nil
}

// UsesBlueprints are the fields the uses writer offers on its targets. The
// hub adds the ones a blueprint lacks and leaves the rest as it is.
func UsesBlueprints(targets []Target) []orrery.Blueprint {
	out := make([]orrery.Blueprint, 0, len(targets))
	for _, t := range targets {
		what := "plugin's skills, commands, and tools"
		if t.Skill() {
			what = "skill"
		}
		out = append(out, orrery.Blueprint{
			"id": t.Blueprint,
			// An offer names the blueprint's title; the hub keeps the one an
			// existing blueprint has.
			"title": t.Blueprint,
			"fields": map[string]any{
				UsesField:      map[string]any{"type": "number", "title": "Uses (30 days)", "description": "How many times agents used this " + what + " in the last 30 days: loaded by an agent, typed by a person, or called. Counted by tally from the transcripts on each computer that ships to it."},
				ComputersField: map[string]any{"type": "number", "title": "Computers (30 days)", "description": "On how many computers this " + what + " was used in the last 30 days, of the computers that ship to tally."},
				InstalledField: map[string]any{"type": "number", "title": "Installed on (computers)", "description": "On how many computers this " + installedWhat(t) + " is installed now, of the computers that shipped to tally in the last 30 days. Read from each computer's own record of installed plugins."},
				UsedField:      map[string]any{"type": "datetime", "title": "Usage updated", "description": "When tally last wrote the usage."},
			},
		})
	}
	return out
}

func installedWhat(t Target) string {
	if t.Skill() {
		return "skill's plugin"
	}
	return "plugin"
}

// UsesEntities shapes the counts into enrich writes, one per entity of a
// target's shape the hub already has. An entity nothing used in the window
// is written as zero, so a count never outlives its use.
func UsesEntities(targets []Target, rows []agents.UseRow, installed map[string]int64, existing map[string][]string, at time.Time) []orrery.Entity {
	plugins, skills := agents.Plugins(rows), agents.Skills(rows)
	updated := at.UTC().Format(time.RFC3339)
	var out []orrery.Entity
	for _, t := range targets {
		reach := map[string]agents.Reach{}
		from := plugins
		if t.Skill() {
			from = skills
		}
		for name, r := range from {
			reach[t.KeyOf(name)] = r
		}
		// A plugin's key by its name; a skill's entities take their plugin's.
		installedOn := map[string]int64{}
		for _, key := range existing[t.Blueprint] {
			for plugin, n := range installed {
				if key == t.KeyOf(plugin) || (t.Skill() && strings.HasPrefix(key, t.KeyOf(plugin+":"))) {
					installedOn[key] = n
				}
			}
		}
		for _, key := range existing[t.Blueprint] {
			if !t.Keys(key) {
				continue
			}
			r := reach[key]
			out = append(out, orrery.Entity{Blueprint: t.Blueprint, Key: key, Fields: map[string]any{UsesField: r.Count, ComputersField: r.Machines, InstalledField: installedOn[key], UsedField: updated}})
		}
	}
	return out
}

// UsesWriter writes each plugin's and skill's use onto its entity in the
// hub, run like a collector.
type UsesWriter struct {
	hub      *orrery.Client
	reader   *orrery.Client
	agents   *agents.Collector
	targets  []Target
	inst     *collect.Instruments
	interval time.Duration
	log      *slog.Logger
	offered  bool
}

// UsesFromEnv builds the writer, or nil when ORRERY_USES, ORRERY_URL,
// ORRERY_TOKEN (a source token, to write), or ORRERY_READ_TOKEN (one that
// reads, to list the entities) is unset, or the agents collector is off.
func UsesFromEnv(log *slog.Logger, inst *collect.Instruments, a *agents.Collector) (*UsesWriter, error) {
	spec := os.Getenv("ORRERY_USES")
	url, token, read := os.Getenv("ORRERY_URL"), os.Getenv("ORRERY_TOKEN"), os.Getenv("ORRERY_READ_TOKEN")
	switch {
	case spec == "":
		log.Info("uses writer off", "reason", "ORRERY_USES is unset")
		return nil, nil
	case url == "" || token == "" || read == "":
		log.Info("uses writer off", "reason", "ORRERY_URL, ORRERY_TOKEN, and ORRERY_READ_TOKEN are not all set")
		return nil, nil
	case a == nil:
		log.Info("uses writer off", "reason", "it needs the agents collector")
		return nil, nil
	}
	targets, err := ParseTargets(spec)
	if err != nil {
		return nil, err
	}
	return &UsesWriter{
		hub:      orrery.New(url, token, orrery.WithLogger(log)),
		reader:   orrery.New(url, read, orrery.WithLogger(log)),
		agents:   a,
		targets:  targets,
		inst:     inst,
		interval: collect.Interval(os.Getenv("CATALOG_INTERVAL"), 15*time.Minute),
		log:      log,
	}, nil
}

func (w *UsesWriter) Name() string { return "uses" }

// Run writes every interval.
func (w *UsesWriter) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, w.Name(), w.interval, w.log, done, w.Round)
}

// Round counts the window's uses and writes them onto every target's
// entities.
func (w *UsesWriter) Round(ctx context.Context) error {
	now := time.Now()
	rows, err := w.agents.UseRows(ctx, now.Add(-UsesWindow), now.Add(time.Minute))
	if err != nil {
		return fmt.Errorf("uses: %w", err)
	}
	if !w.offered {
		_, err := w.hub.OfferBlueprints(ctx, UsesBlueprints(w.targets))
		w.inst.Upstream(ctx, Upstream, outcome(err))
		if err != nil {
			return fmt.Errorf("offer blueprints: %w", err)
		}
		w.offered = true
	}
	existing := map[string][]string{}
	for _, t := range w.targets {
		records, err := w.reader.Entities(ctx, orrery.Query{Blueprint: t.Blueprint})
		w.inst.Upstream(ctx, Upstream, outcome(err))
		if err != nil {
			return fmt.Errorf("list %s: %w", t.Blueprint, err)
		}
		for _, r := range records {
			existing[t.Blueprint] = append(existing[t.Blueprint], r.Key)
		}
	}
	installed, err := w.agents.InstalledOn(ctx, now.Add(-UsesWindow))
	if err != nil {
		return fmt.Errorf("installed: %w", err)
	}
	entities := UsesEntities(w.targets, rows, installed, existing, now)
	if len(entities) == 0 {
		w.log.InfoContext(ctx, "uses written", "entities", 0, "reason", "the hub has none of the target blueprints' entities")
		return nil
	}
	res, err := w.hub.Incremental(ctx, UsesScope, func(add func(orrery.Entity) error) error {
		for _, e := range entities {
			if err := add(e); err != nil {
				return err
			}
		}
		return nil
	}, orrery.EnrichOnly(), orrery.RunObservedAt(now))
	w.inst.Upstream(ctx, Upstream, outcome(err))
	if err != nil {
		return err
	}
	w.log.InfoContext(ctx, "uses written", "entities", len(entities), "updated", res.Counts.Updated, "unchanged", res.Counts.Unchanged, "rejected", res.Counts.Rejected)
	return nil
}
