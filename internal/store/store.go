// Package store holds the derived state database described in §12.
//
// Nothing here is authoritative: repositories and ignore rules live in YAML
// (§5), so deleting this directory loses history and caches, never intent. The
// reconciler below is what keeps that promise honest — state rows are rebuilt
// from configuration on every open.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion is the newest migration this binary knows. A database stamped
// with a higher number refuses to open, rather than being silently downgraded.
const SchemaVersion = 3

// migrations are applied in order; index 0 brings a fresh database to version 1.
// They are forward-only by design.
var migrations = []string{
	`
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS repo_state (
    repo_id      TEXT PRIMARY KEY,
    path         TEXT NOT NULL,
    config_hash  TEXT NOT NULL DEFAULT '',
    last_scan_at INTEGER,
    orphan       INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS repo_state_path ON repo_state(path);

CREATE TABLE IF NOT EXISTS audit (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         INTEGER NOT NULL,
    event      TEXT NOT NULL,
    tool       TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    result     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_at ON audit(at);
`,

	// Migration 2: the read path. Inventory snapshots and plans are derived
	// from configuration plus the network, and are safe to lose.
	`
CREATE TABLE IF NOT EXISTS dependencies (
    repo_id    TEXT NOT NULL,
    target     TEXT NOT NULL,
    name       TEXT NOT NULL,
    current    TEXT NOT NULL,
    direct     INTEGER NOT NULL DEFAULT 0,
    scanned_at INTEGER NOT NULL,
    PRIMARY KEY (repo_id, target, name)
);

CREATE TABLE IF NOT EXISTS advisories (
    repo_id  TEXT NOT NULL,
    target   TEXT NOT NULL,
    package  TEXT NOT NULL,
    version  TEXT NOT NULL,
    id       TEXT NOT NULL,
    severity TEXT NOT NULL DEFAULT 'unknown',
    payload  TEXT NOT NULL DEFAULT '',
    seen_at  INTEGER NOT NULL,
    PRIMARY KEY (repo_id, target, package, version, id)
);

CREATE TABLE IF NOT EXISTS plans (
    plan_id    TEXT PRIMARY KEY,
    repo_id    TEXT NOT NULL,
    target     TEXT NOT NULL,
    ecosystem  TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    plan_json  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS plans_repo ON plans(repo_id, created_at);
`,

	// Migration 3: the write path. A run is the record of one branch.
	`
CREATE TABLE IF NOT EXISTS runs (
    run_id     TEXT PRIMARY KEY,
    repo_id    TEXT NOT NULL,
    plan_id    TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    branch     TEXT NOT NULL,
    worktree   TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL,
    commit_sha TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS runs_repo ON runs(repo_id, created_at);
CREATE INDEX IF NOT EXISTS runs_status ON runs(status);
`,
}

// Store is an open state database.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates the state directory, opens the database, and applies migrations.
func Open(stateDir string) (*Store, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	path := filepath.Join(stateDir, "state.db")

	q := url.Values{}
	q.Set("_pragma", "busy_timeout(5000)")
	q.Set("_pragma", "journal_mode(WAL)")
	q.Set("_pragma", "foreign_keys(1)")
	dsn := "file:" + path + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// A single connection keeps WAL behaviour predictable for a local CLI and
	// avoids SQLITE_BUSY storms between a timer run and an interactive session.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// RawExec runs a statement against the state database. It exists for tests and
// diagnostics; application code should use the typed methods below.
func (s *Store) RawExec(query string, args ...any) (sql.Result, error) {
	return s.db.Exec(query, args...)
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var current int
	err := s.db.QueryRow(`SELECT version FROM schema_version LIMIT 1`).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		current = 0
	case err != nil:
		// The table does not exist yet on a brand-new database.
		var n int
		if e2 := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_version'`).Scan(&n); e2 != nil {
			return fmt.Errorf("inspect schema: %w", err)
		}
		if n == 0 {
			current = 0
		} else {
			return fmt.Errorf("read schema version: %w", err)
		}
	}

	if current > SchemaVersion {
		return fmt.Errorf("state database is at schema %d, but this binary knows %d: refusing to open",
			current, SchemaVersion)
	}
	for v := current; v < SchemaVersion; v++ {
		if _, err := s.db.Exec(migrations[v]); err != nil {
			return fmt.Errorf("apply migration %d: %w", v+1, err)
		}
		if _, err := s.db.Exec(`DELETE FROM schema_version`); err != nil {
			return fmt.Errorf("reset schema version: %w", err)
		}
		if _, err := s.db.Exec(`INSERT INTO schema_version(version) VALUES(?)`, v+1); err != nil {
			return fmt.Errorf("stamp schema version: %w", err)
		}
	}
	return nil
}

