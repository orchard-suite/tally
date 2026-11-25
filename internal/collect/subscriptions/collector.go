package subscriptions

import (
	"context"
	"time"
)

// Collector runs usage-probe's serve loop as one of tally's collectors. The
// loop keeps its own interval (USAGE_PROBE_EVERY), its own OTLP push under
// the service name usage-probe, and the build-account rotation.
type Collector struct {
	root string
}

// FromEnv builds the collector, or nil when USAGE_PROBE_ACCOUNTS is unset.
func FromEnv() *Collector {
	if !Enabled() {
		return nil
	}
	return &Collector{root: Root()}
}

func (c *Collector) Name() string { return "subscriptions" }

// Interval is how often a round runs. A setting serve cannot read reads as
// zero, and serve stops on it.
func (c *Collector) Interval() time.Duration {
	every, _ := Every()
	return every
}

func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return Serve(ctx, c.root, done)
}
