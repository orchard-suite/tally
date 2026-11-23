package agents

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// PricesURL is LiteLLM's public table of model prices, the one most tools
// that estimate agent cost read.
const PricesURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// Rate is one model's price per token, by kind.
type Rate struct {
	Input        float64 `json:"input"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	CacheRead    float64 `json:"cache_read"`
	Output       float64 `json:"output"`
}

// Cost is what t costs at r, in dollars.
func (r Rate) Cost(t Tokens) float64 {
	return float64(t.Input)*r.Input + float64(t.CacheWrite5m)*r.CacheWrite5m + float64(t.CacheWrite1h)*r.CacheWrite1h +
		float64(t.CacheRead)*r.CacheRead + float64(t.Output)*r.Output
}

// Prices are rates by bare model name.
type Prices map[string]Rate

// Rate is model's rate, by its bare name.
func (p Prices) Rate(model string) (Rate, bool) {
	r, ok := p[Model(model)]
	return r, ok
}

var (
	regionPrefix = regexp.MustCompile(`^(us-gov|us|eu|apac|au|jp|global)\.`)
	vendorPrefix = regexp.MustCompile(`^(anthropic|openai|google|xai|meta|mistral)\.`)
	versionTail  = regexp.MustCompile(`(-v\d+(:\d+)?|@\d{8}|-\d{8}|-\d{4}-\d{2}-\d{2})$`)
)

// Model is a model's bare name: no gateway or account prefix, no vendor or
// region namespace, no build or date suffix. The same model through any
// route is one model.
func Model(raw string) string {
	m := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	m = regionPrefix.ReplaceAllString(m, "")
	m = vendorPrefix.ReplaceAllString(m, "")
	for {
		n := versionTail.ReplaceAllString(m, "")
		if n == m {
			return m
		}
		m = n
	}
}

// ParsePrices reads LiteLLM's table. A model listed under several routes
// takes its bare entry's price, or else the first route's by name.
func ParsePrices(raw []byte) (Prices, error) {
	var table map[string]json.RawMessage
	if err := json.Unmarshal(raw, &table); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := Prices{}
	exact := map[string]bool{}
	for _, k := range keys {
		var e struct {
			Input        *float64 `json:"input_cost_per_token"`
			Output       *float64 `json:"output_cost_per_token"`
			CacheWrite   *float64 `json:"cache_creation_input_token_cost"`
			CacheWrite1h *float64 `json:"cache_creation_input_token_cost_above_1hr"`
			CacheRead    *float64 `json:"cache_read_input_token_cost"`
		}
		if json.Unmarshal(table[k], &e) != nil || e.Input == nil || e.Output == nil {
			continue
		}
		name := Model(k)
		bare := strings.ToLower(k) == name
		if _, seen := out[name]; seen && (exact[name] || !bare) {
			continue
		}
		r := Rate{Input: *e.Input, Output: *e.Output, CacheWrite5m: *e.Input, CacheRead: *e.Input}
		if e.CacheWrite != nil {
			r.CacheWrite5m = *e.CacheWrite
		}
		r.CacheWrite1h = r.CacheWrite5m
		if e.CacheWrite1h != nil {
			r.CacheWrite1h = *e.CacheWrite1h
		}
		if e.CacheRead != nil {
			r.CacheRead = *e.CacheRead
		}
		out[name] = r
		exact[name] = bare
	}
	return out, nil
}
