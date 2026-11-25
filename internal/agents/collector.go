package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/orchard-suite/tally/internal/collect"
)

// Collector keeps the agent store: it takes shippers' posts, refreshes the
// price table, and exports this month's usage and cost.
type Collector struct {
	store    *Store
	log      *slog.Logger
	inst     *collect.Instruments
	machines map[string]string
	owners   map[string]string
	billing  string
	zone     *time.Location
	every    time.Duration
	pricesAt string
	client   *http.Client
	batches  metric.Int64Counter

	mu sync.Mutex
	// enrolled are the machines an enrollment source named, by the hash
	// of each one's token, beside the ones SHIP_TOKENS names.
	enrolled map[string]string
	prices   Prices
	pricedAt time.Time
	snap     snapshot
}

type snapshot struct {
	// uses are this month's skills, commands, and tools.
	uses   []UseSum
	sums   []Sum
	posts  map[string]time.Time
	spend  Spend
	months []MonthModel
}

// MonthModel is what every machine posted of one model in one month.
type MonthModel struct {
	// Month is the month in the collector's zone, such as 2026-10.
	Month string `json:"month"`
	// Model is the bare model name.
	Model string `json:"model"`
	Tokens
}

// Months is how many whole months before the current one the collector
// keeps by month and model: as many as the board shows billed.
const Months = 6

// ByMonth adds sums up by bare model, under month.
func ByMonth(month string, sums []Sum) []MonthModel {
	by := map[string]Tokens{}
	for _, s := range sums {
		t := by[Model(s.Model)]
		t.Add(s.Tokens)
		by[Model(s.Model)] = t
	}
	out := make([]MonthModel, 0, len(by))
	for m, t := range by {
		out = append(out, MonthModel{Month: month, Model: m, Tokens: t})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Model < out[b].Model })
	return out
}

// Billing modes: how agent work is paid for.
const (
	// Subscription work is paid by plan; its cost at API prices is what it
	// would have cost.
	Subscription = "subscription"
	// API work is billed by the token; its cost at API prices is spend.
	API = "api"
)

// Spend is agent work at API prices over the periods the estate shows.
type Spend struct {
	// Trailing is the last 30 days: a monthly run rate.
	Trailing    float64 `json:"trailing"`
	MonthToDate float64 `json:"month_to_date"`
	Previous    float64 `json:"previous"`
}

// pricesEvery is how often the price table is read again.
const pricesEvery = 24 * time.Hour

// FromEnv turns the collector on with AGENTS_DB (the store's path) and
// SHIP_TOKENS (machine=sha256 of its token, comma separated), and names
// each machine's owner from MACHINE_OWNERS (machine=owner, comma
// separated). AGENTS_BILLING says how the work is paid for: subscription
// (the default) or api, which makes its cost part of the estate. It is off
// without AGENTS_DB.
func FromEnv(log *slog.Logger, inst *collect.Instruments) (*Collector, error) {
	path := os.Getenv("AGENTS_DB")
	if path == "" {
		log.Info("agents collector off: AGENTS_DB is not set")
		return nil, nil
	}
	machines, err := ParseTokens(os.Getenv("SHIP_TOKENS"))
	if err != nil {
		return nil, err
	}
	if len(machines) == 0 {
		log.Warn("agents collector takes no posts: SHIP_TOKENS is empty")
	}
	owners, err := ParseOwners(os.Getenv("MACHINE_OWNERS"))
	if err != nil {
		return nil, err
	}
	zone, err := collect.Zone()
	if err != nil {
		return nil, err
	}
	billing := os.Getenv("AGENTS_BILLING")
	switch billing {
	case "":
		billing = Subscription
	case Subscription, API:
	default:
		return nil, fmt.Errorf("AGENTS_BILLING: want %s or %s, got %q", Subscription, API, billing)
	}
	store, err := OpenStore(path)
	if err != nil {
		return nil, err
	}
	c := &Collector{store: store, log: log, inst: inst, machines: machines, owners: owners, billing: billing, zone: zone,
		every: collect.Interval(os.Getenv("AGENTS_INTERVAL"), 5*time.Minute), pricesAt: os.Getenv("PRICES_URL"),
		client: &http.Client{Timeout: time.Minute}}
	if c.pricesAt == "" {
		c.pricesAt = PricesURL
	}
	if err := c.register(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseTokens reads SHIP_TOKENS: machine=hex sha256 of that machine's
// token, comma separated. The map is the hash to its machine.
func ParseTokens(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		machine, hash, ok := strings.Cut(part, "=")
		machine, hash = strings.TrimSpace(machine), strings.ToLower(strings.TrimSpace(hash))
		if b, err := hex.DecodeString(hash); !ok || machine == "" || err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("SHIP_TOKENS: want machine=<sha256 hex>, got an entry for %q", machine)
		}
		out[hash] = machine
	}
	return out, nil
}

