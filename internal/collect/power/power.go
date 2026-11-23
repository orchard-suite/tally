// Package power reads watts per host and prices them. A host's watts come
// from a Home Assistant sensor (HASS_URL, HASS_TOKEN, HASS_POWER_ENTITIES) or
// from a Prometheus query whose series carry a host label (PROMETHEUS_URL,
// POWER_PROMETHEUS_QUERY), such as a host's own IPMI power meter. Both may be
// on; a host read both ways adds them.
package power

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
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

// Host is one machine's draw and what it costs.
type Host struct {
	Host        string  `json:"host"`
	Watts       float64 `json:"watts"`
	CostPerHour float64 `json:"cost_per_hour"`
}

// Snapshot is one round's hosts.
type Snapshot struct {
	At          time.Time `json:"at"`
	PricePerKWh float64   `json:"price_per_kwh"`
	Hosts       []Host    `json:"hosts"`
}

// Mapping is one Home Assistant entity and the host it meters. Several
// entities may meter one host; their watts add.
type Mapping struct {
	Entity, Host string
}

// ParseMappings reads HASS_POWER_ENTITIES: comma separated entity_id=host.
func ParseMappings(s string) ([]Mapping, error) {
	var out []Mapping
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		entity, host, ok := strings.Cut(part, "=")
		entity, host = strings.TrimSpace(entity), strings.TrimSpace(host)
		if !ok || entity == "" || host == "" {
			return nil, fmt.Errorf("HASS_POWER_ENTITIES: want entity_id=host, got %q", part)
		}
		out = append(out, Mapping{Entity: entity, Host: host})
	}
	if len(out) == 0 {
		return nil, errors.New("HASS_POWER_ENTITIES names no entity")
	}
	return out, nil
}

// State is what Home Assistant says about one entity.
type State struct {
	State      string `json:"state"`
	Attributes struct {
		Unit string `json:"unit_of_measurement"`
	} `json:"attributes"`
}

// Watts reads a power state in W or kW. unknown and unavailable are errors.
func Watts(s State) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s.State), 64)
	if err != nil {
		return 0, fmt.Errorf("state %q is not a number", s.State)
	}
	switch strings.ToLower(s.Attributes.Unit) {
	case "w", "":
		return v, nil
	case "kw":
		return v * 1000, nil
	}
	return 0, fmt.Errorf("unit %q is not W or kW", s.Attributes.Unit)
}

// Shape sums each host's entities and prices them. metered is what a
// Prometheus query reported by host; it adds to the same host.
func Shape(mappings []Mapping, watts map[string]float64, metered map[string]float64, pricePerKWh float64) []Host {
	byHost := map[string]float64{}
	for _, m := range mappings {
		w, ok := watts[m.Entity]
		if !ok {
			continue
		}
		byHost[m.Host] += w
	}
	for host, w := range metered {
		byHost[host] += w
	}
	out := make([]Host, 0, len(byHost))
	for host, w := range byHost {
		out = append(out, Host{Host: host, Watts: w, CostPerHour: w / 1000 * pricePerKWh})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Metered reads a Prometheus vector into watts by host. A sample without a
// host label is skipped; a host with several samples adds them.
func Metered(samples []promapi.Sample) map[string]float64 {
	out := map[string]float64{}
	for _, s := range samples {
		host := s.Labels["host"]
		if host == "" {
			continue
		}
		out[host] += s.Value
	}
	return out
}

// Collector is the power collector.
type Collector struct {
	url, token string
	mappings   []Mapping
	prom       *promapi.Client
	query      string
	price      float64
	interval   time.Duration
	http       *http.Client
	log        *slog.Logger
	inst       *collect.Instruments
	latest     atomic.Pointer[Snapshot]
}

// FromEnv builds the collector, or nil when neither Home Assistant nor a
// Prometheus power query is configured.
func FromEnv(log *slog.Logger, inst *collect.Instruments) (*Collector, error) {
	url, token, entities := os.Getenv("HASS_URL"), os.Getenv("HASS_TOKEN"), os.Getenv("HASS_POWER_ENTITIES")
	promURL, query := os.Getenv("PROMETHEUS_URL"), os.Getenv("POWER_PROMETHEUS_QUERY")
	hass := url != "" || token != "" || entities != ""
	if !hass && query == "" {
		return nil, nil
	}
	if hass && (url == "" || token == "" || entities == "") {
		return nil, errors.New("power needs HASS_URL, HASS_TOKEN, and HASS_POWER_ENTITIES together")
	}
	if query != "" && promURL == "" {
		return nil, errors.New("POWER_PROMETHEUS_QUERY needs PROMETHEUS_URL")
	}
	var mappings []Mapping
	if hass {
		var err error
		if mappings, err = ParseMappings(entities); err != nil {
			return nil, err
		}
	}
	price, err := Price(os.Getenv("ELECTRICITY_PRICE_PER_KWH"))
	if err != nil {
		return nil, err
	}
	if price == 0 {
		log.Warn("ELECTRICITY_PRICE_PER_KWH is unset; power is measured but costs nothing")
	}
	c := &Collector{
		url: strings.TrimRight(url, "/"), token: token, mappings: mappings, query: query, price: price,
		interval: collect.Interval(os.Getenv("POWER_INTERVAL"), time.Minute),
		http:     &http.Client{Timeout: 20 * time.Second},
		log:      log, inst: inst,
	}
	if query != "" {
		if c.prom, err = azure.Prometheus(promURL); err != nil {
			return nil, err
		}
	}
	return c, c.register()
}

// Price reads ELECTRICITY_PRICE_PER_KWH; empty is 0.
func Price(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	p, err := strconv.ParseFloat(s, 64)
	if err != nil || p < 0 {
		return 0, errors.New("ELECTRICITY_PRICE_PER_KWH must be a price such as 0.14")
	}
	return p, nil
}

func (c *Collector) Name() string { return "power" }

// Latest is the last round's hosts, or nil before the first.
func (c *Collector) Latest() *Snapshot { return c.latest.Load() }

// PricePerKWh is the configured price.
func (c *Collector) PricePerKWh() float64 { return c.price }

func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.round)
}

