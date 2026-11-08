// Package client connects CLI/MCP clients to the per-repository daemon.
package client

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/daemon"
	"github.com/CoachCoe/Ledger/internal/gitx"
)

// Call sends request to the repository's daemon, starting it if it is not
// running.
func Call(ctx context.Context, repo *gitx.Repo, request api.Request) (any, error) {
	raw, err := canon.Marshal(request)
	if err != nil {
		return nil, err
	}
	if _, err := api.Parse(raw); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(repo.MainDir, ".ledger")); err != nil {
		return nil, apierr.New("not_initialized", "run ledger init first")
	}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", daemon.Socket(repo))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	// once reports unreachable when the daemon is not listening, so the
	// caller can start it; every other failure is final.
	once := func() (result any, unreachable bool, err error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://ledger/v1", bytes.NewReader(raw))
		if err != nil {
			return nil, false, err
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, true, err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return nil, false, apierr.New("transport_error", "daemon returned HTTP %d", response.StatusCode)
		}
		var r struct {
			OK     bool          `json:"ok"`
			Result any           `json:"result"`
			Error  *apierr.Error `json:"error"`
		}
		if err := json.UnmarshalRead(response.Body, &r); err != nil {
			return nil, false, err
		}
		if !r.OK {
			if r.Error == nil {
				return nil, false, apierr.New("transport_error", "missing error envelope")
			}
			return nil, false, r.Error
		}
		return r.Result, false, nil
	}
	if result, unreachable, err := once(); !unreachable {
		return result, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(repo.MainDir, ".ledger", "runtime", "ledgerd.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	command := exec.Command(executable, "daemon") //nolint:gosec // re-exec this binary with fixed arguments
	command.Dir = repo.MainDir
	command.Stdout = log
	command.Stderr = log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	if err := command.Process.Release(); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var last error
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, apierr.New("daemon_unavailable", "daemon did not start: %v; see %s", last, logPath)
		case <-tick.C:
			result, unreachable, err := once()
			if !unreachable {
				return result, err
			}
			last = err
		}
	}
}
