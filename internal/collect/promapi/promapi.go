// Package promapi asks a Prometheus for instant and range queries.
package promapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client reads one Prometheus.
type Client struct {
	base  string
	http  *http.Client
	token func(context.Context) (string, error)
}

// Option changes how a Client asks.
type Option func(*Client)

// WithToken sends a bearer token from token on every request, for a
// Prometheus that wants one, such as an Azure Monitor workspace.
func WithToken(token func(context.Context) (string, error)) Option {
	return func(c *Client) { c.token = token }
}

func New(base string, opts ...Option) *Client {
	c := &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 90 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Sample is one instant result: its labels and value.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Point is one step of a range result.
type Point struct {
	Time  time.Time
	Value float64
}

// Series is one range result.
type Series struct {
	Labels map[string]string
	Points []Point
}

// Query runs an instant query and returns its vector.
func (c *Client) Query(ctx context.Context, expr string) ([]Sample, error) {
	raw, err := c.get(ctx, "/api/v1/query", url.Values{"query": {expr}})
	if err != nil {
		return nil, err
	}
	return ParseVector(raw)
}

// QueryRange runs a range query at step and returns its matrix.
func (c *Client) QueryRange(ctx context.Context, expr string, from, to time.Time, step time.Duration) ([]Series, error) {
	raw, err := c.get(ctx, "/api/v1/query_range", url.Values{
		"query": {expr},
		"start": {strconv.FormatInt(from.Unix(), 10)},
		"end":   {strconv.FormatInt(to.Unix(), 10)},
		"step":  {strconv.FormatInt(int64(step.Seconds()), 10)},
	})
	if err != nil {
		return nil, err
	}
	return ParseMatrix(raw)
}

func (c *Client) get(ctx context.Context, path string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.token != nil {
		token, err := c.token(ctx)
		if err != nil {
			return nil, fmt.Errorf("prometheus sign-in: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus %s: %s: %s", path, res.Status, firstLine(raw))
	}
	return raw, nil
}

type envelope struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// ParseVector reads an instant query answer.
func ParseVector(raw []byte) ([]Sample, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("prometheus answer: %w", err)
	}
	if env.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", env.Error)
	}
	if env.Data.ResultType != "vector" {
		return nil, fmt.Errorf("prometheus: want a vector, got %s", env.Data.ResultType)
	}
	var rows []struct {
		Metric map[string]string `json:"metric"`
		Value  [2]any            `json:"value"`
	}
	if err := json.Unmarshal(env.Data.Result, &rows); err != nil {
		return nil, fmt.Errorf("prometheus vector: %w", err)
	}
	out := make([]Sample, 0, len(rows))
	for _, r := range rows {
		v, err := number(r.Value[1])
		if err != nil {
			return nil, err
		}
		out = append(out, Sample{Labels: r.Metric, Value: v})
	}
	return out, nil
}

// ParseMatrix reads a range query answer.
func ParseMatrix(raw []byte) ([]Series, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("prometheus answer: %w", err)
	}
	if env.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", env.Error)
	}
	if env.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("prometheus: want a matrix, got %s", env.Data.ResultType)
	}
	var rows []struct {
		Metric map[string]string `json:"metric"`
		Values [][2]any          `json:"values"`
	}
	if err := json.Unmarshal(env.Data.Result, &rows); err != nil {
		return nil, fmt.Errorf("prometheus matrix: %w", err)
	}
	out := make([]Series, 0, len(rows))
	for _, r := range rows {
		s := Series{Labels: r.Metric}
		for _, p := range r.Values {
			t, _ := p[0].(float64)
			v, err := number(p[1])
			if err != nil {
				return nil, err
			}
			s.Points = append(s.Points, Point{Time: time.Unix(int64(t), 0).UTC(), Value: v})
		}
		out = append(out, s)
	}
	return out, nil
}

func number(v any) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("prometheus: value %v is not a string", v)
	}
	return strconv.ParseFloat(s, 64)
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return s
}
