package repolock_test

import (
	"errors"
	"testing"

	"github.com/thread3d/dependancythang/internal/repolock"
)

func TestAcquireIsExclusive(t *testing.T) {
	dir := t.TempDir()

	first, err := repolock.Acquire(dir, "abc123")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()

	second, err := repolock.Acquire(dir, "abc123")
	if err == nil {
		second.Release()
		t.Fatal("second acquire succeeded; the single-writer rule is broken")
	}
	if !errors.Is(err, repolock.ErrBusy) {
		t.Errorf("error = %v, want ErrBusy so callers can return REPO_BUSY", err)
	}
}

func TestReleaseAllowsReacquire(t *testing.T) {
	dir := t.TempDir()

	l, err := repolock.Acquire(dir, "abc123")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	again, err := repolock.Acquire(dir, "abc123")
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	defer again.Release()
}

func TestDifferentRepositoriesDoNotConflict(t *testing.T) {
	dir := t.TempDir()
	a, err := repolock.Acquire(dir, "repo-a")
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer a.Release()

	b, err := repolock.Acquire(dir, "repo-b")
	if err != nil {
		t.Fatalf("acquire b should not conflict with a: %v", err)
	}
	defer b.Release()
}

func TestInspectReportsHolder(t *testing.T) {
	dir := t.TempDir()

	if _, held := repolock.Inspect(dir, "abc123"); held {
		t.Error("nothing is held yet, Inspect should say free")
	}

	l, err := repolock.Acquire(dir, "abc123")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer l.Release()

	h, held := repolock.Inspect(dir, "abc123")
	if !held {
		t.Fatal("Inspect did not notice a held lock")
	}
	if h.PID == 0 {
		t.Error("Inspect returned no pid for the holder")
	}
}

func TestReleaseIsNilSafe(t *testing.T) {
	var l *repolock.Lock
	if err := l.Release(); err != nil {
		t.Errorf("nil Release returned %v", err)
	}
}
