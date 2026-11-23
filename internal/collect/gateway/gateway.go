// Package gateway reads what an AI gateway measured: the tokens and
// requests every caller's key used, by day, product tier, and model, from
// the Log Analytics workspace the gateway's token metrics land in. It is
// the measured lens: the gateway sees every call made through it, so its
// tokens are the truth for how much was used, as the bill is the truth for
// what was paid and a shipper's conversations are the detail of what for.
//
// It turns on with GATEWAY_WORKSPACE, the workspace's id, and reads it as
// the pod's Azure identity (any role that may query the workspace, such as
// Reader). It reads the current month and the GATEWAY_MONTHS (default 6)
// before it, whole, every GATEWAY_INTERVAL (default an hour).
package gateway

import (
	"bytes"
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

	"github.com/orchard-suite/tally/internal/agents"
	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/azure"
)

// LogAnalytics is the query API's root and the audience its tokens are for.
const LogAnalytics = "https://api.loganalytics.io"

// Row is what one key used of one model on one day.
type Row struct {
	// Day is the day in the board's zone, such as 2026-11-04.
	Day string `json:"day"`
	// Key is the gateway subscription the caller used: a person's key, such
	// as dev--name--tier, or an app's.
	Key     string `json:"key"`
	Product string `json:"product"`
	// Model is the bare model name, or empty on days before the gateway
	// tagged its token metrics with the model.
	Model string `json:"model"`
	agents.Tokens
}

// Snapshot is one round's rows, oldest day first.
type Snapshot struct {
	At time.Time `json:"at"`
	// From is the first day read.
	From string `json:"from"`
	Rows []Row  `json:"rows"`
}

// Months is how many whole months before the current one a round reads by
// default: as many as the board shows billed.
const Months = 6

// Query is the round's KQL: every token and request metric the gateway
// emitted from from on, added up by day in zone, key, product, and model.
func Query(from time.Time, zone string) string {
	return fmt.Sprintf(`AppMetrics
| where TimeGenerated >= datetime(%s)
| where Name in ("Prompt Tokens", "Prompt Cached Tokens", "Cache Creation Tokens", "Completion Tokens", "ai-request")
| extend key = tostring(Properties["Subscription ID"]), product = tostring(Properties["Product ID"]), model = tostring(coalesce(Properties["Model"], Properties["model"]))
| summarize prompt = sumif(Sum, Name == "Prompt Tokens"), cached = sumif(Sum, Name == "Prompt Cached Tokens"), created = sumif(Sum, Name == "Cache Creation Tokens"), completion = sumif(Sum, Name == "Completion Tokens"), requests = sumif(Sum, Name == "ai-request")
    by day = format_datetime(startofday(datetime_utc_to_local(TimeGenerated, "%s")), "yyyy-MM-dd"), key, product, model
| where isnotempty(key)`, from.UTC().Format(time.RFC3339), zone)
}

// Result is the query API's answer.
type Result struct {
	Tables []struct {
		Columns []struct {
			Name string `json:"name"`
		} `json:"columns"`
		Rows [][]any `json:"rows"`
	} `json:"tables"`
}

