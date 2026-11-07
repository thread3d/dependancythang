package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thread3d/dependancythang/internal/store"
)

func TestMigrateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s1, err := store.Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	stats, err := s2.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Repos != 0 {
		t.Errorf("repos = %d, want 0 on a fresh database", stats.Repos)
	}
}

func TestReconcileUpsertsAndOrphans(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	refs := []store.RepoRef{
		{ID: "aaa", Path: "/tmp/a", Hash: "h1"},
		{ID: "bbb", Path: "/tmp/b", Hash: "h2"},
	}
	if err := s.Reconcile(ctx, refs); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	states, err := s.States(ctx)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("states = %d, want 2", len(states))
	}

	// Dropping "bbb" from configuration must orphan it, not delete its history.
	if err := s.Reconcile(ctx, refs[:1]); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	states, err = s.States(ctx)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	byID := map[string]store.RepoState{}
	for _, st := range states {
		byID[st.RepoID] = st
	}
	if st, ok := byID["bbb"]; !ok {
		t.Error("orphaned repository row disappeared; history would be lost")
	} else if !st.Orphan {
		t.Error("bbb should be marked orphaned")
	}
	if st := byID["aaa"]; st.Orphan {
		t.Error("aaa should still be live")
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Simulate a database written by a future binary.
	if _, err := s.RawExec(`DELETE FROM schema_version`); err != nil {
		t.Fatalf("delete version: %v", err)
	}
	if _, err := s.RawExec(`INSERT INTO schema_version(version) VALUES(999)`); err != nil {
		t.Fatalf("stamp version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := store.Open(dir); err == nil {
		t.Fatal("expected refusal to open a newer schema")
	} else if !strings.Contains(err.Error(), "refusing to open") {
		t.Errorf("error = %v, want it to mention refusing to open", err)
	}
}

func TestAuditMirror(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if err := s.AppendAudit(ctx, time.Now(), "tool.call", "list_repositories", "{}", "ok"); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Audit != 1 {
		t.Errorf("audit events = %d, want 1", stats.Audit)
	}
}
