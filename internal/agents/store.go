package agents

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store keeps every machine's works, conversations, and usage in one
// SQLite file that only this process opens.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// OpenStore opens or creates the store at path, with WAL on.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema + runSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("agents store: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the file.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS works (
  machine TEXT NOT NULL,
  id      TEXT NOT NULL,
  title   TEXT NOT NULL,
  project TEXT NOT NULL,
  PRIMARY KEY (machine, id)
);
CREATE TABLE IF NOT EXISTS conversations (
  machine TEXT NOT NULL,
  id      TEXT NOT NULL,
  work    TEXT NOT NULL,
  parent  TEXT NOT NULL,
  agent   TEXT NOT NULL,
  role    TEXT NOT NULL,
  title   TEXT NOT NULL,
  PRIMARY KEY (machine, id)
);
CREATE TABLE IF NOT EXISTS usage (
  machine        TEXT NOT NULL,
  conversation   TEXT NOT NULL,
  model          TEXT NOT NULL,
  minute         INTEGER NOT NULL,
  requests       INTEGER NOT NULL,
  input          INTEGER NOT NULL,
  cache_write_5m INTEGER NOT NULL,
  cache_write_1h INTEGER NOT NULL,
  cache_read     INTEGER NOT NULL,
  output         INTEGER NOT NULL,
  PRIMARY KEY (machine, conversation, model, minute)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS usage_minute ON usage(minute);
CREATE TABLE IF NOT EXISTS uses (
  machine      TEXT NOT NULL,
  conversation TEXT NOT NULL,
  model        TEXT NOT NULL,
  minute       INTEGER NOT NULL,
  kind         TEXT NOT NULL,
  name         TEXT NOT NULL,
  count        INTEGER NOT NULL,
  PRIMARY KEY (machine, conversation, model, minute, kind, name)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS uses_minute ON uses(minute);
CREATE TABLE IF NOT EXISTS installed (
  machine     TEXT NOT NULL,
  name        TEXT NOT NULL,
  marketplace TEXT NOT NULL,
  version     TEXT NOT NULL,
  PRIMARY KEY (machine, name, marketplace)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS machines (
  machine   TEXT PRIMARY KEY,
  last_post INTEGER NOT NULL
);
`

// Apply stores one machine's batch in one transaction. Every record
// replaces the one with its key.
func (s *Store) Apply(ctx context.Context, machine string, b Batch, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, w := range b.Works {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO works (machine, id, title, project) VALUES (?, ?, ?, ?)`, machine, w.ID, w.Title, w.Project); err != nil {
			return err
		}
	}
	for _, c := range b.Conversations {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO conversations (machine, id, work, parent, agent, role, title) VALUES (?, ?, ?, ?, ?, ?, ?)`, machine, c.ID, c.Work, c.Parent, c.Agent, c.Role, c.Title); err != nil {
			return err
		}
	}
	if len(b.Buckets) > 0 {
		st, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO usage (machine, conversation, model, minute, requests, input, cache_write_5m, cache_write_1h, cache_read, output) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, k := range b.Buckets {
			if _, err := st.ExecContext(ctx, machine, k.Conversation, k.Model, k.Minute.Unix(), k.Requests, k.Input, k.CacheWrite5m, k.CacheWrite1h, k.CacheRead, k.Output); err != nil {
				return err
			}
		}
	}
	if len(b.Uses) > 0 {
		st, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO uses (machine, conversation, model, minute, kind, name, count) VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, u := range b.Uses {
			if _, err := st.ExecContext(ctx, machine, u.Conversation, u.Model, u.Minute.Unix(), u.Kind, u.Name, u.Count); err != nil {
				return err
			}
		}
	}
	if b.Installed != nil {
		// The whole list: what the machine no longer has goes with the rest.
		if _, err := tx.ExecContext(ctx, `DELETE FROM installed WHERE machine = ?`, machine); err != nil {
			return err
		}
		for _, p := range b.Installed.Plugins {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO installed (machine, name, marketplace, version) VALUES (?, ?, ?, ?)`, machine, p.Name, p.Marketplace, p.Version); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO machines (machine, last_post) VALUES (?, ?)`, machine, now.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// Withdraw deletes everything machine posted, in one transaction, and says
// how many buckets went with it.
func (s *Store) Withdraw(ctx context.Context, machine string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var buckets int64
	for _, table := range []string{"usage", "uses", "installed", "conversations", "works", "machines"} {
		res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE machine = ?`, machine)
		if err != nil {
			return 0, err
		}
		if table == "usage" {
			buckets, _ = res.RowsAffected()
		}
	}
	return buckets, tx.Commit()
}

