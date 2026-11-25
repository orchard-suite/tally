// Package collect is the frame every collector hangs on: one interface, a
// runner that counts every round by outcome, and the budget counter for
// every paid or rate-limited upstream a collector asks.
package collect

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Service is the service name on every span, metric, and log line.
const Service = "tally"

// Meter is the one meter every tally instrument is made on.
func Meter() metric.Meter {
	return otel.Meter(Service)
}

// Collector gathers one source's numbers on its own schedule. Run collects
// until ctx ends and calls done after every round with whether it worked.
// Interval is how often a round runs while rounds work.
type Collector interface {
	Name() string
	Interval() time.Duration
	Run(ctx context.Context, done func(ok bool)) error
}

// Instruments are the counters shared by every collector, and where each
// one stands.
type Instruments struct {
	runs        metric.Int64Counter
	lastSuccess metric.Int64Gauge
	upstream    metric.Int64Counter

	mu        sync.Mutex
	status    map[string]*Status
	intervals map[string]time.Duration
}

// Status is where one collector stands, for the board.
type Status struct {
	Name        string     `json:"name"`
	Rounds      int        `json:"rounds"`
	Failed      int        `json:"failed"`
	LastRun     *time.Time `json:"last_run,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
}

// Statuses lists every running collector, by name.
func (i *Instruments) Statuses() []Status {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]Status, 0, len(i.status))
	for _, s := range i.status {
		out = append(out, *s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func (i *Instruments) note(name string, ok bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.status[name]
	now := time.Now().UTC()
	s.Rounds++
	s.LastRun = &now
	if ok {
		s.LastSuccess = &now
	} else {
		s.Failed++
	}
}

// NewInstruments registers tally.collector.run, tally.collector.last_success,
// tally.collector.interval, and tally.upstream.requests.
func NewInstruments() (*Instruments, error) {
	m := Meter()
	runs, err := m.Int64Counter("tally.collector.run", metric.WithDescription("Collector rounds, by collector and outcome (ok or failed)."))
	if err != nil {
		return nil, err
	}
	last, err := m.Int64Gauge("tally.collector.last_success", metric.WithUnit("s"), metric.WithDescription("When the collector last finished a round that worked, in unix seconds."))
	if err != nil {
		return nil, err
	}
	up, err := m.Int64Counter("tally.upstream.requests", metric.WithDescription("Requests to each upstream a collector pays for or is rate-limited by, by outcome: ok, refused (a 429 or quota answer), or failed."))
	if err != nil {
		return nil, err
	}
	i := &Instruments{runs: runs, lastSuccess: last, upstream: up, status: map[string]*Status{}, intervals: map[string]time.Duration{}}
	if _, err := m.Int64ObservableGauge("tally.collector.interval", metric.WithUnit("s"),
		metric.WithDescription("How often the collector runs a round while its rounds work, in seconds."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for name, every := range i.Intervals() {
				o.Observe(int64(every/time.Second), metric.WithAttributes(attribute.String("collector", name)))
			}
			return nil
		})); err != nil {
		return nil, err
	}
	return i, nil
}

// Intervals is every running collector's interval, by name.
func (i *Instruments) Intervals() map[string]time.Duration {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make(map[string]time.Duration, len(i.intervals))
	for name, every := range i.intervals {
		out[name] = every
	}
	return out
}

// Upstream outcomes.
const (
	OK      = "ok"
	Refused = "refused"
	Failed  = "failed"
)

// Upstream counts one request to a budgeted upstream.
func (i *Instruments) Upstream(ctx context.Context, name, outcome string) {
	i.upstream.Add(ctx, 1, metric.WithAttributes(attribute.String("upstream", name), attribute.String("outcome", outcome)))
}

// Start runs every collector until ctx ends, each in its own goroutine,
// counting every round.
func (i *Instruments) Start(ctx context.Context, log *slog.Logger, collectors []Collector) {
	for _, c := range collectors {
		i.mu.Lock()
		i.status[c.Name()] = &Status{Name: c.Name()}
		i.intervals[c.Name()] = c.Interval()
		i.mu.Unlock()
		go func() {
			name := attribute.String("collector", c.Name())
			done := func(ok bool) {
				i.note(c.Name(), ok)
				outcome := OK
				if !ok {
					outcome = Failed
				} else {
					i.lastSuccess.Record(ctx, time.Now().Unix(), metric.WithAttributes(name))
				}
				i.runs.Add(ctx, 1, metric.WithAttributes(name, attribute.String("outcome", outcome)))
			}
			log.Info("collector started", "collector", c.Name())
			if err := c.Run(ctx, done); err != nil && ctx.Err() == nil {
				log.Error("collector stopped", "collector", c.Name(), "error", err)
			}
		}()
	}
}

// Every runs round now and then on every tick of interval until ctx ends.
// Each round is a root span of its own, so its log lines carry a trace id,
// and a round has at most the interval to finish.
func Every(ctx context.Context, name string, interval time.Duration, log *slog.Logger, done func(ok bool), round func(ctx context.Context) error) error {
	tracer := otel.Tracer(Service)
	run := func() {
		rctx, span := tracer.Start(ctx, "collect "+name, trace.WithNewRoot(), trace.WithAttributes(attribute.String("task", name)))
		rctx, cancel := context.WithTimeout(rctx, interval)
		started := time.Now()
		err := round(rctx)
		cancel()
		if err != nil {
			span.RecordError(err)
			log.ErrorContext(rctx, "collector round failed", "collector", name, "error", err, "took_ms", time.Since(started).Milliseconds())
		} else {
			log.InfoContext(rctx, "collector round done", "collector", name, "took_ms", time.Since(started).Milliseconds())
		}
		span.End()
		done(err == nil)
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			run()
		}
	}
}

// Zone is where tally's days and months begin: TIMEZONE, an IANA zone such
// as America/Chicago, or UTC.
func Zone() (*time.Location, error) {
	name := os.Getenv("TIMEZONE")
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("TIMEZONE: %w", err)
	}
	return loc, nil
}

// Interval reads a duration setting, or falls back.
func Interval(value string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(value); err == nil && d > 0 {
		return d
	}
	return fallback
}
