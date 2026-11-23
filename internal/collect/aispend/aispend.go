// Package aispend prices the AI subscriptions in dollars and, where a
// provider's usage API gives tokens, counts them. It turns on with
// SUBSCRIPTION_PRICES (a price per account), PLAN_PRICES (a price per plan,
// applied to every signed-in account the subscriptions collector reports on
// that plan), OPENAI_ADMIN_KEY, or ANTHROPIC_ADMIN_KEY.
package aispend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/subscriptions"
)

// Account is one paid AI account: its monthly price and, when known, the
// tokens it has used this month.
type Account struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	// Plan is the subscription the price came from, when priced by plan.
	Plan         string  `json:"plan,omitempty"`
	CostPerMonth float64 `json:"cost_per_month"`
	// Tokens by direction (input, output) this month. nil when the provider
	// gives none.
	Tokens map[string]int64 `json:"tokens,omitempty"`
}

// Snapshot is one round's accounts.
type Snapshot struct {
	At       time.Time `json:"at"`
	Accounts []Account `json:"accounts"`
}

// ParsePrices reads SUBSCRIPTION_PRICES: comma separated provider:account=price.
func ParsePrices(s string) ([]Account, error) {
	var out []Account
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		who, price, ok := strings.Cut(part, "=")
		provider, account, ok2 := strings.Cut(who, ":")
		if !ok || !ok2 || provider == "" || account == "" {
			return nil, fmt.Errorf("SUBSCRIPTION_PRICES: want provider:account=price, got %q", part)
		}
		p, err := strconv.ParseFloat(strings.TrimSpace(price), 64)
		if err != nil || p < 0 {
			return nil, fmt.Errorf("SUBSCRIPTION_PRICES: %q is not a price", price)
		}
		out = append(out, Account{Provider: strings.TrimSpace(provider), Account: strings.ToLower(strings.TrimSpace(account)), CostPerMonth: p})
	}
	return out, nil
}

// PlanPrice is what one provider's plan costs a month.
type PlanPrice struct {
	Provider, Plan string
	CostPerMonth   float64
}

// ParsePlanPrices reads PLAN_PRICES: comma separated provider:plan=price.
func ParsePlanPrices(s string) ([]PlanPrice, error) {
	var out []PlanPrice
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		who, price, ok := strings.Cut(part, "=")
		provider, plan, ok2 := strings.Cut(who, ":")
		if !ok || !ok2 || provider == "" || plan == "" {
			return nil, fmt.Errorf("PLAN_PRICES: want provider:plan=price, got %q", part)
		}
		p, err := strconv.ParseFloat(strings.TrimSpace(price), 64)
		if err != nil || p < 0 {
			return nil, fmt.Errorf("PLAN_PRICES: %q is not a price", price)
		}
		out = append(out, PlanPrice{Provider: strings.TrimSpace(provider), Plan: strings.ToLower(strings.TrimSpace(plan)), CostPerMonth: p})
	}
	return out, nil
}

// PriceByPlan prices every account on a plan that has a price. An account
// already priced by name keeps that price. An account on a plan with no
// price is listed unpriced, so the gap shows on the board.
func PriceByPlan(priced []Account, plans []subscriptions.Plan, prices []PlanPrice) []Account {
	out := append([]Account(nil), priced...)
	named := map[string]bool{}
	for _, a := range priced {
		named[a.Provider+":"+a.Account] = true
	}
	for _, p := range plans {
		if named[p.Provider+":"+p.Account] {
			continue
		}
		a := Account{Provider: p.Provider, Account: p.Account, Plan: p.Plan}
		for _, pp := range prices {
			if pp.Provider == p.Provider && pp.Plan == p.Plan {
				a.CostPerMonth = pp.CostPerMonth
			}
		}
		out = append(out, a)
	}
	return out
}

// usageAPI is one provider's organization usage report.
type usageAPI struct {
	provider, account, key string
	// request builds the month-to-date request.
	request func(ctx context.Context, key string, from time.Time) (*http.Request, error)
	// parse sums the tokens in one page and gives the next page, if any.
	parse func(raw []byte) (tokens map[string]int64, next string, err error)
}

