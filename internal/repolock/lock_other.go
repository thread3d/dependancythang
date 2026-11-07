//go:build !unix

package repolock

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// errWouldBlock is the platform's "lock held elsewhere" signal.
var errWouldBlock = errors.New("lock held")

// On platforms without flock, fall back to an O_EXCL sidecar whose staleness is
// judged by age. Windows is a v2 target (§3), so this path is a placeholder that
// keeps the package building and behaves conservatively: it never silently
// permits two writers within the stale window.
const staleAfter = 3600 // seconds

func lockFile(f *os.File) error {
	side := f.Name() + ".excl"
	h, err := os.OpenFile(side, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		_ = h.Close()
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	// Reclaim a lock whose owner is long gone.
	if info, statErr := os.Stat(side); statErr == nil {
		if age := time.Since(info.ModTime()).Seconds(); age > staleAfter {
			_ = os.Remove(side)
			if h, err := os.OpenFile(side, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
				_ = h.Close()
				return nil
			}
		}
	}
	return errWouldBlock
}

func unlockFile(f *os.File) error {
	if f == nil {
		return nil
	}
	return os.Remove(filepath.Clean(f.Name() + ".excl"))
}
