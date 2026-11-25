// Package azure reads each subscription's cost by resource group and service
// for the last 30 days, the current month, and the previous month from Cost
// Management, signed in as the
// namespace's Azure identity (no client secret). It turns on with
// AZURE_COST=on, signs in with the identity's environment (AZURE_CLIENT_ID
// and friends, from the azure-identity ConfigMap), and reads every
// subscription the identity can see, or only those AZURE_SUBSCRIPTIONS names.
package azure

import (
	"bytes"
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
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/promapi"
)

// ARM is the Resource Manager root.
const ARM = "https://management.azure.com"

// Periods. Trailing is the last 30 days to now, the run rate; Current is
// this calendar month so far; Previous is last calendar month, whole.
const (
	Trailing = "trailing"
	Current  = "current"
	Previous = "previous"
)

// Cost is one resource group and service's cost in one period.
type Cost struct {
	Provider string  `json:"provider"`
	Account  string  `json:"account"`
	Group    string  `json:"group"`
	Service  string  `json:"service"`
	Currency string  `json:"currency"`
	Period   string  `json:"period"`
	Cost     float64 `json:"cost"`
}

// Snapshot is one round's costs.
type Snapshot struct {
	At    time.Time `json:"at"`
	Costs []Cost    `json:"costs"`
}

// Identity is what the azure-identity ConfigMap gives a pod.
type Identity struct {
	TenantID, ClientID, TokenFile, AuthorityHost string
}

// FromIdentityEnv reads the identity from the environment the ConfigMap sets.
func FromIdentityEnv() (Identity, error) {
	id := Identity{
		TenantID:      os.Getenv("AZURE_TENANT_ID"),
		ClientID:      os.Getenv("AZURE_CLIENT_ID"),
		TokenFile:     os.Getenv("AZURE_FEDERATED_TOKEN_FILE"),
		AuthorityHost: os.Getenv("AZURE_AUTHORITY_HOST"),
	}
	if id.TenantID == "" || id.ClientID == "" || id.TokenFile == "" {
		return id, errors.New("AZURE_TENANT_ID, AZURE_CLIENT_ID, and AZURE_FEDERATED_TOKEN_FILE must be set")
	}
	if id.AuthorityHost == "" {
		id.AuthorityHost = "https://login.microsoftonline.com/"
	}
	return id, nil
}

// Monitor is the audience of an Azure Monitor workspace's Prometheus.
const Monitor = "https://prometheus.monitor.azure.com"

// SignIn trades the service account token for a Resource Manager token.
func SignIn(ctx context.Context, id Identity) (string, error) {
	t, _, err := signIn(ctx, id, ARM)
	return t, err
}

// Tokens gives tokens for an audience, signing in again a few minutes
// before the last one expires.
func Tokens(id Identity, audience string) func(context.Context) (string, error) {
	var (
		mu    sync.Mutex
		token string
		renew time.Time
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if token != "" && time.Now().Before(renew) {
			return token, nil
		}
		t, life, err := signIn(ctx, id, audience)
		if err != nil {
			return "", err
		}
		token, renew = t, time.Now().Add(life-5*time.Minute)
		return token, nil
	}
}

// Prometheus is a client for the Prometheus at base. An Azure Monitor
// workspace (its host ends in .prometheus.monitor.azure.com) is read as the
// pod's Azure identity, which needs Monitoring Data Reader on it; any other
// Prometheus is read as is.
func Prometheus(base string) (*promapi.Client, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("PROMETHEUS_URL: %w", err)
	}
	if !strings.HasSuffix(u.Hostname(), ".prometheus.monitor.azure.com") {
		return promapi.New(base), nil
	}
	id, err := FromIdentityEnv()
	if err != nil {
		return nil, fmt.Errorf("an Azure Monitor workspace needs the Azure identity: %w", err)
	}
	return promapi.New(base, promapi.WithToken(Tokens(id, Monitor))), nil
}

// signIn trades the service account token for a token for audience, and
// says how long it lives. The kubelet rotates the file, so it is read on
// every sign-in.
func signIn(ctx context.Context, id Identity, audience string) (string, time.Duration, error) {
	assertion, err := os.ReadFile(id.TokenFile)
	if err != nil {
		return "", 0, fmt.Errorf("service account token: %w", err)
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {id.ClientID},
		"scope":                 {audience + "/.default"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {strings.TrimSpace(string(assertion))},
	}
	endpoint := strings.TrimSuffix(id.AuthorityHost, "/") + "/" + id.TenantID + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer res.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error_description"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("token: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("token: %s", out.Error)
	}
	return out.AccessToken, time.Duration(out.ExpiresIn) * time.Second, nil
}