// round reads every entity and runs the query. A host that cannot be read
// is left out of the round, and the round fails, so the gap shows.
func (c *Collector) round(ctx context.Context) error {
	watts := map[string]float64{}
	var errs []error
	for _, m := range c.mappings {
		w, err := c.read(ctx, m.Entity)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Entity, err))
			continue
		}
		watts[m.Entity] = w
	}
	metered := map[string]float64{}
	if c.prom != nil {
		samples, err := c.prom.Query(ctx, c.query)
		if err != nil {
			c.inst.Upstream(ctx, "prometheus", collect.Failed)
			errs = append(errs, fmt.Errorf("prometheus: %w", err))
		} else {
			c.inst.Upstream(ctx, "prometheus", collect.OK)
			metered = Metered(samples)
		}
	}
	snap := &Snapshot{At: time.Now().UTC(), PricePerKWh: c.price, Hosts: Shape(c.mappings, watts, metered, c.price)}
	c.latest.Store(snap)
	c.log.InfoContext(ctx, "power read", "hosts", len(snap.Hosts), "entities", len(watts), "metered", len(metered))
	return errors.Join(errs...)
}

func (c *Collector) read(ctx context.Context, entity string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/api/states/"+entity, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.http.Do(req)
	if err != nil {
		c.inst.Upstream(ctx, "home-assistant", collect.Failed)
		return 0, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		outcome := collect.Failed
		if res.StatusCode == http.StatusTooManyRequests {
			outcome = collect.Refused
		}
		c.inst.Upstream(ctx, "home-assistant", outcome)
		return 0, fmt.Errorf("home assistant: %s", res.Status)
	}
	c.inst.Upstream(ctx, "home-assistant", collect.OK)
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("home assistant state: %w", err)
	}
	return Watts(s)
}

func (c *Collector) register() error {
	m := collect.Meter()
	watts, err := m.Float64ObservableGauge("tally.power.watts", metric.WithUnit("W"), metric.WithDescription("Power a host draws now, from its Home Assistant sensor or its own meter in Prometheus."))
	if err != nil {
		return err
	}
	cost, err := m.Float64ObservableGauge("tally.power.cost_per_hour", metric.WithDescription("What a host's draw costs per hour at ELECTRICITY_PRICE_PER_KWH."))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		snap := c.latest.Load()
		if snap == nil {
			return nil
		}
		for _, h := range snap.Hosts {
			who := metric.WithAttributes(attribute.String("host", h.Host))
			o.ObserveFloat64(watts, h.Watts, who)
			o.ObserveFloat64(cost, h.CostPerHour, who)
		}
		return nil
	}, watts, cost)
	return err
}
