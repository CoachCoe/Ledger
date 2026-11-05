// Command observerbench measures how quickly the daemon's workspace
// observer reports plain file edits, through the public Unix-socket API.
//
//	observerbench [-seconds 60] LEDGER_BINARY REPOSITORY
//
// REPOSITORY must be a disposable Git repository with a commit, no Ledger
// state, and an installed, ignored node_modules directory, which is copied
// into each of ten workspaces to reproduce dependency load. Results are
// printed and written to REPOSITORY/.ledger/observer-benchmark.json.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	workspaces = 10
	limit      = 2 * time.Second
)

func main() {
	seconds := flag.Int("seconds", 60, "how long to make edits")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: observerbench [-seconds N] LEDGER_BINARY REPOSITORY")
		os.Exit(2)
	}
	passed, err := run(flag.Arg(0), flag.Arg(1), time.Duration(*seconds)*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "observerbench:", err)
		os.Exit(1)
	}
	if !passed {
		os.Exit(1)
	}
}

type bench struct {
	ledger, repo string
	env          []string
	http         *http.Client
}

func run(ledger, repo string, duration time.Duration) (passed bool, err error) {
	if ledger, err = filepath.Abs(ledger); err != nil {
		return false, err
	}
	if repo, err = filepath.Abs(repo); err != nil {
		return false, err
	}
	if info, err := os.Stat(filepath.Join(repo, "node_modules")); err != nil || !info.IsDir() {
		return false, errors.New("install node_modules in the disposable repository first")
	}
	if _, err := os.Stat(filepath.Join(repo, ".ledger")); err == nil {
		return false, errors.New("use a disposable repository without Ledger state")
	}
	config, err := os.MkdirTemp("", "ledger-bench-keys-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(config)
	socket := filepath.Join(repo, ".ledger", "runtime", "ledgerd.sock")
	b := &bench{
		ledger: ledger,
		repo:   repo,
		env:    append(os.Environ(), "XDG_CONFIG_HOME="+config),
		http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		}},
	}
	if err := b.cli("init"); err != nil {
		return false, err
	}
	if err := b.cli("system", "status"); err != nil {
		return false, err
	}
	defer func() {
		if stopErr := b.cli("daemon", "stop"); err == nil {
			err = stopErr
		}
	}()
	paths, err := b.setup()
	if err != nil {
		return false, err
	}
	result, err := b.measure(paths, duration)
	if err != nil {
		return false, err
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return false, err
	}
	fmt.Println(string(out))
	return result.Passed, os.WriteFile(filepath.Join(repo, ".ledger", "observer-benchmark.json"), append(out, '\n'), 0o600)
}

func (b *bench) cli(args ...string) error {
	cmd := exec.Command(b.ledger, args...) //nolint:gosec // runs the Ledger binary under test
	cmd.Dir = b.repo
	cmd.Env = b.env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ledger %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}

func (b *bench) rpc(operation, session string, input any) (map[string]any, error) {
	opID := ""
	if !strings.HasSuffix(operation, ".status") {
		random := make([]byte, 16)
		_, _ = rand.Read(random)
		opID = "op_" + strings.ToLower(base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(random))
	}
	body, err := json.Marshal(map[string]any{"api_version": 1, "operation": operation, "operation_id": opID, "session_id": session, "input": input})
	if err != nil {
		return nil, err
	}
	response, err := b.http.Post("http://ledger/v1", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	var reply struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
		return nil, err
	}
	if !reply.OK {
		return nil, fmt.Errorf("%s: %v", operation, reply.Error)
	}
	if nested, ok := reply.Result["result"].(map[string]any); ok {
		return nested, nil
	}
	return reply.Result, nil
}