// Row is one bucket with its conversation and work, for a report.
type Row struct {
	Machine      string
	Conversation string
	Parent       string
	Role         string
	Work         string
	WorkTitle    string
	Project      string
	Model        string
	Minute       time.Time
	Tokens
}

// Rows are every bucket from from up to to. A bucket whose conversation
// tally has not been told about yet is its own lead work.
func (s *Store) Rows(ctx context.Context, from, to time.Time) ([]Row, error) {
	return s.rows(ctx, `u.minute >= ? AND u.minute < ?`, from.Unix(), to.Unix())
}

// rows are the buckets where says, each with its conversation and work.
func (s *Store) rows(ctx context.Context, where string, args ...any) ([]Row, error) {
	r, err := s.db.QueryContext(ctx, `
SELECT u.machine, u.conversation, coalesce(c.parent, ''), coalesce(c.role, 'lead'), coalesce(c.work, u.conversation),
       coalesce(w.title, c.title, ''), coalesce(w.project, ''), u.model, u.minute,
       u.requests, u.input, u.cache_write_5m, u.cache_write_1h, u.cache_read, u.output
FROM usage u
LEFT JOIN conversations c ON c.machine = u.machine AND c.id = u.conversation
LEFT JOIN works w ON w.machine = c.machine AND w.id = c.work
WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []Row
	for r.Next() {
		var x Row
		var minute int64
		if err := r.Scan(&x.Machine, &x.Conversation, &x.Parent, &x.Role, &x.Work, &x.WorkTitle, &x.Project, &x.Model, &minute,
			&x.Requests, &x.Input, &x.CacheWrite5m, &x.CacheWrite1h, &x.CacheRead, &x.Output); err != nil {
			return nil, err
		}
		x.Minute = time.Unix(minute, 0).UTC()
		out = append(out, x)
	}
	return out, r.Err()
}

// Sum is one machine, model, and role's tokens on one day of a span.
type Sum struct {
	Machine string
	Model   string
	Role    string
	// Day is the UTC day the tokens were spent, which picks their price.
	Day time.Time
	Tokens
}

// Sums are the tokens from from up to to, by machine, model, role, and UTC
// day.
func (s *Store) Sums(ctx context.Context, from, to time.Time) ([]Sum, error) {
	r, err := s.db.QueryContext(ctx, `
SELECT u.machine, u.model, coalesce(c.role, 'lead'), u.minute / 86400,
       sum(u.requests), sum(u.input), sum(u.cache_write_5m), sum(u.cache_write_1h), sum(u.cache_read), sum(u.output)
FROM usage u
LEFT JOIN conversations c ON c.machine = u.machine AND c.id = u.conversation
WHERE u.minute >= ? AND u.minute < ?
GROUP BY 1, 2, 3, 4`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []Sum
	for r.Next() {
		var x Sum
		var day int64
		if err := r.Scan(&x.Machine, &x.Model, &x.Role, &day, &x.Requests, &x.Input, &x.CacheWrite5m, &x.CacheWrite1h, &x.CacheRead, &x.Output); err != nil {
			return nil, err
		}
		x.Day = time.Unix(day*86400, 0).UTC()
		out = append(out, x)
	}
	return out, r.Err()
}

// LastPosts is when each machine last posted.
func (s *Store) LastPosts(ctx context.Context) (map[string]time.Time, error) {
	r, err := s.db.QueryContext(ctx, `SELECT machine, last_post FROM machines`)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]time.Time{}
	for r.Next() {
		var m string
		var at int64
		if err := r.Scan(&m, &at); err != nil {
			return nil, err
		}
		out[m] = time.Unix(at, 0).UTC()
	}
	return out, r.Err()
}

// UseSum is one skill, command, or tool over a span: how many times it was
// used, in how many conversations, and on how many machines.
type UseSum struct {
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Plugin        string `json:"plugin,omitempty"`
	Count         int64  `json:"count"`
	Conversations int64  `json:"conversations"`
	Machines      int64  `json:"machines"`
}

// UseSums are the uses from from up to to, most used first.
func (s *Store) UseSums(ctx context.Context, from, to time.Time) ([]UseSum, error) {
	r, err := s.db.QueryContext(ctx, `
SELECT kind, name, sum(count), count(DISTINCT machine || char(0) || conversation), count(DISTINCT machine)
FROM uses
WHERE minute >= ? AND minute < ?
GROUP BY 1, 2
ORDER BY 3 DESC, 1, 2`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []UseSum
	for r.Next() {
		var x UseSum
		if err := r.Scan(&x.Kind, &x.Name, &x.Count, &x.Conversations, &x.Machines); err != nil {
			return nil, err
		}
		x.Plugin = Use{Kind: x.Kind, Name: x.Name}.Plugin()
		out = append(out, x)
	}
	return out, r.Err()
}

// InstalledOn is how many machines have each plugin installed, by plugin
// name, of the machines that posted since since: one that stopped shipping
// no longer says what it has.
func (s *Store) InstalledOn(ctx context.Context, since time.Time) (map[string]int64, error) {
	r, err := s.db.QueryContext(ctx, `SELECT i.name, count(DISTINCT i.machine) FROM installed i JOIN machines m ON m.machine = i.machine WHERE m.last_post >= ? GROUP BY 1`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]int64{}
	for r.Next() {
		var name string
		var n int64
		if err := r.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, r.Err()
}

// UseRow is one machine's count of one skill, command, or tool over a span.
type UseRow struct {
	Machine string
	Use
}

// UseRows are each machine's uses from from up to to.
func (s *Store) UseRows(ctx context.Context, from, to time.Time) ([]UseRow, error) {
	r, err := s.db.QueryContext(ctx, `SELECT machine, kind, name, sum(count) FROM uses WHERE minute >= ? AND minute < ? GROUP BY 1, 2, 3`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []UseRow
	for r.Next() {
		var x UseRow
		if err := r.Scan(&x.Machine, &x.Kind, &x.Name, &x.Count); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, r.Err()
}

// Reach is how much one plugin or one of its skills was used: how many
// times, and on how many machines.
type Reach struct {
	Count    int64
	Machines int64
}

// Plugins is each plugin's reach: every use of its skills, commands, and
// tools, a machine counted once however many of its parts it used.
func Plugins(rows []UseRow) map[string]Reach {
	return reach(rows, func(u Use) string { return u.Plugin() })
}

// Skills is each plugin skill's reach, by <plugin>:<skill>: the times the
// agent loaded it and the times a person typed it, a machine counted once.
func Skills(rows []UseRow) map[string]Reach {
	return reach(rows, func(u Use) string {
		if u.Kind == UseTool || u.Plugin() == "" {
			return ""
		}
		return u.Name
	})
}

func reach(rows []UseRow, of func(Use) string) map[string]Reach {
	counts := map[string]int64{}
	machines := map[string]map[string]bool{}
	for _, r := range rows {
		k := of(r.Use)
		if k == "" {
			continue
		}
		counts[k] += r.Count
		if machines[k] == nil {
			machines[k] = map[string]bool{}
		}
		machines[k][r.Machine] = true
	}
	out := make(map[string]Reach, len(counts))
	for k, n := range counts {
		out[k] = Reach{Count: n, Machines: int64(len(machines[k]))}
	}
	return out
}
