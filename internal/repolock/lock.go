// Package repolock implements the single-writer rule from §12: exactly one
// process mutates a given repository at a time. There is no daemon and no IPC,
// so a lock file is the whole concurrency design.
package repolock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ErrBusy is returned when another process holds the lock. Callers map it onto
// the REPO_BUSY code with the holder's identity.
var ErrBusy = errors.New("repository is locked by another process")

// Lock is an acquired per-repository lock.
type Lock struct {
	path string
	f    *os.File
}

// Holder describes who holds a lock, best effort.
type Holder struct {
	PID    int    `json:"pid"`
	Since  string `json:"since,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Acquire takes the lock for repoID under stateDir/locks. It returns a Lock or
// an error wrapping ErrBusy when another process holds it.
func Acquire(stateDir, repoID string) (*Lock, error) {
	dir := filepath.Join(stateDir, "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	path := filepath.Join(dir, repoID+".lock")

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errWouldBlock) {
			return nil, fmt.Errorf("%w: %s held by pid %s", ErrBusy, repoID, readHolder(f))
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	// Record the holder for status output, then truncate before writing so the
	// file never accumulates stale bytes.
	if err := f.Truncate(0); err == nil {
		if _, err := f.Seek(0, 0); err == nil {
			_, _ = fmt.Fprintf(f, `{"pid":%d,"since":%q}`, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			_ = f.Sync()
		}
	}
	return &Lock{path: path, f: f}, nil
}

// Release unlocks and closes. It is safe to call on a nil Lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockFile(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// Inspect reports who currently holds a repository lock, without taking it.
func Inspect(stateDir, repoID string) (Holder, bool) {
	path := filepath.Join(stateDir, "locks", repoID+".lock")
	f, err := os.Open(path)
	if err != nil {
		return Holder{}, false
	}
	defer f.Close()

	if err := lockFile(f); err == nil {
		// Nobody holds it.
		_ = unlockFile(f)
		return Holder{}, false
	}
	return Holder{PID: parseHolderPID(f), Detail: "locked"}, true
}

func readHolder(f *os.File) string {
	pid := parseHolderPID(f)
	if pid == 0 {
		return "unknown"
	}
	return strconv.Itoa(pid)
}

func parseHolderPID(f *os.File) int {
	if f == nil {
		return 0
	}
	buf := make([]byte, 256)
	n, _ := f.ReadAt(buf, 0)
	if n == 0 {
		return 0
	}
	var h struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(buf[:n], &h); err != nil {
		return 0
	}
	return h.PID
}
