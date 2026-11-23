package ship

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"

	_ "modernc.org/sqlite"
)

// T3 is what T3 Code knows about the sessions it ran: which thread each
// session belongs to, and how threads delegate to one another.
type T3 struct {
	// BySession is a native session id to its T3 thread id: a Claude Code
	// session id, or the ACP session key of an Ion session.
	BySession map[string]string
	Threads   map[string]Thread
}

// Thread is one T3 thread.
type Thread struct {
	ID      string
	Title   string
	Project string
	// Parent is the thread that delegated this one, empty for one a person
	// started.
	Parent string
}

// Root is the thread a person started that id descends from.
func (t T3) Root(id string) string {
	seen := map[string]bool{}
	for {
		th, ok := t.Threads[id]
		if !ok || th.Parent == "" || seen[id] {
			return id
		}
		seen[id] = true
		id = th.Parent
	}
}

// ReadT3 reads T3 Code's state database, read-only. A missing database is
// no T3 at all, not an error.
func ReadT3(ctx context.Context, path string) (T3, error) {
	out := T3{BySession: map[string]string{}, Threads: map[string]Thread{}}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return out, err
	}
	defer db.Close()

	projects := map[string]string{}
	if err := rows(ctx, db, `SELECT project_id, title FROM projection_projects`, func(r *sql.Rows) error {
		var id, title string
		if err := r.Scan(&id, &title); err != nil {
			return err
		}
		projects[id] = title
		return nil
	}); err != nil {
		return out, fmt.Errorf("t3 projects: %w", err)
	}
	if err := rows(ctx, db, `SELECT thread_id, title, project_id, coalesce(json_extract(payload_json, '$.lineage.parentThreadId'), '') FROM orchestration_v2_projection_threads`, func(r *sql.Rows) error {
		var th Thread
		var project string
		if err := r.Scan(&th.ID, &th.Title, &project, &th.Parent); err != nil {
			return err
		}
		th.Project = projects[project]
		out.Threads[th.ID] = th
		return nil
	}); err != nil {
		return out, fmt.Errorf("t3 threads: %w", err)
	}
	if err := rows(ctx, db, `SELECT coalesce(json_extract(payload_json, '$.nativeThreadRef.nativeId'), ''), thread_id FROM orchestration_v2_projection_provider_threads WHERE json_extract(payload_json, '$.driver') IN ('claudeAgent', 'acpRegistry')`, func(r *sql.Rows) error {
		var session, thread string
		if err := r.Scan(&session, &thread); err != nil {
			return err
		}
		if session != "" {
			out.BySession[session] = thread
		}
		return nil
	}); err != nil {
		return out, fmt.Errorf("t3 provider threads: %w", err)
	}
	return out, nil
}

func rows(ctx context.Context, db *sql.DB, q string, each func(*sql.Rows) error) error {
	r, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer r.Close()
	for r.Next() {
		if err := each(r); err != nil {
			return err
		}
	}
	return r.Err()
}
