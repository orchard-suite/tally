// Package advisor reads Azure Advisor's open right-sizing advice for
// virtual machines: each VM Advisor finds underused, the size it is on, the
// size or shutdown it advises, its CPU and memory at the 95th percentile,
// and what the change saves a year. Advisor decides and a person dismisses
// in Advisor; tally only reads, through Azure Resource Graph, every
// subscription the identity can see, and names what kind of change each
// piece of advice is.
package advisor

import (
	"bytes"
	"context"
	"encoding/json"
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
)

// RightSize is Advisor's recommendation type "Right-size or shutdown
// underutilized virtual machines".
const RightSize = "e10b1381-5f0a-47ff-8c7b-37bd13d7c974"

// The kinds of advice, as Advisor names them.
const (
	Resize   = "SkuChange"
	Shutdown = "Shutdown"
)

// The kinds of change, as tally names them. Shrinking stays in the VM's
// family; a step down to burstable or into another family changes how the
// VM behaves, so a person decides it.
const (
	Shrink    = "shrink"
	Burstable = "burstable"
	Family    = "family"
	Off       = "shutdown"
)

// Changes are the kinds of change, in the order the board shows them.
var Changes = []string{Shrink, Burstable, Family, Off}

// DefaultCounted is what the savings count when ADVISOR_COUNT is unset.
const DefaultCounted = Shrink

// SizeFamily is a VM size's family: the letters that lead its size, such as
// D for Standard_D8as_v5, E for Standard_E4-2ds_v4, B for Standard_B2s_v2,
// and NC for Standard_NC6s_v3.
func SizeFamily(sku string) string {
	size := sku
	if i := strings.IndexByte(size, '_'); i >= 0 {
		size = size[i+1:]
	}
	end := 0
	for end < len(size) && size[end] >= 'A' && size[end] <= 'Z' {
		end++
	}
	return size[:end]
}

// Change names the kind of change advice is.
func Change(kind, from, to string) string {
	if kind == Shutdown {
		return Off
	}
	f, t := SizeFamily(from), SizeFamily(to)
	switch {
	case strings.HasPrefix(t, "B") && !strings.HasPrefix(f, "B"):
		return Burstable
	case f != t:
		return Family
	}
	return Shrink
}

// Counted parses ADVISOR_COUNT: the kinds of change whose savings count.
func Counted(v string) (map[string]bool, error) {
	if v == "" {
		v = DefaultCounted
	}
	out := map[string]bool{}
	for _, c := range strings.Split(v, ",") {
		c = strings.TrimSpace(c)
		known := false
		for _, k := range Changes {
			known = known || c == k
		}
		if !known {
			return nil, fmt.Errorf("ADVISOR_COUNT names %q; the kinds are %s", c, strings.Join(Changes, ", "))
		}
		out[c] = true
	}
	return out, nil
}

// Advice is one VM's right-sizing advice.
type Advice struct {
	VM            string `json:"vm"`
	ResourceGroup string `json:"resource_group"`
	Subscription  string `json:"subscription"`
	// Kind is SkuChange or Shutdown.
	Kind string `json:"kind"`
	// Change is shrink, burstable, family, or shutdown.
	Change string `json:"change"`
	// Counted says the change is a kind ADVISOR_COUNT counts; the rest
	// wait on a person's decision.
	Counted bool   `json:"counted"`
	From    string `json:"from"`
	// To is the advised size; empty for a shutdown.
	To string `json:"to,omitempty"`
	// CPUP95 and MemoryP95 are the VM's use at the 95th percentile, in
	// percent of its size, over the days Advisor looked at.
	CPUP95         float64 `json:"cpu_p95"`
	MemoryP95      float64 `json:"memory_p95"`
	SavingsPerYear float64 `json:"savings_per_year"`
	Currency       string  `json:"currency"`
}

// Snapshot is one round's advice, the largest saving first.
type Snapshot struct {
	At     time.Time `json:"at"`
	Advice []Advice  `json:"advice"`
}

// Query is the Resource Graph query for the advice, open or not.
const Query = `advisorresources
| where type == "microsoft.advisor/recommendations"
| where properties.recommendationTypeId == "` + RightSize + `"
| project id, vm = tostring(properties.impactedValue), resourceGroup, subscriptionId,
  status = tostring(properties.recommendationStatus), platformState = tostring(properties.platformState),
  customerState = tostring(properties.customerState), x = properties.extendedProperties`