// ParseOwners reads MACHINE_OWNERS: machine=owner, comma separated, where
// the owner is how the board names the person a machine belongs to.
func ParseOwners(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		machine, owner, ok := strings.Cut(part, "=")
		machine, owner = strings.TrimSpace(machine), strings.TrimSpace(owner)
		if !ok || machine == "" || owner == "" {
			return nil, fmt.Errorf("MACHINE_OWNERS: want machine=owner, got %q", part)
		}
		out[machine] = owner
	}
	return out, nil
}

// Name is the collector's name.
func (c *Collector) Name() string { return "agents" }

// Interval is how often a round runs when the last one worked.
func (c *Collector) Interval() time.Duration { return c.every }

// Store is the agent store, for the board.
func (c *Collector) Store() *Store { return c.store }

// Prices is the price table as last read, empty before the first read.
func (c *Collector) Prices() Prices {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prices
}

// Run refreshes on every interval until ctx ends.
func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	defer c.store.Close()
	return collect.Every(ctx, c.Name(), c.every, c.log, done, c.round)
}

func (c *Collector) round(ctx context.Context) error {
	var errs []error
	c.mu.Lock()
	stale := time.Since(c.pricedAt) > pricesEvery
	c.mu.Unlock()
	if stale {
		p, err := c.readPrices(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("prices: %w", err))
		} else {
			c.mu.Lock()
			c.prices, c.pricedAt = p, time.Now()
			c.mu.Unlock()
			c.log.InfoContext(ctx, "prices read", "models", len(p))
		}
	}
	now := time.Now().In(c.zone)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, c.zone)
	end := now.Add(time.Minute)
	sums, err := c.store.Sums(ctx, month, end)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	posts, err := c.store.LastPosts(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	uses, err := c.store.UseSums(ctx, month, end)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	trailing, err := c.store.Sums(ctx, now.AddDate(0, 0, -30), end)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	previous, err := c.store.Sums(ctx, month.AddDate(0, -1, 0), month)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	var months []MonthModel
	for i := Months; i >= 0; i-- {
		start := month.AddDate(0, -i, 0)
		stop := start.AddDate(0, 1, 0)
		if i == 0 {
			stop = end
		}
		ms, err := c.store.Sums(ctx, start, stop)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		months = append(months, ByMonth(start.Format("2006-01"), ms)...)
	}
	c.mu.Lock()
	prices := c.prices
	c.snap = snapshot{uses: uses, sums: sums, posts: posts, months: months, spend: Spend{Trailing: Price(trailing, prices), MonthToDate: Price(sums, prices), Previous: Price(previous, prices)}}
	c.mu.Unlock()
	return errors.Join(errs...)
}

// Price is what sums cost at prices; a model with no price adds nothing.
func Price(sums []Sum, prices Prices) float64 {
	total := 0.0
	for _, s := range sums {
		if r, ok := prices[Model(s.Model)]; ok {
			total += r.Cost(s.Tokens)
		}
	}
	return total
}

// Shipped is what every machine posted, by month and bare model, over the
// current month and the Months before it; nil before the first round.
func (c *Collector) Shipped() []MonthModel {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.snap.months)
}

