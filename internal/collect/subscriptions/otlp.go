package subscriptions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Metric names as Prometheus stores them. They carry no OTLP unit, so the
// receiver adds no suffix and these are the names a query uses.
const (
	metricUsed    = "llm_quota_used_percent"
	metricResets  = "llm_quota_resets_at_seconds"
	metricSuccess = "llm_quota_probe_success"
	metricFailure = "llm_quota_probe_failure"

	metricActive      = "llm_rotation_active"
	metricExhausted   = "llm_rotation_exhausted"
	metricRank        = "llm_rotation_rank"
	metricPinned      = "llm_rotation_pinned"
	metricFreeAt      = "llm_rotation_free_at_seconds"
	metricPreferredAt = "llm_rotation_preferred_at_seconds"
)

// rotationReport is what the metrics say about the build accounts: each
// one in order, which is active, which is picked by hand, whether all of
// them are at the bar, and where each stands.
type rotationReport struct {
	Provider  string
	Prefer    string
	Accounts  []string
	Active    string
	Pinned    string
	Exhausted bool
	Standings []standing
}

func newRotationReport(order []rotationAccount, prefer string, ch choice) *rotationReport {
	r := &rotationReport{Provider: "claude", Prefer: prefer, Active: ch.Account, Pinned: ch.Pinned, Exhausted: ch.Exhausted, Standings: ch.Standings}
	for _, a := range order {
		r.Accounts = append(r.Accounts, a.Account)
	}
	return r
}

type otlpKV struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
	} `json:"value"`
}

func kv(key, value string) otlpKV {
	a := otlpKV{Key: key}
	a.Value.StringValue = value
	return a
}

type otlpPoint struct {
	Attributes   []otlpKV `json:"attributes"`
	TimeUnixNano string   `json:"timeUnixNano"`
	AsDouble     float64  `json:"asDouble"`
}

type otlpMetric struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Gauge       struct {
		DataPoints []otlpPoint `json:"dataPoints"`
	} `json:"gauge"`
}