// Suppressions is the Resource Graph query for advice a person dismissed or
// postponed; each id is its recommendation's id and a suffix.
const Suppressions = `advisorresources
| where type == "microsoft.advisor/suppressions"
| project id`

// page is one Resource Graph answer.
type page struct {
	Data []struct {
		ID            string            `json:"id"`
		VM            string            `json:"vm"`
		ResourceGroup string            `json:"resourceGroup"`
		Subscription  string            `json:"subscriptionId"`
		Status        string            `json:"status"`
		PlatformState string            `json:"platformState"`
		CustomerState string            `json:"customerState"`
		X             map[string]string `json:"x"`
	} `json:"data"`
	SkipToken string `json:"$skipToken"`
}

// Open says advice still stands: Advisor has not closed it (it closes advice
// once the VM no longer qualifies) and no person has dismissed, postponed,
// or suppressed it.
func open(status, platform, customer string, suppressed bool) bool {
	return status != "Completed" && platform != "Resolved" && (customer == "" || customer == "New") && !suppressed
}

// Shape reads the open advice from Resource Graph pages of advice and of
// suppressions, and names each change; counted are the kinds whose savings
// count. Advisor writes every extended property as text.
func Shape(pages, suppressions [][]byte, counted map[string]bool, at time.Time) (Snapshot, error) {
	suppressed := map[string]bool{}
	for _, raw := range suppressions {
		var p struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return Snapshot{}, fmt.Errorf("resource graph suppressions: %w", err)
		}
		for _, d := range p.Data {
			if i := strings.Index(strings.ToLower(d.ID), "/suppressions/"); i > 0 {
				suppressed[strings.ToLower(d.ID[:i])] = true
			}
		}
	}
	snap := Snapshot{At: at, Advice: []Advice{}}
	for _, raw := range pages {
		var p page
		if err := json.Unmarshal(raw, &p); err != nil {
			return Snapshot{}, fmt.Errorf("resource graph page: %w", err)
		}
		for _, d := range p.Data {
			if !open(d.Status, d.PlatformState, d.CustomerState, suppressed[strings.ToLower(d.ID)]) {
				continue
			}
			num := func(k string) float64 { f, _ := strconv.ParseFloat(d.X[k], 64); return f }
			a := Advice{VM: d.VM, ResourceGroup: d.ResourceGroup, Subscription: d.Subscription, Kind: d.X["recommendationType"],
				From: d.X["currentSku"], To: d.X["targetSku"], CPUP95: num("MaxCpuP95"), MemoryP95: num("MaxMemoryP95"),
				SavingsPerYear: num("annualSavingsAmount"), Currency: d.X["savingsCurrency"]}
			if a.Kind == Shutdown {
				a.To = ""
			}
			a.Change = Change(a.Kind, a.From, a.To)
			a.Counted = counted[a.Change]
			if a.VM == "" {
				return Snapshot{}, fmt.Errorf("advice with no VM in %s", d.ResourceGroup)
			}
			snap.Advice = append(snap.Advice, a)
		}
	}
	sort.Slice(snap.Advice, func(i, j int) bool {
		if snap.Advice[i].SavingsPerYear != snap.Advice[j].SavingsPerYear {
			return snap.Advice[i].SavingsPerYear > snap.Advice[j].SavingsPerYear
		}
		return snap.Advice[i].VM < snap.Advice[j].VM
	})
	return snap, nil
}

// Collector reads the advice every interval.
type Collector struct {
	identity azure.Identity
	counted  map[string]bool
	log      *slog.Logger
	inst     *collect.Instruments
	http     *http.Client
	interval time.Duration
	latest   atomic.Pointer[Snapshot]
}

