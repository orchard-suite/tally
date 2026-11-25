package collect

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

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