// Month is the first and last instant of a month.
type Month struct{ From, To time.Time }

// Months gives the current and previous month at now.
func Months(now time.Time) (current, previous Month) {
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	current = Month{From: start, To: start.AddDate(0, 1, 0).Add(-time.Second)}
	prevStart := start.AddDate(0, -1, 0)
	previous = Month{From: prevStart, To: start.Add(-time.Second)}
	return current, previous
}

// TrailingDays gives the last n whole days up to and including today, in
// UTC: a month's worth of spend whatever day of the month it is.
func TrailingDays(now time.Time, n int) Month {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return Month{From: today.AddDate(0, 0, -(n - 1)), To: today.AddDate(0, 0, 1).Add(-time.Second)}
}

// Query is the Cost Management request for one month, grouped by resource
// group and service.
func Query(m Month) []byte {
	body := map[string]any{
		"type":      "ActualCost",
		"timeframe": "Custom",
		"timePeriod": map[string]string{
			"from": m.From.Format(time.RFC3339),
			"to":   m.To.Format(time.RFC3339),
		},
		"dataset": map[string]any{
			"granularity": "None",
			"aggregation": map[string]any{"totalCost": map[string]string{"name": "Cost", "function": "Sum"}},
			"grouping": []map[string]string{
				{"type": "Dimension", "name": "ResourceGroupName"},
				{"type": "Dimension", "name": "ServiceName"},
			},
		},
	}
	raw, _ := json.Marshal(body)
	return raw
}

// Shape reads a Cost Management answer into costs for one period. The
// answer names its columns; rows are read by those names.
func Shape(raw []byte, account, period string) ([]Cost, error) {
	var res struct {
		Properties struct {
			Columns []struct {
				Name string `json:"name"`
			} `json:"columns"`
			Rows [][]any `json:"rows"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("cost management answer: %w", err)
	}
	col := map[string]int{}
	for i, c := range res.Properties.Columns {
		col[strings.ToLower(c.Name)] = i
	}
	for _, want := range []string{"cost", "resourcegroupname", "servicename", "currency"} {
		if _, ok := col[want]; !ok {
			return nil, fmt.Errorf("cost management answer has no %s column", want)
		}
	}
	out := make([]Cost, 0, len(res.Properties.Rows))
	for _, row := range res.Properties.Rows {
		if len(row) < len(col) {
			continue
		}
		cost, _ := row[col["cost"]].(float64)
		str := func(name string) string {
			s, _ := row[col[name]].(string)
			return s
		}
		group := str("resourcegroupname")
		if group == "" {
			group = "(none)"
		}
		out = append(out, Cost{Provider: "azure", Account: account, Group: strings.ToLower(group), Service: str("servicename"), Currency: str("currency"), Period: period, Cost: cost})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cost > out[j].Cost })
	return out, nil
}

// Collector is the azure collector.
type Collector struct {
	// only names the subscriptions to read; empty reads every one the
	// identity can see.
	only     []string
	identity Identity
	interval time.Duration
	http     *http.Client
	log      *slog.Logger
	inst     *collect.Instruments
	latest   atomic.Pointer[Snapshot]
}

// FromEnv builds the collector with AZURE_COST=on and the identity's
// environment, or nil without AZURE_COST=on.
func FromEnv(log *slog.Logger, inst *collect.Instruments) (*Collector, error) {
	if os.Getenv("AZURE_COST") != "on" {
		log.Info("azure collector off: AZURE_COST=on is not set")
		return nil, nil
	}
	id, err := FromIdentityEnv()
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	c := &Collector{
		identity: id,
		interval: collect.Interval(os.Getenv("AZURE_INTERVAL"), 6*time.Hour),
		http:     &http.Client{Timeout: 120 * time.Second},
		log:      log, inst: inst,
	}
	for _, s := range strings.Split(os.Getenv("AZURE_SUBSCRIPTIONS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.only = append(c.only, s)
		}
	}
	return c, c.register()
}

// subscriptions lists the subscriptions to read: those named, or every one
// the identity can see.
func (c *Collector) subscriptions(ctx context.Context, token string) ([]string, error) {
	if len(c.only) > 0 {
		return c.only, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ARM+"/subscriptions?api-version=2022-12-01", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := c.http.Do(req)
	if err != nil {
		c.inst.Upstream(ctx, "azure-arm", collect.Failed)
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		c.inst.Upstream(ctx, "azure-arm", collect.Failed)
		return nil, fmt.Errorf("list subscriptions: %s: %s", res.Status, firstLine(raw))
	}
	c.inst.Upstream(ctx, "azure-arm", collect.OK)
	return ParseSubscriptions(raw)
}

// ParseSubscriptions reads the ids from a subscription listing.
func ParseSubscriptions(raw []byte) ([]string, error) {
	var page struct {
		Value []struct {
			ID string `json:"subscriptionId"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("subscriptions: %w", err)
	}
	out := make([]string, 0, len(page.Value))
	for _, s := range page.Value {
		if s.ID != "" {
			out = append(out, s.ID)
		}
	}
	return out, nil
}

func (c *Collector) Name() string { return "azure" }

// Interval is how often a round runs when the last one worked.
func (c *Collector) Interval() time.Duration { return c.interval }

// Latest is the last round's costs, or nil before the first.
func (c *Collector) Latest() *Snapshot { return c.latest.Load() }

func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.round)
}

