package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// writeout pushes f to the device without flushing the drive cache. Go's
// File.Sync is F_FULLFSYNC on darwin; plain fsync(2) is write-out only.
func writeout(f *os.File) error { return unix.Fsync(int(f.Fd())) }

// barrier flushes the drive cache once for every earlier write-out.
func barrier(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0)
	return err
}
