// Package vantage ties agent runs to the work they did: it follows a
// Vantage hub's change feed for each claim, which names the run, the item,
// and the agent (VAN-2009), reads each claimed item's scope, and reads the
// scope tree. The agents store keeps what it reads, and the agents
// collector prices the runs through it.
package vantage

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
	"strings"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
	"github.com/orchard-suite/tally/internal/collect"
)

// Change is one event of the hub's change feed, as much of it as tally
// reads.
type Change struct {
	Position int64     `json:"position"`
	Item     string    `json:"item"`
	Type     string    `json:"type"`
	At       time.Time `json:"at"`
	Agent    string    `json:"agent"`
	// Data is the event's own; its shape is the event type's, and a claim's
	// names its run.
	Data json.RawMessage `json:"data"`
}

// Page is one answer of GET /api/v1/changes.
type Page struct {
	Changes []Change `json:"changes"`
	Next    int64    `json:"next"`
}

// Fold reads a page: each claim made by a run, and each item whose scope
// may have changed (made, scoped, or claimed for the first time here). A
// claim the hub made for a person with no run, or with no agent, is no
// agent run and is left out.
func Fold(p Page) (claims []agents.Claim, stale []string) {
	for _, c := range p.Changes {
		switch c.Type {
		case "claimed":
			var d struct {
				Run string `json:"run"`
			}
			if json.Unmarshal(c.Data, &d) != nil {
				continue
			}
			run := d.Run
			if c.Agent == "" || run == "" || strings.HasPrefix(run, "user:") || strings.HasPrefix(run, "agent:") || !agents.ValidRun(run) {
				continue
			}
			claims = append(claims, agents.Claim{Run: run, Item: c.Item, Agent: c.Agent, At: c.At.UTC()})
		case "created", "scoped":
			stale = append(stale, c.Item)
		}
	}
	return claims, stale
}

// Tree is GET /api/v1/scopes as tally reads it.
type Tree struct {
	Scopes []agents.ScopeNode `json:"scopes"`
}

// Store is where the collector keeps what it reads.
type Store interface {
	FeedPosition(ctx context.Context) (int64, error)
	TakeFeed(ctx context.Context, claims []agents.Claim, stale []string, position int64) error
	UnscopedItems(ctx context.Context, before time.Time, limit int) ([]string, error)
	SetItemScope(ctx context.Context, item, scope string, at time.Time) error
	SetScopes(ctx context.Context, tree []agents.ScopeNode) error
}

// Limits on one round, so a hub with a long history is read over rounds.
const (
	pageSize = 500
	maxPages = 40
	// maxItems is the most item scopes read in one round.
	maxItems = 200
	// scopeAge is how old an item's scope may grow before it is read again,
	// since a workspace's scope can change with no event on its items.
	scopeAge = 24 * time.Hour
)

// Collector follows the hub.
type Collector struct {
	base     string
	token    string
	store    Store
	log      *slog.Logger
	inst     *collect.Instruments
	client   *http.Client
	interval time.Duration
}

// FromEnv turns the collector on with VANTAGE_URL and VANTAGE_TOKEN, a hub
// token whose person sees every item the estate's agents work. It is off
// without them, and without the agents store it writes into.
func FromEnv(log *slog.Logger, inst *collect.Instruments, store Store) (*Collector, error) {
	base, token := strings.TrimRight(os.Getenv("VANTAGE_URL"), "/"), os.Getenv("VANTAGE_TOKEN")
	if base == "" || token == "" {
		log.Info("vantage collector off: VANTAGE_URL and VANTAGE_TOKEN are not both set")
		return nil, nil
	}
	if store == nil {
		return nil, errors.New("vantage collector: VANTAGE_URL is set but the agents collector is off (set AGENTS_DB)")
	}
	return New(base, token, store, log, inst, collect.Interval(os.Getenv("VANTAGE_INTERVAL"), 5*time.Minute)), nil
}

// New is a collector for the hub at base.
func New(base, token string, store Store, log *slog.Logger, inst *collect.Instruments, interval time.Duration) *Collector {
	return &Collector{base: base, token: token, store: store, log: log, inst: inst, client: &http.Client{Timeout: time.Minute}, interval: interval}
}

// Name is the collector's name.
func (c *Collector) Name() string { return "vantage" }

// Interval is how often a round runs when the last one worked.
func (c *Collector) Interval() time.Duration { return c.interval }

// Run follows the hub every interval until ctx ends.
func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.Round)
}

// Round reads the feed from where the store left it, the scopes of items
// not read lately, and the tree. A failed read stops the round with what it
// took so far kept.
func (c *Collector) Round(ctx context.Context) error {
	pos, err := c.store.FeedPosition(ctx)
	if err != nil {
		return err
	}
	taken := 0
	for range maxPages {
		var p Page
		if err := c.get(ctx, fmt.Sprintf("/api/v1/changes?after=%d&limit=%d", pos, pageSize), &p); err != nil {
			return fmt.Errorf("changes: %w", err)
		}
		claims, stale := Fold(p)
		if err := c.store.TakeFeed(ctx, claims, stale, p.Next); err != nil {
			return err
		}
		taken += len(claims)
		if len(p.Changes) < pageSize || p.Next <= pos {
			pos = p.Next
			break
		}
		pos = p.Next
	}
	now := time.Now()
	items, err := c.store.UnscopedItems(ctx, now.Add(-scopeAge), maxItems)
	if err != nil {
		return err
	}
	for _, item := range items {
		var card struct {
			Card struct {
				Scope *string `json:"scope"`
			} `json:"card"`
		}
		if err := c.get(ctx, "/api/v1/items/"+url.PathEscape(item)+"?fields=scope", &card); err != nil {
			return fmt.Errorf("item %s: %w", item, err)
		}
		scope := ""
		if card.Card.Scope != nil {
			scope = *card.Card.Scope
		}
		if err := c.store.SetItemScope(ctx, item, scope, now); err != nil {
			return err
		}
	}
	var tree Tree
	if err := c.get(ctx, "/api/v1/scopes", &tree); err != nil {
		return fmt.Errorf("scopes: %w", err)
	}
	if err := c.store.SetScopes(ctx, tree.Scopes); err != nil {
		return err
	}
	c.log.InfoContext(ctx, "vantage read", "position", pos, "claims", taken, "items", len(items), "scopes", len(tree.Scopes))
	return nil
}

func (c *Collector) get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		c.inst.Upstream(ctx, "vantage", collect.Failed)
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		c.inst.Upstream(ctx, "vantage", collect.Refused)
		return fmt.Errorf("refused: %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		c.inst.Upstream(ctx, "vantage", collect.Failed)
		return errors.New(resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(into); err != nil {
		c.inst.Upstream(ctx, "vantage", collect.Failed)
		return err
	}
	c.inst.Upstream(ctx, "vantage", collect.OK)
	return nil
}
