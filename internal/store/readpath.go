package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DependencyRow is one inventory row.
type DependencyRow struct {
	Name    string `json:"name"`
	Current string `json:"current"`
	Direct  bool   `json:"direct"`
}

// SaveInventory replaces the snapshot for one target. A snapshot is derived
// state: the next scan overwrites it, and deleting the database loses nothing
// that configuration cannot rebuild.
func (s *Store) SaveInventory(ctx context.Context, repoID, target string, deps []DependencyRow, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin inventory: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM dependencies WHERE repo_id=? AND target=?`, repoID, target); err != nil {
		return fmt.Errorf("clear inventory: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO dependencies(repo_id, target, name, current, direct, scanned_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(repo_id, target, name) DO UPDATE SET
			current=excluded.current, direct=excluded.direct, scanned_at=excluded.scanned_at`)
	if err != nil {
		return fmt.Errorf("prepare inventory: %w", err)
	}
	defer stmt.Close()

	for _, d := range deps {
		direct := 0
		if d.Direct {
			direct = 1
		}
		if _, err := stmt.ExecContext(ctx, repoID, target, d.Name, d.Current, direct, at.Unix()); err != nil {
			return fmt.Errorf("insert inventory row %s: %w", d.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit inventory: %w", err)
	}
	return nil
}

// LoadInventory returns the stored snapshot for a target.
func (s *Store) LoadInventory(ctx context.Context, repoID, target string) ([]DependencyRow, int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, current, direct, scanned_at FROM dependencies
		WHERE repo_id=? AND target=? ORDER BY direct DESC, name`, repoID, target)
	if err != nil {
		return nil, 0, fmt.Errorf("query inventory: %w", err)
	}
	defer rows.Close()

	var out []DependencyRow
	var scanned int64
	for rows.Next() {
		var d DependencyRow
		var direct int
		var at int64
		if err := rows.Scan(&d.Name, &d.Current, &direct, &at); err != nil {
			return nil, 0, fmt.Errorf("scan inventory row: %w", err)
		}
		d.Direct = direct != 0
		scanned = at
		out = append(out, d)
	}
	return out, scanned, rows.Err()
}

// SaveAuditless is unused; kept out deliberately.

// SavePlan persists an immutable plan.
func (s *Store) SavePlan(ctx context.Context, planID, repoID, target, ecosystem string, createdAt time.Time, planJSON []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO plans(plan_id, repo_id, target, ecosystem, created_at, plan_json)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(plan_id) DO NOTHING`,
		planID, repoID, target, ecosystem, createdAt.Unix(), string(planJSON))
	if err != nil {
		return fmt.Errorf("save plan %s: %w", planID, err)
	}
	return nil
}

// PlanRecord is a stored plan.
type PlanRecord struct {
	PlanID    string `json:"plan_id"`
	RepoID    string `json:"repo_id"`
	Target    string `json:"target"`
	Ecosystem string `json:"ecosystem"`
	CreatedAt int64  `json:"created_at"`
	JSON      string `json:"-"`
}

// LoadPlan returns a stored plan by id.
func (s *Store) LoadPlan(ctx context.Context, planID string) (PlanRecord, error) {
	var rec PlanRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT plan_id, repo_id, target, ecosystem, created_at, plan_json
		FROM plans WHERE plan_id=?`, planID).
		Scan(&rec.PlanID, &rec.RepoID, &rec.Target, &rec.Ecosystem, &rec.CreatedAt, &rec.JSON)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, ErrNoPlan
	}
	if err != nil {
		return rec, fmt.Errorf("load plan %s: %w", planID, err)
	}
	return rec, nil
}

// ErrNoPlan reports an unknown plan id.
var ErrNoPlan = errors.New("plan not found")

// ListPlans returns recent plans for a repository.
func (s *Store) ListPlans(ctx context.Context, repoID string, limit int) ([]PlanRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT plan_id, repo_id, target, ecosystem, created_at, plan_json
		FROM plans WHERE repo_id=? ORDER BY created_at DESC LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	defer rows.Close()

	var out []PlanRecord
	for rows.Next() {
		var rec PlanRecord
		if err := rows.Scan(&rec.PlanID, &rec.RepoID, &rec.Target, &rec.Ecosystem, &rec.CreatedAt, &rec.JSON); err != nil {
			return nil, fmt.Errorf("scan plan: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SaveAdvisories upserts advisory rows for a target.
func (s *Store) SaveAdvisories(ctx context.Context, repoID, target, pkg, version string, advisories []AdvisoryRow, at time.Time) error {
	for _, a := range advisories {
		payload, err := json.Marshal(a)
		if err != nil {
			return fmt.Errorf("encode advisory %s: %w", a.ID, err)
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO advisories(repo_id, target, package, version, id, severity, payload, seen_at)
			VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(repo_id, target, package, version, id) DO UPDATE SET
				severity=excluded.severity, payload=excluded.payload, seen_at=excluded.seen_at`,
			repoID, target, pkg, version, a.ID, string(a.Severity), string(payload), at.Unix()); err != nil {
			return fmt.Errorf("save advisory %s: %w", a.ID, err)
		}
	}
	return nil
}

// AdvisoryRow is a stored advisory.
type AdvisoryRow struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
}
