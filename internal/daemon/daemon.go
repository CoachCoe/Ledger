// Package daemon serves the core API over one private Unix socket per repository.
package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/core"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/lock"
	"github.com/CoachCoe/Ledger/internal/observe"
	"github.com/CoachCoe/Ledger/internal/record"
)

const maxRequest = 1 << 20

// Options configures the daemon. Zero durations select the defaults.
type Options struct {
	Idle            time.Duration
	Poll            time.Duration
	Debounce        time.Duration
	DisableWatchers bool
	Log             io.Writer
}

// Socket is the path of the daemon socket of the repository at root.
func Socket(root string) string {
	return filepath.Join(root, ".ledger", "runtime", "ledgerd.sock")
}

// acquire takes the daemon lock. A daemon closes its listener before it
// releases the lock, so a held lock with nothing answering on the socket
// belongs to one that is shutting down: wait for it rather than exit and leave
// the client that started us polling a socket nobody will open.
func acquire(ctx context.Context, path, socket string) (func(), error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		unlock, err := lock.Acquire(path, true)
		if err == nil {
			return unlock, nil
		}
		if e, ok := errors.AsType[*apierr.Error](err); !ok || e.Code != "already_running" || time.Now().After(deadline) {
			return nil, err
		}
		if conn, dialErr := net.DialTimeout("unix", socket, time.Second); dialErr == nil {
			_ = conn.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Run serves the API until ctx ends, a root-authorized daemon.stop
// arrives, or the daemon has been idle with no open workspace for
// opts.Idle. Only one daemon runs per repository.
func Run(ctx context.Context, repo record.Repository, keys identity.IdentityProvider, opts Options) error {
	if opts.Idle <= 0 {
		opts.Idle = 10 * time.Minute
	}
	if opts.Poll <= 0 {
		opts.Poll = 10 * time.Second
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 500 * time.Millisecond
	}
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	runtimeDir := filepath.Dir(Socket(repo.Root))
	if err := os.Chmod(runtimeDir, 0700); err != nil { //nolint:gosec // directories require owner execute permission
		return err
	}
	unlock, err := acquire(ctx, filepath.Join(runtimeDir, "daemon.lock"), Socket(repo.Root))
	if err != nil {
		return err
	}
	defer unlock()
	store, err := repo.Open(ctx, record.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	svc, err := core.New(ctx, store, keys)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(Socket(repo.Root)); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return apierr.New("socket_conflict", "refusing to replace a non-socket path")
		}
		if err := os.Remove(Socket(repo.Root)); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", Socket(repo.Root))
	if err != nil {
		return apierr.New("socket_unavailable", "%v", err)
	}
	defer func() { _ = listener.Close() }()
	defer os.Remove(Socket(repo.Root))
	if err := os.Chmod(Socket(repo.Root), 0600); err != nil {
		return err
	}
	kick := make(chan struct{}, 1)
	watchers := observe.New(opts.DisableWatchers, observe.RulesFilter(svc.CaseSensitive()), func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	})
	defer func() { _ = watchers.Close() }()
	syncWatchers := func() error {
		paths, err := svc.Workspaces(ctx)
		if err != nil {
			return err
		}
		return watchers.Sync(paths)
	}
	if err := syncWatchers(); err != nil {
		return err
	}
	if err := svc.Observe(ctx); err != nil {
		return err
	}
	var clients atomic.Int64
	var activity atomic.Int64
	activity.Store(time.Now().UnixNano())
	stopping := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activity.Store(time.Now().UnixNano())
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/v1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequest))
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			err = apierr.New("request_too_large", "requests are limited to %d bytes", maxRequest)
		}
		var result any
		var req api.Request
		if err == nil {
			req, err = api.Parse(raw)
			if err == nil {
				result, err = svc.Handle(r.Context(), req)
			}
		}
		// Only mutations can open or close workspaces.
		if err == nil && !api.ReadOnly(req.Operation) {
			if e := syncWatchers(); e != nil {
				err = apierr.New("watcher_unavailable", "%v", e).With("committed_result", result)
			}
		}
		b, e := json.Marshal(api.ReplyVersion(api.RequestVersion(raw), result, err))
		if e != nil {
			http.Error(w, "response encoding failed", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(append(b, '\n'))
		if err == nil && req.Operation == "daemon.stop" {
			select {
			case stopping <- struct{}{}:
			default:
			}
		}
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, ConnState: func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			clients.Add(1)
		case http.StateClosed, http.StateHijacked:
			clients.Add(-1)
		}
	}}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(stop)
		_ = server.Close()
	}()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	poll := time.NewTicker(opts.Poll)
	defer poll.Stop()
	idleInterval := opts.Idle / 2
	if idleInterval > time.Second {
		idleInterval = time.Second
	}
	if idleInterval < time.Millisecond {
		idleInterval = time.Millisecond
	}
	idle := time.NewTicker(idleInterval)
	defer idle.Stop()
	var timer *time.Timer
	var due <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	report := func(err error) {
		if err != nil {
			b, _ := json.Marshal(api.Reply(nil, err))
			fmt.Fprintln(opts.Log, string(b))
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-stopping:
			return nil
		case err := <-served:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case err := <-watchers.Errors:
			report(apierr.New("watcher_unavailable", "%v", err))
		case <-kick:
			if due == nil {
				timer = time.NewTimer(opts.Debounce)
				due = timer.C
			}
		case <-due:
			due = nil
			report(svc.Observe(ctx))
			report(syncWatchers())
		case <-poll.C:
			report(svc.Observe(ctx))
			report(syncWatchers())
		case <-idle.C:
			if clients.Load() == 0 && time.Since(time.Unix(0, activity.Load())) >= opts.Idle {
				paths, err := svc.Workspaces(ctx)
				if err != nil {
					return err
				}
				if len(paths) == 0 {
					return nil
				}
			}
		}
	}
}