func (c *Collector) round(ctx context.Context) error {
	token, err := SignIn(ctx, c.identity)
	if err != nil {
		c.inst.Upstream(ctx, "azure-signin", collect.Failed)
		return err
	}
	c.inst.Upstream(ctx, "azure-signin", collect.OK)
	subs, err := c.subscriptions(ctx, token)
	if err != nil {
		return err
	}
	current, previous := Months(time.Now())
	trailing := TrailingDays(time.Now(), 30)
	var costs []Cost
	for _, sub := range subs {
		for _, p := range []struct {
			name  string
			month Month
		}{{Trailing, trailing}, {Current, current}, {Previous, previous}} {
			raw, err := c.query(ctx, token, sub, p.month)
			if err != nil {
				return fmt.Errorf("%s, %s month: %w", sub, p.name, err)
			}
			rows, err := Shape(raw, sub, p.name)
			if err != nil {
				return err
			}
			costs = append(costs, rows...)
		}
	}
	snap := &Snapshot{At: time.Now().UTC(), Costs: costs}
	c.latest.Store(snap)
	c.log.InfoContext(ctx, "azure costs read", "subscriptions", len(subs), "rows", len(costs))
	return nil
}

// query asks Cost Management, waiting out its rate limit. The budget is
// tight (a few calls a minute per subscription) and a pod that restarts
// twice spends it, so a 429 is counted as refused and tried again after
// the pause Azure asks for, up to three times, before the round fails.
func (c *Collector) query(ctx context.Context, token, subscription string, m Month) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		raw, wait, err := c.queryOnce(ctx, token, subscription, m)
		if wait == 0 || attempt == 2 {
			return raw, err
		}
		c.log.WarnContext(ctx, "cost management rate limited; waiting", "wait", wait.String(), "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// RetryAfter reads a 429's Retry-After header in seconds; absent or odd,
// a minute.
func RetryAfter(h http.Header) time.Duration {
	if v, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && v > 0 && v <= 300 {
		return time.Duration(v) * time.Second
	}
	return time.Minute
}

// queryOnce asks once. A 429 comes back with how long to wait.
func (c *Collector) queryOnce(ctx context.Context, token, subscription string, m Month) ([]byte, time.Duration, error) {
	endpoint := ARM + "/subscriptions/" + subscription + "/providers/Microsoft.CostManagement/query?api-version=2023-11-01"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(Query(m)))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		c.inst.Upstream(ctx, "azure-cost-management", collect.Failed)
		return nil, 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, 0, err
	}
	switch {
	case res.StatusCode == http.StatusTooManyRequests:
		c.inst.Upstream(ctx, "azure-cost-management", collect.Refused)
		return nil, RetryAfter(res.Header), fmt.Errorf("cost management refused: %s", res.Status)
	case res.StatusCode != http.StatusOK:
		c.inst.Upstream(ctx, "azure-cost-management", collect.Failed)
		return nil, 0, fmt.Errorf("cost management: %s: %s", res.Status, firstLine(raw))
	}
	c.inst.Upstream(ctx, "azure-cost-management", collect.OK)
	return raw, 0, nil
}

func (c *Collector) register() error {
	m := collect.Meter()
	cost, err := m.Float64ObservableGauge("tally.cloud.cost", metric.WithDescription("Cloud cost by provider, account, resource group, service, and currency, for the last 30 days and the current and previous month."))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		snap := c.latest.Load()
		if snap == nil {
			return nil
		}
		for _, r := range snap.Costs {
			o.ObserveFloat64(cost, r.Cost, metric.WithAttributes(
				attribute.String("provider", r.Provider), attribute.String("account", r.Account),
				attribute.String("group", r.Group), attribute.String("service", r.Service),
				attribute.String("currency", r.Currency), attribute.String("period", r.Period),
			))
		}
		return nil
	}, cost)
	return err
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return s
}
