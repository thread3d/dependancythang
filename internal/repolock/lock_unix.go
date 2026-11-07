//go:build unix

package repolock

import (
	"errors"
	"os"
	"syscall"
)

// errWouldBlock is the platform's "lock held elsewhere" signal.
var errWouldBlock = syscall.EWOULDBLOCK

// lockFile takes an exclusive, non-blocking advisory lock. The kernel releases
// it when the process dies, so stale locks are impossible and no pid-file
// reaping is needed.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errWouldBlock
	}
	return err
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
