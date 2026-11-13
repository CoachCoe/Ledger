package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// writeout starts write-back without waiting, as Git's batch mode does on
// Linux; the barrier's syncfs waits for it.
func writeout(f *os.File) error {
	return unix.SyncFileRange(int(f.Fd()), 0, 0, unix.SYNC_FILE_RANGE_WRITE)
}

func barrier(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Syncfs(int(f.Fd()))
}
