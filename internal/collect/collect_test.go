package collect

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRetry(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     []time.Duration
	}{
		{6 * time.Hour, []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 3 * time.Hour, 3 * time.Hour}},
		{2 * time.Hour, []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, time.Hour}},
		{15 * time.Minute, []time.Duration{225 * time.Second, 450 * time.Second, 450 * time.Second}},
		{time.Minute, []time.Duration{15 * time.Second, 30 * time.Second, 30 * time.Second}},
	}
	for _, c := range cases {
		for i, want := range c.want {
			if got := Retry(c.interval, i+1); got != want {
				t.Errorf("Retry(%s, %d) = %s, want %s", c.interval, i+1, got, want)
			}
			if got := Retry(c.interval, i+1); got >= c.interval {
				t.Errorf("Retry(%s, %d) = %s, not under the interval", c.interval, i+1, got)
			}
		}
	}
}

// A failed round is tried again after Retry's wait, not a whole interval,
// and the round after one that works waits the whole interval again.
func TestEveryRetriesAFailedRoundSooner(t *testing.T) {
	const interval = 800 * time.Millisecond // first retry is 200ms
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var at []time.Time
	var outcomes []bool
	round := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		if len(at) == 3 {
			cancel()
		}
		if len(at) == 1 {
			return errors.New("refused")
		}
		return nil
	}
	done := func(ok bool) {
		mu.Lock()
		defer mu.Unlock()
		outcomes = append(outcomes, ok)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if err := Every(ctx, "test", interval, log, done, round); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 3 {
		t.Fatalf("ran %d rounds, want 3", len(at))
	}
	if retry := at[1].Sub(at[0]); retry > interval/2 {
		t.Errorf("failed round tried again after %s, want about %s", retry, Retry(interval, 1))
	}
	if next := at[2].Sub(at[1]); next < interval*9/10 {
		t.Errorf("round after a success ran after %s, want the interval %s", next, interval)
	}
	if want := []bool{false, true, true}; len(outcomes) != 3 || outcomes[0] != want[0] || outcomes[1] != want[1] {
		t.Errorf("outcomes = %v, want %v", outcomes, want)
	}
}

type fixed struct {
	name     string
	interval time.Duration
}

func (f fixed) Name() string            { return f.name }
func (f fixed) Interval() time.Duration { return f.interval }
func (f fixed) Run(ctx context.Context, _ func(bool)) error {
	<-ctx.Done()
	return nil
}

func TestIntervalGauge(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	defer otel.SetMeterProvider(prev)

	inst, err := NewInstruments()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inst.Start(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)), []Collector{fixed{"azure", 6 * time.Hour}, fixed{"power", time.Minute}})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "tally.collector.interval" {
				continue
			}
			if m.Unit != "s" {
				t.Errorf("unit = %q, want s", m.Unit)
			}
			for _, p := range m.Data.(metricdata.Gauge[int64]).DataPoints {
				name, _ := p.Attributes.Value("collector")
				got[name.AsString()] = p.Value
			}
		}
	}
	if got["azure"] != 6*3600 || got["power"] != 60 || len(got) != 2 {
		t.Errorf("tally.collector.interval = %v, want azure 21600 and power 60", got)
	}
}