// Billing is how the work is paid for: Subscription or API.
func (c *Collector) Billing() string { return c.billing }

// Spend is the agent work's spend for the estate, when it is billed by the
// token, and nil when a plan pays for it or no round has priced it yet.
func (c *Collector) Spend() *Spend {
	if c.billing != API {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prices == nil {
		return nil
	}
	s := c.snap.spend
	return &s
}

func (c *Collector) readPrices(ctx context.Context) (Prices, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.pricesAt, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.inst.Upstream(ctx, "prices", collect.Failed)
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		c.inst.Upstream(ctx, "prices", collect.Refused)
		return nil, fmt.Errorf("prices refused: %s", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		c.inst.Upstream(ctx, "prices", collect.Failed)
		return nil, fmt.Errorf("prices: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		c.inst.Upstream(ctx, "prices", collect.Failed)
		return nil, err
	}
	p, err := ParsePrices(raw)
	if err != nil || len(p) == 0 {
		c.inst.Upstream(ctx, "prices", collect.Failed)
		return nil, errors.Join(errors.New("prices: no models in the table"), err)
	}
	c.inst.Upstream(ctx, "prices", collect.OK)
	return p, nil
}

func (c *Collector) register() error {
	m := collect.Meter()
	batches, err := m.Int64Counter("tally.agents.batches", metric.WithDescription("Shippers' posts, by outcome: ok, refused (no token, a wrong token, or a bad batch), or failed (the store did not take it)."))
	if err != nil {
		return err
	}
	c.batches = batches
	cost, err := m.Float64ObservableGauge("tally.agents.cost", metric.WithDescription("Agent work this month at API prices in dollars, by machine, model, and role (lead or child)."))
	if err != nil {
		return err
	}
	tokens, err := m.Int64ObservableGauge("tally.agents.tokens", metric.WithDescription("Agent tokens this month, by machine, model, role, and kind (input, cache_write, cache_read, output)."))
	if err != nil {
		return err
	}
	last, err := m.Int64ObservableGauge("tally.agents.last_post", metric.WithUnit("s"), metric.WithDescription("When each machine's shipper last posted, in unix seconds."))
	if err != nil {
		return err
	}
	used, err := m.Int64ObservableGauge("tally.agents.uses", metric.WithDescription("Skills, commands, and tools agents used this month, by kind (skill, command, or tool), plugin (empty for none), and name."))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		c.mu.Lock()
		snap, prices := c.snap, c.prices
		c.mu.Unlock()
		type key struct{ machine, model, role string }
		byKey := map[key]Tokens{}
		for _, s := range snap.sums {
			k := key{s.Machine, Model(s.Model), s.Role}
			t := byKey[k]
			t.Add(s.Tokens)
			byKey[k] = t
		}
		for k, t := range byKey {
			attrs := []attribute.KeyValue{attribute.String("machine", k.machine), attribute.String("model", k.model), attribute.String("role", k.role)}
			if r, ok := prices[k.model]; ok {
				o.ObserveFloat64(cost, r.Cost(t), metric.WithAttributes(attrs...))
			}
			for kind, n := range map[string]int64{"input": t.Input, "cache_write": t.CacheWrite5m + t.CacheWrite1h, "cache_read": t.CacheRead, "output": t.Output} {
				o.ObserveInt64(tokens, n, metric.WithAttributes(append(attrs, attribute.String("kind", kind))...))
			}
		}
		for machine, at := range snap.posts {
			o.ObserveInt64(last, at.Unix(), metric.WithAttributes(attribute.String("machine", machine)))
		}
		for _, u := range snap.uses {
			o.ObserveInt64(used, u.Count, metric.WithAttributes(attribute.String("kind", u.Kind), attribute.String("plugin", u.Plugin), attribute.String("name", u.Name)))
		}
		return nil
	}, cost, tokens, last, used)
	return err
}

// maxBody is the largest post a shipper may send.
const maxBody = 8 << 20