// RepoState is the derived per-repository row.
type RepoState struct {
	RepoID     string `json:"repo_id"`
	Path       string `json:"path"`
	ConfigHash string `json:"config_hash"`
	LastScanAt *int64 `json:"last_scan_at,omitempty"`
	Orphan     bool   `json:"orphan"`
	UpdatedAt  int64  `json:"updated_at"`
}

// Reconcile brings repo_state in line with configuration: upserts every tracked
// repository and marks rows whose config entry vanished as orphaned rather than
// deleting them, so branches and runs stay attributable (§5).
func (s *Store) Reconcile(ctx context.Context, repos []RepoRef) error {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	live := make(map[string]bool, len(repos))
	for _, r := range repos {
		live[r.ID] = true
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO repo_state(repo_id, path, config_hash, orphan, updated_at)
			VALUES(?,?,?,0,?)
			ON CONFLICT(repo_id) DO UPDATE SET
				path=excluded.path,
				config_hash=excluded.config_hash,
				orphan=0,
				updated_at=excluded.updated_at`,
			r.ID, r.Path, r.Hash, now); err != nil {
			return fmt.Errorf("upsert repo %s: %w", r.ID, err)
		}
	}

	rows, err := tx.QueryContext(ctx, `SELECT repo_id FROM repo_state WHERE orphan=0`)
	if err != nil {
		return fmt.Errorf("scan repo_state: %w", err)
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan repo_id: %w", err)
		}
		if !live[id] {
			stale = append(stale, id)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close rows: %w", err)
	}
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx,
			`UPDATE repo_state SET orphan=1, updated_at=? WHERE repo_id=?`, now, id); err != nil {
			return fmt.Errorf("orphan repo %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reconcile: %w", err)
	}
	return nil
}

// RepoRef is one tracked repository as configuration sees it.
type RepoRef struct {
	ID   string
	Path string
	Hash string
}

// States returns the current derived rows, ordered by path.
func (s *Store) States(ctx context.Context) ([]RepoState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT repo_id, path, config_hash, last_scan_at, orphan, updated_at
		FROM repo_state ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("query repo_state: %w", err)
	}
	defer rows.Close()

	var out []RepoState
	for rows.Next() {
		var st RepoState
		var last sql.NullInt64
		var orphan int
		if err := rows.Scan(&st.RepoID, &st.Path, &st.ConfigHash, &last, &orphan, &st.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan repo_state: %w", err)
		}
		if last.Valid {
			v := last.Int64
			st.LastScanAt = &v
		}
		st.Orphan = orphan != 0
		out = append(out, st)
	}
	return out, rows.Err()
}

// MarkScanned records a successful scan timestamp.
func (s *Store) MarkScanned(ctx context.Context, repoID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE repo_state SET last_scan_at=?, updated_at=? WHERE repo_id=?`,
		at.Unix(), time.Now().Unix(), repoID)
	if err != nil {
		return fmt.Errorf("mark scanned: %w", err)
	}
	return nil
}

// Stats summarises the database for status output.
type Stats struct {
	Repos   int   `json:"repos"`
	Orphans int   `json:"orphans"`
	Audit   int   `json:"audit_events"`
	Bytes   int64 `json:"bytes"`
}

// Stats gathers counts plus the on-disk size.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM repo_state`).Scan(&st.Repos); err != nil {
		return st, fmt.Errorf("count repos: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM repo_state WHERE orphan=1`).Scan(&st.Orphans); err != nil {
		return st, fmt.Errorf("count orphans: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit`).Scan(&st.Audit); err != nil {
		return st, fmt.Errorf("count audit: %w", err)
	}
	if info, err := os.Stat(s.path); err == nil {
		st.Bytes = info.Size()
	}
	return st, nil
}

// AppendAudit mirrors an audit record into the queryable table. The JSONL file
// written by package audit remains the record of truth (§12).
func (s *Store) AppendAudit(ctx context.Context, at time.Time, event, tool, detail, result string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit(at, event, tool, detail, result) VALUES(?,?,?,?,?)`,
		at.Unix(), event, tool, detail, result)
	if err != nil {
		return fmt.Errorf("append audit mirror: %w", err)
	}
	return nil
}