// Shape turns the query's answer into rows, one per day, key, product, and
// model, the tokens split the way agent work is:
//
//   - Claude models count the gateway's prompt tokens without the cache, as
//     the Anthropic API does, so input is the prompt, cache reads the
//     cached prompt, and cache writes the cache creation.
//   - Every other model counts its cached prompt inside the prompt, as the
//     OpenAI API does, so input is the prompt less the cached part.
//
// The gateway reports a cache write without its lifetime; it is counted as
// a five-minute write, the default and the cheaper rate.
func Shape(res Result) ([]Row, error) {
	if len(res.Tables) == 0 {
		return nil, errors.New("no table in the answer")
	}
	t := res.Tables[0]
	col := map[string]int{}
	for i, c := range t.Columns {
		col[c.Name] = i
	}
	for _, name := range []string{"day", "key", "product", "model", "prompt", "cached", "created", "completion", "requests"} {
		if _, ok := col[name]; !ok {
			return nil, fmt.Errorf("the answer has no %s column", name)
		}
	}
	merged := map[[4]string]*Row{}
	for _, r := range t.Rows {
		str := func(name string) string { s, _ := r[col[name]].(string); return s }
		num := func(name string) (int64, error) { return count(r[col[name]]) }
		row := Row{Day: str("day"), Key: str("key"), Product: str("product"), Model: agents.Model(str("model"))}
		if row.Day == "" || row.Key == "" {
			continue
		}
		var prompt, cached, created, completion, requests int64
		for name, dst := range map[string]*int64{"prompt": &prompt, "cached": &cached, "created": &created, "completion": &completion, "requests": &requests} {
			v, err := num(name)
			if err != nil {
				return nil, fmt.Errorf("%s on %s for %s: %w", name, row.Day, row.Key, err)
			}
			*dst = v
		}
		// A request with no tokens is not usage: a model listing, a refused
		// call, or, before the gateway tagged tokens with the model, the
		// request count that carried the model the tokens lacked.
		if prompt+cached+created+completion == 0 {
			continue
		}
		row.Tokens = split(row.Model, prompt, cached, created, completion, requests)
		// Two raw model names can be one bare model.
		k := [4]string{row.Day, row.Key, row.Product, row.Model}
		if m := merged[k]; m != nil {
			m.Tokens.Add(row.Tokens)
			continue
		}
		merged[k] = &row
	}
	out := make([]Row, 0, len(merged))
	for _, r := range merged {
		out = append(out, *r)
	}
	sort.Slice(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.Day != y.Day {
			return x.Day < y.Day
		}
		if x.Key != y.Key {
			return x.Key < y.Key
		}
		if x.Product != y.Product {
			return x.Product < y.Product
		}
		return x.Model < y.Model
	})
	return out, nil
}

func split(model string, prompt, cached, created, completion, requests int64) agents.Tokens {
	t := agents.Tokens{Requests: requests, CacheRead: cached, CacheWrite5m: created, Output: completion}
	if strings.HasPrefix(model, "claude") {
		t.Input = prompt
	} else {
		t.Input = max(prompt-cached, 0)
	}
	return t
}

// count reads a summed metric, which the API gives as a number or, for a
// big one, a string.
func count(v any) (int64, error) {
	switch n := v.(type) {
	case nil:
		return 0, nil
	case float64:
		return int64(n + 0.5), nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return int64(f + 0.5), err
	}
	return 0, fmt.Errorf("%T is not a number", v)
}

// Collector reads the workspace every interval.
type Collector struct {
	workspace string
	months    int
	zone      *time.Location
	token     func(context.Context) (string, error)
	client    *http.Client
	log       *slog.Logger
	inst      *collect.Instruments
	interval  time.Duration
	latest    atomic.Pointer[Snapshot]
}

// FromEnv turns the collector on with GATEWAY_WORKSPACE. It is off without
// it, and refuses to start without the Azure identity to read it as.
func FromEnv(log *slog.Logger, inst *collect.Instruments, zone *time.Location) (*Collector, error) {
	ws := strings.TrimSpace(os.Getenv("GATEWAY_WORKSPACE"))
	if ws == "" {
		log.Info("gateway collector off: GATEWAY_WORKSPACE is not set")
		return nil, nil
	}
	id, err := azure.FromIdentityEnv()
	if err != nil {
		return nil, fmt.Errorf("GATEWAY_WORKSPACE needs the Azure identity: %w", err)
	}
	months := Months
	if v := os.Getenv("GATEWAY_MONTHS"); v != "" {
		if months, err = strconv.Atoi(v); err != nil || months < 0 {
			return nil, fmt.Errorf("GATEWAY_MONTHS %q is not a count of months", v)
		}
	}
	if zone == nil {
		zone = time.UTC
	}
	c := &Collector{workspace: ws, months: months, zone: zone, token: azure.Tokens(id, LogAnalytics),
		client: &http.Client{Timeout: 2 * time.Minute}, log: log, inst: inst,
		interval: collect.Interval(os.Getenv("GATEWAY_INTERVAL"), time.Hour)}
	return c, c.register()
}

