package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPublicCLILifecycle(t *testing.T) {
	// Keep the socket below macOS's sockaddr_un path limit.
	work, err := os.MkdirTemp("/tmp", "ledger-m1-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	binary := filepath.Join(work, "ledger")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ledger")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	repo := filepath.Join(work, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(work, "config"), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@invalid")
	run := func(args ...string) (map[string]any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = repo
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		var response struct {
			OK     bool           `json:"ok"`
			Result map[string]any `json:"result"`
		}
		if e := json.Unmarshal(out, &response); e != nil {
			return nil, e
		}
		if err != nil {
			return nil, &commandError{string(out)}
		}
		if nested, ok := response.Result["result"].(map[string]any); ok {
			return nested, nil
		}
		return response.Result, nil
	}
	must := func(args ...string) map[string]any {
		t.Helper()
		r, err := run(args...)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %v %s", err, out)
		}

	}
	git("init", "-q")
	os.WriteFile(filepath.Join(repo, "a.ts"), []byte("export const x=1;\n"), 0600)
	git("add", "a.ts")
	git("commit", "-qm", "base")
	must("init")
	// Simultaneous first clients must converge on one daemon.
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Go(func() { _, err := run("system", "status"); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = run("daemon", "stop") })
	if fi, err := os.Stat(filepath.Join(repo, ".ledger/runtime/ledgerd.sock")); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: %v %v", fi, err)
	}
	// A second foreground daemon fails without disturbing the first.
	if _, err := run("daemon"); err == nil {
		t.Fatal("second daemon accepted")
	}
	session := must("session", "create", `{"agent_name":"fixture"}`)
	sid := session["session_id"].(string)
	intent := must("intent", "create", `{"description":"test a change","criteria":[{"id":"review","statement":"review","verification":{"kind":"judgment","reviewer":"human"}}]}`)
	input, _ := json.Marshal(map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]})
	// Ten concurrent forks exercise inter-process transport and serialized worktree creation.
	results := make(chan map[string]any, 10)
	errs = make(chan error, 10)
	for range 10 {
		wg.Go(func() { r, err := run("workspace", "fork", "--session", sid, string(input)); results <- r; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var selected map[string]any
	seen := map[string]bool{}
	for r := range results {
		id := r["workspace_id"].(string)
		if seen[id] {
			t.Fatal("duplicate workspace")
		}
		seen[id] = true
		selected = r
	}
	path, wid := selected["path"].(string), selected["workspace_id"].(string)
	ws, _ := json.Marshal(map[string]any{"workspace_id": wid})
	must("workspace", "status", string(ws)) // establish a clean cached observation
	// A plain disk edit must reach the runtime observer without a session API call.
	edit := exec.Command("sh", "-c", "printf 'export const x=2;\n' > a.ts")
	edit.Dir = path
	if err := edit.Run(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := must("workspace", "status", string(ws))
		if paths, ok := status["write_set"].([]any); ok && len(paths) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("edit not observed within 2 seconds")
		}
		time.Sleep(30 * time.Millisecond)
	}
	proposal := must("workspace", "propose", "--session", sid, string(ws))
	if proposal["manifest"] == "" || proposal["commit"] == "" {
		t.Fatal("missing revision bindings")
	}
	// Restart ends the lost session and keeps files; explicit recovery replaces authority.
	runtimeStatus := must("system", "status")
	process, err := os.FindProcess(int(runtimeStatus["process_id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(path, "new.py"), []byte("x=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	must("system", "status")
	if _, err := run("workspace", "rework", "--session", sid, string(ws)); err == nil {
		t.Fatal("lost key still signs")
	}
	recovery := must("session", "recover", string(ws))
	newSID := recovery["session_id"].(string)
	if sid == newSID {
		t.Fatal("reused session identity")
	}
	must("workspace", "rework", "--session", newSID, string(ws))
	must("workspace", "propose", "--session", newSID, string(ws))
	if _, err := os.ReadFile(filepath.Join(path, "new.py")); err != nil {
		t.Fatal("recovery lost files")
	}

	// Poll-only mode uses the same cached observations without filesystem events.
	must("daemon", "stop")
	time.Sleep(100 * time.Millisecond)
	foreground := exec.Command(binary, "daemon", "--no-watch", "--poll", "200ms", "--idle", "200ms")
	foreground.Dir = repo
	foreground.Env = env
	log, err := os.Create(filepath.Join(work, "poll.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	foreground.Stdout = log
	foreground.Stderr = log
	if err := foreground.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- foreground.Wait() }()
	t.Cleanup(func() { _ = foreground.Process.Kill() })
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(filepath.Join(repo, ".ledger/runtime/ledgerd.sock")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll daemon did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	must("workspace", "status", string(ws))
	if err := os.WriteFile(filepath.Join(path, "poll.py"), []byte("x=2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); ; {
		status := must("workspace", "status", string(ws))
		paths, _ := status["write_set"].([]any)
		found := false
		for _, p := range paths {
			found = found || p == "poll.py"
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll-only observation missed edit")
		}
		time.Sleep(25 * time.Millisecond)
	}
	// Root may close lost-key workspaces; dirty cleanup stays pending and files survive.
	for id := range seen {
		input, _ := json.Marshal(map[string]string{"workspace_id": id, "reason": "test_complete"})
		_, err := run("workspace", "close", string(input))
		if id != wid && err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(path, "poll.py")); err != nil {
		t.Fatal("close removed dirty files")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not idle-shutdown after all workspaces closed")
	}

}

type commandError struct{ out string }

func (e *commandError) Error() string { return e.out }