// buildPayload turns one run's readings into an OTLP/JSON metrics request.
// Every account sends whether its read worked; a failed one also sends why.
// rot, when there is a rotation, adds which build account is active.
func buildPayload(readings []Reading, rot *rotationReport, now time.Time) ([]byte, int) {
	ts := strconv.FormatInt(now.UnixNano(), 10)
	used := otlpMetric{Name: metricUsed, Description: "Percent of a subscription usage limit used, as the provider's CLI reports it."}
	resets := otlpMetric{Name: metricResets, Description: "When a subscription usage limit resets, in unix seconds."}
	success := otlpMetric{Name: metricSuccess, Description: "1 when the account's CLI answered the read on this run, 0 when it failed."}
	failure := otlpMetric{Name: metricFailure, Description: "1 for why the account's read failed on this run: signed_out, wrong_account, or read_failed."}
	for _, r := range readings {
		account := []otlpKV{kv("provider", r.Account.Provider), kv("account", r.Account.Email)}
		ok := 1.0
		if r.Err != nil {
			ok = 0
			failure.Gauge.DataPoints = append(failure.Gauge.DataPoints, otlpPoint{Attributes: append(account, kv("reason", reason(r.Err))), TimeUnixNano: ts, AsDouble: 1})
		}
		success.Gauge.DataPoints = append(success.Gauge.DataPoints, otlpPoint{Attributes: account, TimeUnixNano: ts, AsDouble: ok})
		for _, l := range r.Limits {
			attrs := []otlpKV{
				kv("provider", l.Provider), kv("account", l.Account), kv("plan", l.Plan),
				kv("window", l.Window), kv("scope", l.Scope), kv("limit", l.Name),
			}
			used.Gauge.DataPoints = append(used.Gauge.DataPoints, otlpPoint{Attributes: attrs, TimeUnixNano: ts, AsDouble: l.Percent})
			if l.ResetsAt > 0 {
				resets.Gauge.DataPoints = append(resets.Gauge.DataPoints, otlpPoint{Attributes: attrs, TimeUnixNano: ts, AsDouble: float64(l.ResetsAt)})
			}
		}
	}
	activeM := otlpMetric{Name: metricActive, Description: "1 for the build account in use, 0 for the other build accounts."}
	exhausted := otlpMetric{Name: metricExhausted, Description: "1 when every build account is at the rotation bar."}
	rank := otlpMetric{Name: metricRank, Description: "Where a build account stands in the rotation: 1 for the active one, 2 for the next, and so on."}
	pinnedM := otlpMetric{Name: metricPinned, Description: "1 for the build account picked by hand, 0 for the others."}
	freeAt := otlpMetric{Name: metricFreeAt, Description: "When a build account can be used again, in unix seconds; 0 when it can now, -1 when a limit it hit gave no reset."}
	preferredAt := otlpMetric{Name: metricPreferredAt, Description: "When a build account has the preferred model to use again, in unix seconds; 0 when it has now, -1 when a limit it hit gave no reset or it does not offer the model."}
	flag := func(on bool) float64 {
		if on {
			return 1
		}
		return 0
	}
	if rot != nil {
		for _, a := range rot.Accounts {
			who := []otlpKV{kv("provider", rot.Provider), kv("account", a)}
			activeM.Gauge.DataPoints = append(activeM.Gauge.DataPoints, otlpPoint{Attributes: who, TimeUnixNano: ts, AsDouble: flag(strings.EqualFold(a, rot.Active))})
			pinnedM.Gauge.DataPoints = append(pinnedM.Gauge.DataPoints, otlpPoint{Attributes: who, TimeUnixNano: ts, AsDouble: flag(strings.EqualFold(a, rot.Pinned))})
		}
		for _, s := range rot.Standings {
			who := []otlpKV{kv("provider", rot.Provider), kv("account", s.Account)}
			rank.Gauge.DataPoints = append(rank.Gauge.DataPoints, otlpPoint{Attributes: who, TimeUnixNano: ts, AsDouble: float64(s.Rank)})
			if s.Read {
				freeAt.Gauge.DataPoints = append(freeAt.Gauge.DataPoints, otlpPoint{Attributes: who, TimeUnixNano: ts, AsDouble: float64(s.FreeAt)})
				preferredAt.Gauge.DataPoints = append(preferredAt.Gauge.DataPoints, otlpPoint{Attributes: append(who, kv("prefer", rot.Prefer)), TimeUnixNano: ts, AsDouble: float64(s.PreferredAt)})
			}
		}
		v := 0.0
		if rot.Exhausted {
			v = 1
		}
		exhausted.Gauge.DataPoints = append(exhausted.Gauge.DataPoints, otlpPoint{Attributes: []otlpKV{kv("provider", rot.Provider)}, TimeUnixNano: ts, AsDouble: v})
	}
	var metrics []otlpMetric
	for _, m := range []otlpMetric{used, resets, success, failure, activeM, exhausted, rank, pinnedM, freeAt, preferredAt} {
		if len(m.Gauge.DataPoints) > 0 {
			metrics = append(metrics, m)
		}
	}
	if len(metrics) == 0 {
		return nil, 0
	}
	points := 0
	for _, m := range metrics {
		points += len(m.Gauge.DataPoints)
	}
	body := map[string]any{
		"resourceMetrics": []any{map[string]any{
			"resource": map[string]any{"attributes": []otlpKV{
				kv("service.name", "usage-probe"),
				kv("service.instance.id", "usage-probe"),
			}},
			"scopeMetrics": []any{map[string]any{
				"scope":   map[string]any{"name": "usage-probe"},
				"metrics": metrics,
			}},
		}},
	}
	out, _ := json.Marshal(body)
	return out, points
}

// push sends the payload to an OTLP/HTTP receiver's /v1/metrics.
func push(ctx context.Context, endpoint string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/metrics", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("push metrics: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("push metrics: %s: %s", res.Status, bytes.TrimSpace(msg))
	}
	return nil
}
