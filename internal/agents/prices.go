package agents

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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

// Version is one model's price from a day on, as the estate set it.
type Version struct {
	// From is the first day the price is in force, at 00:00 UTC.
	From time.Time
	Rate
}

// Table is the price of every model on any day: the estate's own dated
// versions first, and the public table for a model the estate does not
// price, or a day before its first version. A new price is a new version,
// so the days before it keep theirs.
type Table struct {
	// Estate are each bare model's versions, oldest first.
	Estate map[string][]Version
	// Public is the public table as last read.
	Public Prices
}

// Table is p alone, with no estate prices.
func (p Prices) Table() Table { return Table{Public: p} }

// Empty says the table prices nothing yet.
func (t Table) Empty() bool { return len(t.Estate) == 0 && len(t.Public) == 0 }

// Rate is model's rate on at's day.
func (t Table) Rate(model string, at time.Time) (Rate, bool) {
	m := Model(model)
	vs := t.Estate[m]
	if i := sort.Search(len(vs), func(i int) bool { return vs[i].From.After(at) }); i > 0 {
		return vs[i-1].Rate, true
	}
	r, ok := t.Public[m]
	return r, ok
}

// perMillion is how the estate's file states a price: dollars per million
// tokens.
const perMillion = 1e6

// ParseEstatePrices reads the estate's price file (PRICES_FILE):
//
//	models:
//	  claude-opus-5-5:
//	    - from: 2026-11-01
//	      input: 5
//	      output: 25
//	      cache_read: 0.5
//	      cache_write: 6.25
//	      cache_write_1h: 10
//
// Prices are dollars per million tokens. cache_write is the five-minute
// write; cache_write_1h defaults to it. A model's name is made bare, and two
// versions of one model may not start on one day.
func ParseEstatePrices(raw []byte) (map[string][]Version, error) {
	var f struct {
		Models map[string][]struct {
			From         string   `yaml:"from"`
			Input        *float64 `yaml:"input"`
			Output       *float64 `yaml:"output"`
			CacheRead    *float64 `yaml:"cache_read"`
			CacheWrite   *float64 `yaml:"cache_write"`
			CacheWrite1h *float64 `yaml:"cache_write_1h"`
		} `yaml:"models"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	out := map[string][]Version{}
	for name, versions := range f.Models {
		m := Model(name)
		for _, v := range versions {
			from, err := time.Parse(time.DateOnly, v.From)
			if err != nil {
				return nil, fmt.Errorf("%s: from %q is not a date (YYYY-MM-DD)", name, v.From)
			}
			if v.Input == nil || v.Output == nil || v.CacheRead == nil || v.CacheWrite == nil {
				return nil, fmt.Errorf("%s from %s: input, output, cache_read, and cache_write are all set", name, v.From)
			}
			r := Rate{Input: *v.Input / perMillion, Output: *v.Output / perMillion, CacheRead: *v.CacheRead / perMillion, CacheWrite5m: *v.CacheWrite / perMillion}
			r.CacheWrite1h = r.CacheWrite5m
			if v.CacheWrite1h != nil {
				r.CacheWrite1h = *v.CacheWrite1h / perMillion
			}
			if r.Input < 0 || r.Output < 0 || r.CacheRead < 0 || r.CacheWrite5m < 0 || r.CacheWrite1h < 0 {
				return nil, fmt.Errorf("%s from %s: a negative price", name, v.From)
			}
			out[m] = append(out[m], Version{From: from, Rate: r})
		}
		vs := out[m]
		sort.Slice(vs, func(a, b int) bool { return vs[a].From.Before(vs[b].From) })
		for i := 1; i < len(vs); i++ {
			if vs[i].From.Equal(vs[i-1].From) {
				return nil, fmt.Errorf("%s: two versions from %s", m, vs[i].From.Format(time.DateOnly))
			}
		}
	}
	return out, nil
}
