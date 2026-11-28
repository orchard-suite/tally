package agents

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

// A run is one working session of an agent, as Vantage names it: the id the
// agent sends as X-Vantage-Run. On a laptop the bridge sends the agent
// runtime's own session id, so a run is the conversation the shipper posts
// under that id and every subagent conversation inside it.

// RunOf is the run a conversation belongs to: its id without the agent's
// prefix and without a subagent's tail. A conversation posted bare under a
// digest (Bare) belongs to none.
func RunOf(conversation string) string {
	_, rest, ok := strings.Cut(conversation, ":")
	if !ok {
		return ""
	}
	run, _, _ := strings.Cut(rest, "/")
	return run
}

var runID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// ValidRun says whether run can be a run id: letters, digits, dots,
// dashes, and underscores, at most 100.
func ValidRun(run string) bool { return runID.MatchString(run) }

// Claim is a run taking an item in Vantage: the run, the item's key, the
// agent as Vantage names it (its name on its host), and when.
type Claim struct {
	Run   string    `json:"run"`
	Item  string    `json:"item"`
	Agent string    `json:"agent"`
	At    time.Time `json:"at"`
}

// ScopeNode is one scope in the estate's tree: a program, a department, or
// any level the estate names.
type ScopeNode struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Parent string `json:"parent,omitempty"`
}

const runSchema = `
CREATE TABLE IF NOT EXISTS run_claims (
  run   TEXT NOT NULL,
  item  TEXT NOT NULL,
  agent TEXT NOT NULL,
  at    INTEGER NOT NULL,
  PRIMARY KEY (run, item, at)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS run_items (
  item    TEXT PRIMARY KEY,
  scope   TEXT NOT NULL,
  read_at INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS run_scopes (
  id     TEXT PRIMARY KEY,
  name   TEXT NOT NULL,
  kind   TEXT NOT NULL,
  parent TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS run_feed (
  id       INTEGER PRIMARY KEY CHECK (id = 1),
  position INTEGER NOT NULL
);
`

// FeedPosition is the last position of Vantage's change feed the store has
// taken, zero before the first.
func (s *Store) FeedPosition(ctx context.Context) (int64, error) {
	var p int64
	err := s.db.QueryRowContext(ctx, `SELECT position FROM run_feed WHERE id = 1`).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return p, err
}

// TakeFeed stores a page of claims and the feed position after it, in one
// transaction, so a page is taken once or not at all. Stale names items
// whose scope must be read again.
func (s *Store) TakeFeed(ctx context.Context, claims []Claim, stale []string, position int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range claims {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO run_claims (run, item, agent, at) VALUES (?, ?, ?, ?)`, c.Run, c.Item, c.Agent, c.At.Unix()); err != nil {
			return err
		}
	}
	for _, item := range stale {
		if _, err := tx.ExecContext(ctx, `UPDATE run_items SET read_at = 0 WHERE item = ?`, item); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO run_feed (id, position) VALUES (1, ?)`, position); err != nil {
		return err
	}
	return tx.Commit()
}

// UnscopedItems are the claimed items whose scope was never read, or read
// before before, oldest first, at most limit.
func (s *Store) UnscopedItems(ctx context.Context, before time.Time, limit int) ([]string, error) {
	r, err := s.db.QueryContext(ctx, `
SELECT c.item FROM run_claims c LEFT JOIN run_items i ON i.item = c.item
WHERE i.item IS NULL OR i.read_at < ?
GROUP BY c.item ORDER BY coalesce(max(i.read_at), 0), c.item LIMIT ?`, before.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []string
	for r.Next() {
		var item string
		if err := r.Scan(&item); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, r.Err()
}

// SetItemScope keeps an item's scope as Vantage answered it: its own, or
// its workspace's; empty is the whole estate.
func (s *Store) SetItemScope(ctx context.Context, item, scope string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO run_items (item, scope, read_at) VALUES (?, ?, ?)`, item, scope, at.Unix())
	return err
}

// SetScopes replaces the scope tree.
func (s *Store) SetScopes(ctx context.Context, tree []ScopeNode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM run_scopes`); err != nil {
		return err
	}
	for _, n := range tree {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO run_scopes (id, name, kind, parent) VALUES (?, ?, ?, ?)`, n.ID, n.Name, n.Kind, n.Parent); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Links are what the store knows of Vantage: every claim, each claimed
// item's scope, and the scope tree.
type Links struct {
	Claims []Claim
	Scopes map[string]string
	Tree   []ScopeNode
}

// Links reads every claim, item scope, and scope.
func (s *Store) Links(ctx context.Context) (Links, error) {
	l := Links{Scopes: map[string]string{}}
	r, err := s.db.QueryContext(ctx, `SELECT run, item, agent, at FROM run_claims ORDER BY run, at, item`)
	if err != nil {
		return l, err
	}
	for r.Next() {
		var c Claim
		var at int64
		if err := r.Scan(&c.Run, &c.Item, &c.Agent, &at); err != nil {
			r.Close()
			return l, err
		}
		c.At = time.Unix(at, 0).UTC()
		l.Claims = append(l.Claims, c)
	}
	r.Close()
	if err := r.Err(); err != nil {
		return l, err
	}
	r, err = s.db.QueryContext(ctx, `SELECT item, scope FROM run_items`)
	if err != nil {
		return l, err
	}
	for r.Next() {
		var item, scope string
		if err := r.Scan(&item, &scope); err != nil {
			r.Close()
			return l, err
		}
		l.Scopes[item] = scope
	}
	r.Close()
	if err := r.Err(); err != nil {
		return l, err
	}
	r, err = s.db.QueryContext(ctx, `SELECT id, name, kind, parent FROM run_scopes ORDER BY id`)
	if err != nil {
		return l, err
	}
	defer r.Close()
	for r.Next() {
		var n ScopeNode
		if err := r.Scan(&n.ID, &n.Name, &n.Kind, &n.Parent); err != nil {
			return l, err
		}
		l.Tree = append(l.Tree, n)
	}
	return l, r.Err()
}

// RunRows are every bucket machine posted for run: the conversation named
// by the run and each subagent conversation inside it.
func (s *Store) RunRows(ctx context.Context, machine, run string) ([]Row, error) {
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(run)
	return s.rows(ctx, `u.machine = ? AND (u.conversation LIKE ? ESCAPE '\' OR u.conversation LIKE ? ESCAPE '\')`, machine, "%:"+like, "%:"+like+"/%")
}
