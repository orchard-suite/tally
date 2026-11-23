// Package billed is the reported lens: what the bill says the estate spent,
// month by month, read from the Orrery catalog the estate's billing
// pipeline pushes into. A month appears once the pipeline has run it,
// after the month closes. tally calculates nothing here; it reads the
// billed figures as the pipeline wrote them, in whole cents.
package billed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync/atomic"
	"time"

	"github.com/orchard-suite/platformkit/orrery"
	"go.opentelemetry.io/otel/metric"

	"github.com/orchard-suite/tally/internal/collect"
)

// The blueprints the billing pipeline writes.
const (
	PeriodBlueprint  = "finops_billing_period"
	ProgramBlueprint = "finops_program_portfolio_cost"
)

// Month is one billed month.
type Month struct {
	// Period is the month as year and month, such as 2026-09.
	Period string `json:"period"`
	// AI is the AI part of the month's bill, in dollars.
	AI float64 `json:"ai"`
	// AIModels is the part of AI billed for model use, the part an AI
	// gateway's calls drive, when the pipeline wrote it; the rest of AI,
	// such as an assistant product billed apart, makes no model call.
	AIModels *float64 `json:"ai_models,omitempty"`
	// Usage is the whole month's billed usage, AI included, in dollars.
	Usage float64 `json:"usage"`
	// Invoice is where the month's invoice stands, as the pipeline says it.
	Invoice string `json:"invoice"`
	// Programs are the month's AI by program, the largest first; programs
	// with no AI are left out.
	Programs []Program `json:"programs"`
}

// Program is one program's AI in a month.
type Program struct {
	Program string  `json:"program"`
	AI      float64 `json:"ai"`
}

// Snapshot is one round's months, oldest first.
type Snapshot struct {
	At     time.Time `json:"at"`
	Months []Month   `json:"months"`
}

// Latest is the newest billed month, or nil.
func (s *Snapshot) Latest() *Month {
	if s == nil || len(s.Months) == 0 {
		return nil
	}
	return &s.Months[len(s.Months)-1]
}

// Month is the billed month with period, or nil.
func (s *Snapshot) Month(period string) *Month {
	if s == nil {
		return nil
	}
	for i := range s.Months {
		if s.Months[i].Period == period {
			return &s.Months[i]
		}
	}
	return nil
}

// Shape builds the months from the pipeline's billing periods and program
// costs. A program cost names its month by its period relation.
func Shape(periods, programs []orrery.Record, at time.Time) (Snapshot, error) {
	byKey := map[string]*Month{}
	var months []*Month
	for _, r := range periods {
		period := r.String("period")
		if period == "" {
			return Snapshot{}, fmt.Errorf("billing period %s has no period", r.Key)
		}
		ai, ok := r.Number("ai_cents")
		if !ok {
			return Snapshot{}, fmt.Errorf("billing period %s has no ai_cents", period)
		}
		usage, _ := r.Number("usage_cents")
		m := &Month{Period: period, AI: ai / 100, Usage: usage / 100, Invoice: r.String("invoice_state"), Programs: []Program{}}
		if models, ok := r.Number("ai_models_cents"); ok {
			v := models / 100
			m.AIModels = &v
		}
		byKey[r.Key] = m
		months = append(months, m)
	}
	for _, r := range programs {
		ai, _ := r.Number("ai_cents")
		if ai == 0 {
			continue
		}
		period, ok := r.Related("period")
		if !ok {
			continue
		}
		m := byKey[period.Key]
		if m == nil {
			continue
		}
		name := r.Title
		if p, ok := r.Related("program_portfolio"); ok && p.Title != "" {
			name = p.Title
		}
		m.Programs = append(m.Programs, Program{Program: name, AI: ai / 100})
	}
	out := Snapshot{At: at, Months: make([]Month, 0, len(months))}
	for _, m := range months {
		sort.Slice(m.Programs, func(a, b int) bool {
			if m.Programs[a].AI != m.Programs[b].AI {
				return m.Programs[a].AI > m.Programs[b].AI
			}
			return m.Programs[a].Program < m.Programs[b].Program
		})
		out.Months = append(out.Months, *m)
	}
	sort.Slice(out.Months, func(a, b int) bool { return out.Months[a].Period < out.Months[b].Period })
	return out, nil
}

// Collector reads the billed months every interval.
type Collector struct {
	hub      *orrery.Client
	log      *slog.Logger
	inst     *collect.Instruments
	interval time.Duration
	latest   atomic.Pointer[Snapshot]
}

// FromEnv turns the collector on with ORRERY_URL and ORRERY_READ_TOKEN, an
// Orrery access token that reads. It is off without them.
func FromEnv(log *slog.Logger, inst *collect.Instruments) (*Collector, error) {
	url, token := os.Getenv("ORRERY_URL"), os.Getenv("ORRERY_READ_TOKEN")
	if url == "" || token == "" {
		log.Info("billed collector off: ORRERY_URL and ORRERY_READ_TOKEN are not both set")
		return nil, nil
	}
	c := &Collector{hub: orrery.New(url, token, orrery.WithLogger(log)), log: log, inst: inst,
		interval: collect.Interval(os.Getenv("BILLED_INTERVAL"), time.Hour)}
	return c, c.register()
}

// Name is the collector's name.
func (c *Collector) Name() string { return "billed" }

// Latest is the last round's months, nil before the first round worked.
func (c *Collector) Latest() *Snapshot { return c.latest.Load() }

// Run reads every interval until ctx ends.
func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.round)
}

func (c *Collector) round(ctx context.Context) error {
	periods, err := c.read(ctx, PeriodBlueprint)
	if err != nil {
		return err
	}
	programs, err := c.read(ctx, ProgramBlueprint)
	if err != nil {
		return err
	}
	snap, err := Shape(periods, programs, time.Now().UTC())
	if err != nil {
		return err
	}
	if len(snap.Months) == 0 {
		return errors.New("the catalog holds no billed month")
	}
	c.latest.Store(&snap)
	return nil
}

func (c *Collector) read(ctx context.Context, blueprint string) ([]orrery.Record, error) {
	recs, err := c.hub.Entities(ctx, orrery.Query{Blueprint: blueprint})
	if err != nil {
		c.inst.Upstream(ctx, "orrery", collect.Failed)
		return nil, fmt.Errorf("%s: %w", blueprint, err)
	}
	c.inst.Upstream(ctx, "orrery", collect.OK)
	return recs, nil
}

func (c *Collector) register() error {
	m := collect.Meter()
	ai, err := m.Float64ObservableGauge("tally.billed.ai", metric.WithDescription("The AI part of the newest billed month, in dollars, as the billing pipeline wrote it."))
	if err != nil {
		return err
	}
	usage, err := m.Float64ObservableGauge("tally.billed.usage", metric.WithDescription("The newest billed month's whole usage, AI included, in dollars."))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if latest := c.Latest().Latest(); latest != nil {
			o.ObserveFloat64(ai, latest.AI)
			o.ObserveFloat64(usage, latest.Usage)
		}
		return nil
	}, ai, usage)
	return err
}