// setup forks the workspaces, copies the dependency tree into each, and
// warms every private index once the copy events settle.
func (b *bench) setup() (map[string]string, error) {
	intent, err := b.rpc("intent.create", "", map[string]any{"description": "Real-project watcher benchmark", "criteria": []any{
		map[string]any{"id": "review", "statement": "Measure observation latency", "verification": map[string]any{"kind": "judgment", "reviewer": "any"}},
	}})
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	for i := range workspaces {
		session, err := b.rpc("session.create", "", map[string]any{"agent_name": fmt.Sprintf("bench-%d", i)})
		if err != nil {
			return nil, err
		}
		ws, err := b.rpc("workspace.fork", session["session_id"].(string), map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]})
		if err != nil {
			return nil, err
		}
		copyArgs := []string{"-R"}
		if runtime.GOOS == "darwin" {
			copyArgs = []string{"-c", "-R"}
		}
		path := ws["path"].(string)
		if out, err := exec.Command("cp", append(copyArgs, filepath.Join(b.repo, "node_modules"), path)...).CombinedOutput(); err != nil { //nolint:gosec // fixed tool, harness-owned paths
			return nil, fmt.Errorf("copy dependencies: %w: %s", err, out)
		}
		paths[ws["workspace_id"].(string)] = path
	}
	time.Sleep(3 * time.Second)
	for id := range paths {
		if _, err := b.rpc("workspace.status", "", map[string]any{"workspace_id": id}); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

type result struct {
	Platform        string   `json:"platform"`
	CPUCount        int      `json:"cpu_count"`
	SourceCommit    string   `json:"source_commit"`
	Workspaces      int      `json:"workspaces"`
	DurationSeconds int      `json:"duration_seconds"`
	Samples         int      `json:"samples"`
	P50             *float64 `json:"p50_ms"`
	P95             *float64 `json:"p95_ms"`
	Max             *float64 `json:"max_ms"`
	Undetected      int      `json:"undetected"`
	Errors          []string `json:"errors"`
	Passed          bool     `json:"passed"`
	latencies       []float64
}

// measure edits each workspace every one to three seconds while a reader
// per workspace polls cached status every 100 ms without a session id, so
// reads never trigger observation themselves. Latency runs from the write
// to the first status that lists the new file.
func (b *bench) measure(paths map[string]string, duration time.Duration) (*result, error) {
	type key struct{ workspace, path string }
	var (
		mu      sync.Mutex
		pending = map[key]time.Time{}
		r       = &result{Platform: runtime.GOOS, CPUCount: runtime.NumCPU(), Workspaces: workspaces, DurationSeconds: int(duration / time.Second), Errors: []string{}}
		wg      sync.WaitGroup
	)
	deadline := time.Now().Add(duration)
	stop := make(chan struct{})
	n := 0
	for id, path := range paths {
		rng := mathrand.New(mathrand.NewPCG(uint64(n), 0)) //nolint:gosec // edit timing, not security
		wg.Go(func() {
			for counter := 0; ; counter++ {
				select {
				case <-stop:
					return
				case <-time.After(time.Second + time.Duration(rng.Int64N(int64(2*time.Second)))):
				}
				if time.Now().After(deadline) {
					return
				}
				name := fmt.Sprintf("ledger-bench-%d-%d.ts", n, counter)
				mu.Lock()
				err := os.WriteFile(filepath.Join(path, name), fmt.Appendf(nil, "export const x = %d;\n", counter), 0o600)
				if err == nil {
					pending[key{id, name}] = time.Now()
				} else {
					r.Errors = append(r.Errors, err.Error())
				}
				mu.Unlock()
			}
		})
		wg.Go(func() {
			for {
				status, err := b.rpc("workspace.status", "", map[string]any{"workspace_id": id})
				mu.Lock()
				if err != nil {
					r.Errors = append(r.Errors, err.Error())
					mu.Unlock()
					return
				}
				seen := time.Now()
				for _, p := range status["write_set"].([]any) {
					k := key{id, p.(string)}
					if at, ok := pending[k]; ok {
						r.latencies = append(r.latencies, float64(seen.Sub(at).Microseconds())/1000)
						delete(pending, k)
					}
				}
				mu.Unlock()
				select {
				case <-stop:
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
		n++
	}
	// Keep reading through one more debounce window rather than excluding
	// late edits.
	time.Sleep(time.Until(deadline) + 3*time.Second)
	close(stop)
	wg.Wait()
	head, err := exec.Command("git", "-C", b.repo, "rev-parse", "HEAD").Output() //nolint:gosec // fixed tool, harness-owned path
	if err != nil {
		return nil, err
	}
	r.SourceCommit = strings.TrimSpace(string(head))
	slices.Sort(r.latencies)
	r.Samples, r.Undetected = len(r.latencies), len(pending)
	if r.Samples > 0 {
		r.P50, r.P95, r.Max = percentile(r.latencies, 0.5), percentile(r.latencies, 0.95), percentile(r.latencies, 1)
	}
	r.Passed = r.Samples > 0 && r.Undetected == 0 && len(r.Errors) == 0 && *r.Max <= float64(limit/time.Millisecond)
	return r, nil
}

func percentile(sorted []float64, q float64) *float64 {
	v := sorted[min(len(sorted)-1, int(q*float64(len(sorted))))]
	v = float64(int(v*100+0.5)) / 100
	return &v
}