// OpenAI's usage report: buckets of completions by day since start_time.
func openAIRequest(ctx context.Context, key string, from time.Time) (*http.Request, error) {
	q := url.Values{"start_time": {strconv.FormatInt(from.Unix(), 10)}, "bucket_width": {"1d"}, "limit": {"31"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.openai.com/v1/organization/usage/completions?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return req, nil
}

// ParseOpenAIUsage sums input and output tokens over a page of buckets.
func ParseOpenAIUsage(raw []byte) (map[string]int64, string, error) {
	var page struct {
		Data []struct {
			Results []struct {
				Input  int64 `json:"input_tokens"`
				Output int64 `json:"output_tokens"`
			} `json:"results"`
		} `json:"data"`
		HasMore  bool   `json:"has_more"`
		NextPage string `json:"next_page"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, "", fmt.Errorf("openai usage: %w", err)
	}
	tokens := map[string]int64{"input": 0, "output": 0}
	for _, b := range page.Data {
		for _, r := range b.Results {
			tokens["input"] += r.Input
			tokens["output"] += r.Output
		}
	}
	if page.HasMore {
		return tokens, page.NextPage, nil
	}
	return tokens, "", nil
}

// Anthropic's usage report: buckets of messages by day since starting_at.
func anthropicRequest(ctx context.Context, key string, from time.Time) (*http.Request, error) {
	q := url.Values{"starting_at": {from.Format(time.RFC3339)}, "bucket_width": {"1d"}, "limit": {"31"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/v1/organizations/usage_report/messages?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	return req, nil
}

// ParseAnthropicUsage sums input (uncached, cache reads, and cache writes)
// and output tokens over a page of buckets.
func ParseAnthropicUsage(raw []byte) (map[string]int64, string, error) {
	var page struct {
		Data []struct {
			Results []struct {
				Uncached  int64 `json:"uncached_input_tokens"`
				CacheRead int64 `json:"cache_read_input_tokens"`
				Creation  struct {
					Short int64 `json:"ephemeral_5m_input_tokens"`
					Long  int64 `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
				Output int64 `json:"output_tokens"`
			} `json:"results"`
		} `json:"data"`
		HasMore  bool   `json:"has_more"`
		NextPage string `json:"next_page"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, "", fmt.Errorf("anthropic usage: %w", err)
	}
	tokens := map[string]int64{"input": 0, "output": 0}
	for _, b := range page.Data {
		for _, r := range b.Results {
			tokens["input"] += r.Uncached + r.CacheRead + r.Creation.Short + r.Creation.Long
			tokens["output"] += r.Output
		}
	}
	if page.HasMore {
		return tokens, page.NextPage, nil
	}
	return tokens, "", nil
}

// Merge joins priced accounts with token counts by provider and account; an
// account seen only in a usage report is added with no price.
func Merge(priced []Account, usage []Account) []Account {
	byKey := map[string]*Account{}
	var order []string
	add := func(a Account) *Account {
		key := a.Provider + ":" + a.Account
		if got, ok := byKey[key]; ok {
			return got
		}
		cp := a
		cp.Tokens = nil
		byKey[key] = &cp
		order = append(order, key)
		return &cp
	}
	for _, a := range priced {
		add(a)
	}
	for _, u := range usage {
		got := add(u)
		got.Tokens = u.Tokens
	}
	out := make([]Account, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Account < out[j].Account
	})
	return out
}

// Collector is the ai spend collector.
type Collector struct {
	priced []Account
	byPlan []PlanPrice
	// plans is the subscriptions collector's latest plan per account; nil
	// when nothing prices by plan.
	plans    func() []subscriptions.Plan
	apis     []usageAPI
	interval time.Duration
	http     *http.Client
	log      *slog.Logger
	inst     *collect.Instruments
	latest   atomic.Pointer[Snapshot]
}

// FromEnv builds the collector, or nil when nothing is configured.
func FromEnv(log *slog.Logger, inst *collect.Instruments) (*Collector, error) {
	prices, planPrices, openAI, anthropic := os.Getenv("SUBSCRIPTION_PRICES"), os.Getenv("PLAN_PRICES"), os.Getenv("OPENAI_ADMIN_KEY"), os.Getenv("ANTHROPIC_ADMIN_KEY")
	if prices == "" && planPrices == "" && openAI == "" && anthropic == "" {
		return nil, nil
	}
	priced, err := ParsePrices(prices)
	if err != nil {
		return nil, err
	}
	byPlan, err := ParsePlanPrices(planPrices)
	if err != nil {
		return nil, err
	}
	c := &Collector{
		priced:   priced,
		byPlan:   byPlan,
		interval: collect.Interval(os.Getenv("AI_INTERVAL"), time.Hour),
		http:     &http.Client{Timeout: 60 * time.Second},
		log:      log, inst: inst,
	}
	if openAI != "" {
		c.apis = append(c.apis, usageAPI{provider: "openai", account: name("OPENAI_ACCOUNT", "api"), key: openAI, request: openAIRequest, parse: ParseOpenAIUsage})
	}
	if anthropic != "" {
		c.apis = append(c.apis, usageAPI{provider: "anthropic", account: name("ANTHROPIC_ACCOUNT", "api"), key: anthropic, request: anthropicRequest, parse: ParseAnthropicUsage})
	}
	if planPrices != "" {
		if !subscriptions.Enabled() {
			return nil, errors.New("PLAN_PRICES needs the subscriptions collector (USAGE_PROBE_ACCOUNTS) to know each account's plan")
		}
		c.plans = subscriptions.Plans
	}
	c.latest.Store(&Snapshot{At: time.Now().UTC(), Accounts: c.pricedNow()})
	return c, c.register()
}

func name(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return strings.ToLower(v)
	}
	return fallback
}

func (c *Collector) Name() string { return "ai" }

// Latest is the last round's accounts; the prices are there before the first.
func (c *Collector) Latest() *Snapshot {
	snap := c.latest.Load()
	if snap == nil || c.plans == nil {
		return snap
	}
	// Prices by plan follow the subscriptions collector as it learns
	// accounts, without waiting for this collector's next round.
	var usage []Account
	for _, a := range snap.Accounts {
		if a.Tokens != nil {
			usage = append(usage, a)
		}
	}
	return &Snapshot{At: snap.At, Accounts: Merge(c.pricedNow(), usage)}
}

// pricedNow is every priced account right now: those priced by name, and
// those the subscriptions collector has seen, priced by plan.
func (c *Collector) pricedNow() []Account {
	if c.plans == nil {
		return c.priced
	}
	return PriceByPlan(c.priced, c.plans(), c.byPlan)
}

func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.round)
}

func (c *Collector) round(ctx context.Context) error {
	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var usage []Account
	var errs []error
	for _, api := range c.apis {
		tokens, err := c.report(ctx, api, from)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", api.provider, err))
			continue
		}
		usage = append(usage, Account{Provider: api.provider, Account: api.account, Tokens: tokens})
	}
	snap := &Snapshot{At: now, Accounts: Merge(c.pricedNow(), usage)}
	c.latest.Store(snap)
	c.log.InfoContext(ctx, "ai spend read", "accounts", len(snap.Accounts), "reports", len(usage))
	return errors.Join(errs...)
}

// report reads every page of one provider's month-to-date usage.
func (c *Collector) report(ctx context.Context, api usageAPI, from time.Time) (map[string]int64, error) {
	total := map[string]int64{}
	next := ""
	for page := 0; page < 12; page++ {
		req, err := api.request(ctx, api.key, from)
		if err != nil {
			return nil, err
		}
		if next != "" {
			q := req.URL.Query()
			q.Set("page", next)
			req.URL.RawQuery = q.Encode()
		}
		res, err := c.http.Do(req)
		if err != nil {
			c.inst.Upstream(ctx, api.provider+"-usage", collect.Failed)
			return nil, err
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		switch {
		case res.StatusCode == http.StatusTooManyRequests:
			c.inst.Upstream(ctx, api.provider+"-usage", collect.Refused)
			return nil, fmt.Errorf("usage report refused: %s", res.Status)
		case res.StatusCode != http.StatusOK:
			c.inst.Upstream(ctx, api.provider+"-usage", collect.Failed)
			return nil, fmt.Errorf("usage report: %s", res.Status)
		}
		c.inst.Upstream(ctx, api.provider+"-usage", collect.OK)
		tokens, more, err := api.parse(raw)
		if err != nil {
			return nil, err
		}
		for k, v := range tokens {
			total[k] += v
		}
		if more == "" {
			return total, nil
		}
		next = more
	}
	return total, nil
}

func (c *Collector) register() error {
	m := collect.Meter()
	cost, err := m.Float64ObservableGauge("tally.ai.cost_per_month", metric.WithDescription("What an AI subscription or API account costs a month, from SUBSCRIPTION_PRICES or PLAN_PRICES."))
	if err != nil {
		return err
	}
	tokens, err := m.Int64ObservableGauge("tally.ai.tokens", metric.WithUnit("{token}"), metric.WithDescription("Tokens an AI account has used this month, by direction, where its usage API reports them."))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		snap := c.latest.Load()
		if snap == nil {
			return nil
		}
		for _, a := range snap.Accounts {
			who := []attribute.KeyValue{attribute.String("provider", a.Provider), attribute.String("account", a.Account)}
			if a.CostPerMonth > 0 || a.Tokens == nil {
				o.ObserveFloat64(cost, a.CostPerMonth, metric.WithAttributes(who...))
			}
			for dir, n := range a.Tokens {
				o.ObserveInt64(tokens, n, metric.WithAttributes(append(who, attribute.String("direction", dir))...))
			}
		}
		return nil
	}, cost, tokens)
	return err
}
