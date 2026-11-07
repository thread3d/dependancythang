package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Run statuses, matching the §12 transition table.
const (
	StatusPlanned   = "planned"
	StatusApplying  = "applying"
	StatusApplied   = "applied"
	StatusVerifying = "verifying"
	StatusVerified  = "verified"
	StatusPROpened  = "pr_opened"
	StatusMerged    = "merged"
	StatusClosed    = "closed"
	StatusFailed    = "failed"
	StatusAborted   = "aborted"
	StatusReverted  = "reverted"
)

// Run is one attempt to apply a plan. Every run is a branch (§3 row 13).
type Run struct {
	RunID     string `json:"run_id"`
	RepoID    string `json:"repo_id"`
	PlanID    string `json:"plan_id"`
	Target    string `json:"target,omitempty"`
	Branch    string `json:"branch"`
	Worktree  string `json:"worktree"`
	Status    string `json:"status"`
	Commit    string `json:"commit,omitempty"`
	Detail    string `json:"detail,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Terminal reports whether a status is final. Reverting a merged or closed run
// would try to delete a branch whose content already landed, so those are
// terminal (§12).
func Terminal(status string) bool {
	switch status {
	case StatusMerged, StatusClosed, StatusReverted:
		return true
	default:
		return false
	}
}

// Revertable reports whether revert_run may delete this run's branch.
func Revertable(status string) bool {
	switch status {
	case StatusApplied, StatusVerified, StatusPROpened, StatusFailed, StatusAborted, StatusPlanned:
		return true
	default:
		return false
	}
}

// ErrNoRun reports an unknown run id.
var ErrNoRun = errors.New("run not found")

// SaveRun inserts or updates a run record.
func (s *Store) SaveRun(ctx context.Context, run Run) error {
	now := time.Now().Unix()
	if run.CreatedAt == 0 {
		run.CreatedAt = now
	}
	run.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs(run_id, repo_id, plan_id, target, branch, worktree, status, commit_sha, detail, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET
			status=excluded.status, commit_sha=excluded.commit_sha, detail=excluded.detail,
			updated_at=excluded.updated_at`,
		run.RunID, run.RepoID, run.PlanID, run.Target, run.Branch, run.Worktree,
		run.Status, run.Commit, run.Detail, run.CreatedAt, run.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save run %s: %w", run.RunID, err)
	}
	return nil
}

// LoadRun returns one run by id.
func (s *Store) LoadRun(ctx context.Context, runID string) (Run, error) {
	var r Run
	var commit, detail, target sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT run_id, repo_id, plan_id, target, branch, worktree, status,
		       commit_sha, detail, created_at, updated_at
		FROM runs WHERE run_id=?`, runID).
		Scan(&r.RunID, &r.RepoID, &r.PlanID, &target, &r.Branch, &r.Worktree,
			&r.Status, &commit, &detail, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNoRun
	}
	if err != nil {
		return r, fmt.Errorf("load run %s: %w", runID, err)
	}
	r.Target, r.Commit, r.Detail = target.String, commit.String, detail.String
	return r, nil
}

// ListRuns returns recent runs, newest first, optionally filtered by repository.
func (s *Store) ListRuns(ctx context.Context, repoID string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 20
	}
	query := `SELECT run_id, repo_id, plan_id, target, branch, worktree, status,
	                 commit_sha, detail, created_at, updated_at FROM runs`
	args := []any{}
	if repoID != "" {
		query += ` WHERE repo_id=?`
		args = append(args, repoID)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var r Run
		var commit, detail, target sql.NullString
		if err := rows.Scan(&r.RunID, &r.RepoID, &r.PlanID, &target, &r.Branch, &r.Worktree,
			&r.Status, &commit, &detail, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		r.Target, r.Commit, r.Detail = target.String, commit.String, detail.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRunStatus updates a run's status and detail.
func (s *Store) SetRunStatus(ctx context.Context, runID, status, detail string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status=?, detail=?, updated_at=? WHERE run_id=?`,
		status, detail, time.Now().Unix(), runID)
	if err != nil {
		return fmt.Errorf("set run status %s: %w", runID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoRun
	}
	return nil
}
