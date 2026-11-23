// Package actions reads what GitHub bills for Actions, by repository and
// runner kind, from its billing usage API. It turns on with GITHUB_BILLING,
// the accounts to read, and GITHUB_BILLING_TOKEN, a token that may read
// their billing.
package actions

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

	"github.com/orchard-suite/tally/internal/collect"
)

// Account kinds.
const (
	User = "user"
	Org  = "org"
)

// Account is one GitHub account whose bill is read.
type Account struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Item is one day of one repository's Actions on one SKU, such as Linux
// minutes on a hosted runner.
type Item struct {
	Account    string    `json:"account"`
	Repository string    `json:"repository"`
	SKU        string    `json:"sku"`
	Date       time.Time `json:"date"`
	// Minutes is the quantity when GitHub counts the SKU in minutes.
	Minutes float64 `json:"minutes"`
	// Gross is the list price of the use, and Net what is owed after the
	// plan's included minutes and any discount.
	Gross float64 `json:"gross"`
	Net   float64 `json:"net"`
}

// Snapshot is one round: this month and last month, every account.
type Snapshot struct {
	At    time.Time `json:"at"`
	Items []Item    `json:"items"`
}

// ParseAccounts reads GITHUB_BILLING: user:<name> and org:<name>, comma
// separated.
func ParseAccounts(s string) ([]Account, error) {
	var out []Account
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kind, name, ok := strings.Cut(part, ":")
		if !ok || (kind != User && kind != Org) || name == "" {
			return nil, fmt.Errorf("GITHUB_BILLING: want user:<name> or org:<name>, got %q", part)
		}
		out = append(out, Account{Kind: kind, Name: name})
	}
	if len(out) == 0 {
		return nil, errors.New("GITHUB_BILLING names no account")
	}
	return out, nil
}

// Parse reads one billing usage answer and keeps its Actions items. A
// repository named without its owner is given the account's.
func Parse(account string, raw []byte) ([]Item, error) {
	var body struct {
		UsageItems *[]struct {
			Date           string  `json:"date"`
			Product        string  `json:"product"`
			SKU            string  `json:"sku"`
			Quantity       float64 `json:"quantity"`
			UnitType       string  `json:"unitType"`
			GrossAmount    float64 `json:"grossAmount"`
			NetAmount      float64 `json:"netAmount"`
			RepositoryName string  `json:"repositoryName"`
		} `json:"usageItems"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("github billing: %w", err)
	}
	if body.UsageItems == nil {
		return nil, errors.New("github billing: the answer has no usageItems")
	}
	var out []Item
	for _, u := range *body.UsageItems {
		if !strings.EqualFold(u.Product, "actions") {
			continue
		}
		day, err := parseDate(u.Date)
		if err != nil {
			return nil, err
		}
		repo := u.RepositoryName
		if repo != "" && !strings.Contains(repo, "/") {
			repo = account + "/" + repo
		}
		it := Item{Account: account, Repository: repo, SKU: u.SKU, Date: day, Gross: u.GrossAmount, Net: u.NetAmount}
		if strings.EqualFold(u.UnitType, "minutes") {
			it.Minutes = u.Quantity
		}
		out = append(out, it)
	}
	return out, nil
}

// parseDate reads an item's date, a day or a time, as its UTC day.
func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return day(t), nil
	}
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("github billing: date %q", s)
}

func day(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Collector is the actions collector.
type Collector struct {
	accounts []Account
	token    string
	base     string
	http     *http.Client
	interval time.Duration
	log      *slog.Logger
	inst     *collect.Instruments
	latest   atomic.Pointer[Snapshot]
}

// FromEnv builds the collector, or nil when GITHUB_BILLING is unset.
func FromEnv(log *slog.Logger, inst *collect.Instruments) (*Collector, error) {
	v := os.Getenv("GITHUB_BILLING")
	if v == "" {
		return nil, nil
	}
	accounts, err := ParseAccounts(v)
	if err != nil {
		return nil, err
	}
	token := os.Getenv("GITHUB_BILLING_TOKEN")
	if token == "" {
		return nil, errors.New("GITHUB_BILLING needs GITHUB_BILLING_TOKEN, a token that may read the accounts' billing")
	}
	return &Collector{
		accounts: accounts,
		token:    token,
		base:     "https://api.github.com",
		http:     &http.Client{Timeout: 60 * time.Second},
		interval: collect.Interval(os.Getenv("GITHUB_BILLING_INTERVAL"), 2*time.Hour),
		log:      log,
		inst:     inst,
	}, nil
}

func (c *Collector) Name() string { return "actions" }

// Latest is the last round, or nil before the first.
func (c *Collector) Latest() *Snapshot { return c.latest.Load() }

func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.round)
}

func (c *Collector) round(ctx context.Context) error {
	now := time.Now().UTC()
	this := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	months := []time.Time{this.AddDate(0, -1, 0), this}
	var items []Item
	for _, a := range c.accounts {
		for _, m := range months {
			got, err := c.read(ctx, a, m)
			if err != nil {
				return err
			}
			items = append(items, got...)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Date.Before(items[j].Date) })
	c.latest.Store(&Snapshot{At: now, Items: items})
	c.log.InfoContext(ctx, "actions billing read", "accounts", len(c.accounts), "items", len(items))
	return nil
}

func (c *Collector) read(ctx context.Context, a Account, month time.Time) ([]Item, error) {
	path := "/users/" + url.PathEscape(a.Name) + "/settings/billing/usage"
	if a.Kind == Org {
		path = "/organizations/" + url.PathEscape(a.Name) + "/settings/billing/usage"
	}
	q := url.Values{"year": {strconv.Itoa(month.Year())}, "month": {strconv.Itoa(int(month.Month()))}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := c.http.Do(req)
	if err != nil {
		c.inst.Upstream(ctx, "github", collect.Failed)
		return nil, fmt.Errorf("github billing %s:%s: %w", a.Kind, a.Name, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		c.inst.Upstream(ctx, "github", collect.Failed)
		return nil, err
	}
	switch {
	case res.StatusCode == http.StatusTooManyRequests || (res.StatusCode == http.StatusForbidden && res.Header.Get("X-RateLimit-Remaining") == "0"):
		c.inst.Upstream(ctx, "github", collect.Refused)
		return nil, fmt.Errorf("github billing %s:%s: %s", a.Kind, a.Name, res.Status)
	case res.StatusCode != http.StatusOK:
		c.inst.Upstream(ctx, "github", collect.Failed)
		return nil, fmt.Errorf("github billing %s:%s: %s: %s", a.Kind, a.Name, res.Status, firstLine(raw))
	}
	c.inst.Upstream(ctx, "github", collect.OK)
	return Parse(a.Name, raw)
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
