package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/lock"
)

func TestAcquireWaitsForStoppingDaemon(t *testing.T) {
	dir := t.TempDir()
	path, socket := filepath.Join(dir, "daemon.lock"), filepath.Join(dir, "d.sock")
	release, err := lock.Acquire(path, true)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(300*time.Millisecond, release)
	start := time.Now()
	unlock, err := acquire(context.Background(), path, socket)
	if err != nil {
		t.Fatalf("acquire after the holder released: %v", err)
	}
	unlock()
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("acquired in %v while the lock was held", elapsed)
	}
}

func TestAcquireRefusesWhileDaemonListens(t *testing.T) {
	// t.TempDir paths overflow the 104-byte limit on Unix socket names.
	dir, err := os.MkdirTemp("", "ld")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path, socket := filepath.Join(dir, "daemon.lock"), filepath.Join(dir, "d.sock")
	release, err := lock.Acquire(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	start := time.Now()
	_, err = acquire(context.Background(), path, socket)
	if e, ok := errors.AsType[*apierr.Error](err); !ok || e.Code != "already_running" {
		t.Fatalf("got %v, want already_running", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %v to refuse", elapsed)
	}
}
