//go:build unix

// Package lock holds kernel-released advisory file locks.
package lock

import (
	"errors"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"os"
	"syscall"
)

func Acquire(path string, nonblocking bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	flags := syscall.LOCK_EX
	if nonblocking {
		flags |= syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), flags)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, apierr.New("already_running", "another process holds %s", path)
		}
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