// machine is the machine r's bearer token names, or empty.
func (c *Collector) machine(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	hash := hex.EncodeToString(sum[:])
	if m := c.machines[hash]; m != "" {
		return m
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enrolled[hash]
}

// Enroll replaces the machines an enrollment source names, by the hash of
// each one's token. A hash SHIP_TOKENS names keeps its machine.
func (c *Collector) Enroll(byHash map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enrolled = byHash
}

// Ingest is POST /v1/usage: a shipper's batch, under the machine its
// bearer token names.
func (c *Collector) Ingest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	machine := c.machine(r)
	if machine == "" {
		c.log.WarnContext(ctx, "usage post refused", "reason", "no token or an unknown token")
		c.batches.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", collect.Refused)))
		http.Error(w, "unknown token", http.StatusUnauthorized)
		return
	}
	var b Batch
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	if err := dec.Decode(&b); err != nil {
		c.refuse(w, r, machine, err)
		return
	}
	if err := b.Validate(); err != nil {
		c.refuse(w, r, machine, err)
		return
	}
	if err := c.store.Apply(ctx, machine, b, time.Now()); err != nil {
		c.log.ErrorContext(ctx, "usage post not stored", "machine", machine, "error", err)
		c.batches.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", collect.Failed)))
		http.Error(w, "not stored", http.StatusInternalServerError)
		return
	}
	c.log.InfoContext(ctx, "usage post taken", "machine", machine, "works", len(b.Works), "conversations", len(b.Conversations), "buckets", len(b.Buckets))
	c.batches.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", collect.OK)))
	w.WriteHeader(http.StatusNoContent)
}

// Withdraw is DELETE /v1/usage: everything the machine its bearer token
// names ever posted is deleted, so its shipper can post again from the
// start. A machine withdraws only its own.
func (c *Collector) Withdraw(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	machine := c.machine(r)
	if machine == "" {
		c.log.WarnContext(ctx, "usage withdrawal refused", "reason", "no token or an unknown token")
		http.Error(w, "unknown token", http.StatusUnauthorized)
		return
	}
	n, err := c.store.Withdraw(ctx, machine)
	if err != nil {
		c.log.ErrorContext(ctx, "usage not withdrawn", "machine", machine, "error", err)
		http.Error(w, "not withdrawn", http.StatusInternalServerError)
		return
	}
	c.log.InfoContext(ctx, "usage withdrawn", "machine", machine, "buckets", n)
	w.WriteHeader(http.StatusNoContent)
}

func (c *Collector) refuse(w http.ResponseWriter, r *http.Request, machine string, err error) {
	c.log.WarnContext(r.Context(), "usage post refused", "machine", machine, "reason", err.Error())
	c.batches.Add(r.Context(), 1, metric.WithAttributes(attribute.String("outcome", collect.Refused)))
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// Report is the report from from to to, at the prices last read.
func (c *Collector) Report(ctx context.Context, from, to time.Time) (Report, error) {
	rows, err := c.store.Rows(ctx, from, to)
	if err != nil {
		return Report{}, err
	}
	rep := BuildReport(rows, c.Prices(), from, to, time.Now())
	rep.Billing = c.billing
	for i := range rep.Machines {
		rep.Machines[i].Owner = c.owners[rep.Machines[i].Machine]
	}
	return rep, nil
}

// Uses are the skills, commands, and tools used from from up to to, most
// used first.
func (c *Collector) Uses(ctx context.Context, from, to time.Time) ([]UseSum, error) {
	return c.store.UseSums(ctx, from, to)
}

// InstalledOn is how many machines that posted since since have each plugin
// installed, by plugin name.
func (c *Collector) InstalledOn(ctx context.Context, since time.Time) (map[string]int64, error) {
	return c.store.InstalledOn(ctx, since)
}

// UseRows are each machine's uses from from up to to.
func (c *Collector) UseRows(ctx context.Context, from, to time.Time) ([]UseRow, error) {
	return c.store.UseRows(ctx, from, to)
}