// Name is the collector's name.
func (c *Collector) Name() string { return "gateway" }

// Latest is the last round's rows, nil before the first round worked.
func (c *Collector) Latest() *Snapshot { return c.latest.Load() }

// Run reads every interval until ctx ends.
func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.round)
}

// From is the first instant a round at now reads: the first of the month
// months before now's, in zone.
func From(now time.Time, months int, zone *time.Location) time.Time {
	now = now.In(zone)
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, zone).AddDate(0, -months, 0)
}

func (c *Collector) round(ctx context.Context) error {
	now := time.Now()
	from := From(now, c.months, c.zone)
	res, err := c.query(ctx, Query(from, c.zone.String()))
	if err != nil {
		return err
	}
	rows, err := Shape(res)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errors.New("the workspace holds no gateway token metric in the window")
	}
	c.latest.Store(&Snapshot{At: now.UTC(), From: from.Format(time.DateOnly), Rows: rows})
	c.log.InfoContext(ctx, "gateway read", "rows", len(rows), "from", from.Format(time.DateOnly))
	return nil
}

func (c *Collector) query(ctx context.Context, kql string) (Result, error) {
	var res Result
	token, err := c.token(ctx)
	if err != nil {
		return res, fmt.Errorf("sign in: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"query": kql})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, LogAnalytics+"/v1/workspaces/"+c.workspace+"/query", bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		c.inst.Upstream(ctx, "loganalytics", collect.Failed)
		return res, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		c.inst.Upstream(ctx, "loganalytics", collect.Refused)
		return res, errors.New("log analytics refused the query: too many requests")
	}
	if resp.StatusCode/100 != 2 {
		c.inst.Upstream(ctx, "loganalytics", collect.Failed)
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return res, fmt.Errorf("log analytics answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		c.inst.Upstream(ctx, "loganalytics", collect.Failed)
		return res, fmt.Errorf("log analytics answer: %w", err)
	}
	c.inst.Upstream(ctx, "loganalytics", collect.OK)
	return res, nil
}

// register exports this month's tokens by model and kind. A key never
// becomes a label: keys are people.
func (c *Collector) register() error {
	m := collect.Meter()
	tokens, err := m.Int64ObservableGauge("tally.gateway.tokens", metric.WithDescription("Tokens the AI gateway measured this month so far, by model and kind (input, cache_read, cache_write, output)."))
	if err != nil {
		return err
	}
	requests, err := m.Int64ObservableGauge("tally.gateway.requests", metric.WithDescription("Requests the AI gateway counted this month so far, by model."))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		snap := c.Latest()
		if snap == nil {
			return nil
		}
		month := time.Now().In(c.zone).Format("2006-01")
		byModel := map[string]agents.Tokens{}
		for _, r := range snap.Rows {
			if strings.HasPrefix(r.Day, month) {
				t := byModel[r.Model]
				t.Add(r.Tokens)
				byModel[r.Model] = t
			}
		}
		for model, t := range byModel {
			at := attribute.String("model", model)
			for kind, v := range map[string]int64{"input": t.Input, "cache_read": t.CacheRead, "cache_write": t.CacheWrite5m + t.CacheWrite1h, "output": t.Output} {
				o.ObserveInt64(tokens, v, metric.WithAttributes(at, attribute.String("kind", kind)))
			}
			o.ObserveInt64(requests, t.Requests, metric.WithAttributes(at))
		}
		return nil
	}, tokens, requests)
	return err
}
