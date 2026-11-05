// Package client connects CLI/MCP clients to the per-repository daemon.
package client

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
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
	once := func() (any, error, bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://ledger/v1", bytes.NewReader(raw))
		if err != nil {
			return nil, err, false
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, err, true
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return nil, apierr.New("transport_error", "daemon returned HTTP %d", response.StatusCode), false
		}
		var r struct {
			OK     bool          `json:"ok"`
			Result any           `json:"result"`
			Error  *apierr.Error `json:"error"`
		}
		if err := json.UnmarshalRead(response.Body, &r); err != nil {
			return nil, err, false
		}
		if !r.OK {
			if r.Error == nil {
				return nil, apierr.New("transport_error", "missing error envelope"), false
			}
			return nil, r.Error, false
		}
		return r.Result, nil, false
	}
	if result, err, connect := once(); !connect {
		return result, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(repo.MainDir, ".ledger", "runtime", "ledgerd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
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
			return nil, apierr.New("daemon_unavailable", "daemon did not start: %v; see %s", last, fmt.Sprintf("%s/.ledger/runtime/ledgerd.log", repo.MainDir))
		case <-tick.C:
			result, err, connect := once()
			if !connect {
				return result, err
			}
			last = err
		}
	}
}