// FromEnv turns the collector on with ADVISOR=on and the Azure identity's
// environment (AZURE_CLIENT_ID, AZURE_TENANT_ID, AZURE_FEDERATED_TOKEN_FILE).
// The identity needs Reader where its VMs are. ADVISOR_COUNT names the kinds
// of change whose savings count (shrink when unset).
func FromEnv(log *slog.Logger, inst *collect.Instruments) (*Collector, error) {
	if os.Getenv("ADVISOR") != "on" {
		log.Info("advisor collector off: ADVISOR=on is not set")
		return nil, nil
	}
	id, err := azure.FromIdentityEnv()
	if err != nil {
		return nil, fmt.Errorf("advisor: %w", err)
	}
	counted, err := Counted(os.Getenv("ADVISOR_COUNT"))
	if err != nil {
		return nil, err
	}
	c := &Collector{identity: id, counted: counted, log: log, inst: inst, http: &http.Client{Timeout: time.Minute},
		interval: collect.Interval(os.Getenv("ADVISOR_INTERVAL"), 6*time.Hour)}
	return c, c.register()
}

// Name is the collector's name.
func (c *Collector) Name() string { return "advisor" }

// Interval is how often a round runs when the last one worked.
func (c *Collector) Interval() time.Duration { return c.interval }

// Latest is the last round's advice, nil before the first round worked.
func (c *Collector) Latest() *Snapshot { return c.latest.Load() }

// Run reads every interval until ctx ends.
func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.round)
}

const graphURL = azure.ARM + "/providers/Microsoft.ResourceGraph/resources?api-version=2022-10-01"

func (c *Collector) round(ctx context.Context) error {
	token, err := azure.SignIn(ctx, c.identity)
	if err != nil {
		return err
	}
	pages, err := c.all(ctx, token, Query)
	if err != nil {
		return err
	}
	suppressions, err := c.all(ctx, token, Suppressions)
	if err != nil {
		return err
	}
	snap, err := Shape(pages, suppressions, c.counted, time.Now().UTC())
	if err != nil {
		return err
	}
	c.latest.Store(&snap)
	return nil
}

// all reads every page of a query.
func (c *Collector) all(ctx context.Context, token, query string) ([][]byte, error) {
	var pages [][]byte
	skip := ""
	for {
		opts := map[string]any{"$top": 1000}
		if skip != "" {
			opts["$skipToken"] = skip
		}
		raw, err := c.post(ctx, token, map[string]any{"query": query, "options": opts})
		if err != nil {
			return nil, err
		}
		pages = append(pages, raw)
		var p struct {
			SkipToken string `json:"$skipToken"`
		}
		_ = json.Unmarshal(raw, &p)
		if skip = p.SkipToken; skip == "" {
			return pages, nil
		}
	}
}

func (c *Collector) post(ctx context.Context, token string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphURL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		c.inst.Upstream(ctx, "resourcegraph", collect.Failed)
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		c.inst.Upstream(ctx, "resourcegraph", collect.Failed)
		return nil, err
	}
	switch {
	case res.StatusCode == http.StatusTooManyRequests:
		c.inst.Upstream(ctx, "resourcegraph", collect.Refused)
		return nil, fmt.Errorf("resource graph refused: %s", res.Status)
	case res.StatusCode != http.StatusOK:
		c.inst.Upstream(ctx, "resourcegraph", collect.Failed)
		return nil, fmt.Errorf("resource graph: %s: %s", res.Status, bytes.TrimSpace(raw[:min(len(raw), 300)]))
	}
	c.inst.Upstream(ctx, "resourcegraph", collect.OK)
	return raw, nil
}

func (c *Collector) register() error {
	m := collect.Meter()
	n, err := m.Int64ObservableGauge("tally.advisor.recommendations", metric.WithDescription("Virtual machines Azure Advisor advises resizing or shutting down, by change (shrink, burstable, family, shutdown) and counted (whether ADVISOR_COUNT counts its savings)."))
	if err != nil {
		return err
	}
	save, err := m.Float64ObservableGauge("tally.advisor.savings", metric.WithDescription("What Azure Advisor's open right-sizing advice saves a year, in dollars, by change and counted."))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		snap := c.Latest()
		if snap == nil {
			return nil
		}
		count, total := map[string]int64{}, map[string]float64{}
		for _, a := range snap.Advice {
			count[a.Change]++
			total[a.Change] += a.SavingsPerYear
		}
		for _, ch := range Changes {
			attrs := metric.WithAttributes(attribute.String("change", ch), attribute.Bool("counted", c.counted[ch]))
			o.ObserveInt64(n, count[ch], attrs)
			o.ObserveFloat64(save, total[ch], attrs)
		}
		return nil
	}, n, save)
	return err
}
