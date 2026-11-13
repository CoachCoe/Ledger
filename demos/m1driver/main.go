// Command m1driver exercises the public CLI in a native repository; it
// imports no Ledger internals and runs no Git.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return errors.New("usage: m1driver <ledger-binary>")
	}
	binary, err := filepath.Abs(os.Args[1])
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("/tmp", "ledger-m1-demo-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	repo := filepath.Join(work, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		return err
	}
	env := append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(work, "config"))
	command := func(dir, exe string, args ...string) ([]byte, error) {
		c := exec.Command(exe, args...) //nolint:gosec // demo invokes the supplied Ledger binary and fixed OS tools
		c.Dir = dir
		c.Env = env
		b, err := c.CombinedOutput()
		if err != nil {
			return b, fmt.Errorf("%s: %w: %s", exe, err, b)
		}
		return b, nil
	}
	ledger := func(args ...string) (map[string]any, error) {
		b, err := command(repo, binary, args...)
		if err != nil {
			return nil, err
		}
		var reply struct {
			OK     bool           `json:"ok"`
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(b, &reply); err != nil {
			return nil, err
		}
		if !reply.OK {
			return nil, fmt.Errorf("%s", b)
		}
		if result, ok := reply.Result["result"].(map[string]any); ok {
			return result, nil
		}
		return reply.Result, nil
	}
	payload := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return string(b)
	}
	for path, source := range map[string]string{"a.ts": "export function total(x: number) { return x; }\n", "b.js": "export const x = 1;\n", "c.py": "def total(x):\n    return x\n"} {
		if err := os.WriteFile(filepath.Join(repo, path), []byte(source), 0600); err != nil {
			return err
		}
	}
	initialized, err := ledger("init")
	if err != nil {
		return err
	}
	session, err := ledger("session", "create", `{"agent_name":"provider-neutral-demo"}`)
	if err != nil {
		return err
	}
	defer func() { _, _ = ledger("daemon", "stop") }()
	sid := session["session_id"].(string)
	intent, err := ledger("intent", "create", `{"description":"Change a value","criteria":[{"id":"review","statement":"Review the proposal","verification":{"kind":"judgment","reviewer":"human"}}]}`)
	if err != nil {
		return err
	}
	input := payload(map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]})
	opid := "op_" + strings.Repeat("b", 26)
	fork, err := ledger("workspace", "fork", "--session", sid, "--operation-id", opid, input)
	if err != nil {
		return err
	}
	again, err := ledger("workspace", "fork", "--session", sid, "--operation-id", opid, input)
	if err != nil {
		return err
	}
	if fork["workspace_id"] != again["workspace_id"] {
		return fmt.Errorf("fork retry changed identity")
	}
	wid, path := fork["workspace_id"].(string), fork["path"].(string)
	ws := payload(map[string]any{"workspace_id": wid})
	if _, err := ledger("workspace", "status", ws); err != nil {
		return err
	}
	if _, err := command(path, "sh", "-c", "printf 'export const x = 2;\\n' > b.js"); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, err := ledger("workspace", "status", ws)
		if err != nil {
			return err
		}
		if files, ok := status["write_set"].([]any); ok && len(files) > 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("watcher missed 2-second target")
		}
		time.Sleep(25 * time.Millisecond)
	}
	fmt.Println("ok: detached workspace, exact fork retry, shell edit observed within two seconds")
	if fork["base_tree"] != initialized["tree"] {
		return fmt.Errorf("fork is not based on the imported tree")
	}
	var manifests []string
	for revision := range 3 {
		if revision > 0 {
			if _, err := ledger("workspace", "rework", "--session", sid, ws); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(path, "b.js"), []byte(fmt.Sprintf("export const x = %d;\n", revision+2)), 0600); err != nil {
				return err
			}
		}
		proposal, err := ledger("workspace", "propose", "--session", sid, ws)
		if err != nil {
			return err
		}
		manifest := proposal["manifest"].(string)
		binding, err := ledger("evidence", "resolve", payload(map[string]any{"manifest": manifest}))
		if err != nil {
			return err
		}
		if binding["base_tree"] != fork["base_tree"] {
			return fmt.Errorf("revision changed fixed base")
		}
		if binding["tree"] != proposal["tree"] {
			return fmt.Errorf("tree binding mismatch")
		}
		if slices.Contains(manifests, manifest) {
			return fmt.Errorf("revision reused a manifest")
		}
		manifests = append(manifests, manifest)
	}
	report, err := ledger("verify", "--strict")
	if err != nil {
		return err
	}
	verified := 0
	for _, m := range report["manifests"].([]any) {
		m := m.(map[string]any)
		components := m["components"].(map[string]any)
		if slices.Contains(manifests, m["hash"].(string)) && components["tree"] == "verified" && components["oplog"] == "verified" {
			verified++
		}
	}
	if verified != len(manifests) {
		return fmt.Errorf("%d of %d revisions verify their tree and operation log", verified, len(manifests))
	}
	fmt.Println("ok: three revisions keep the fixed base and verify their tree and operation log offline")
	if _, err := ledger("daemon", "stop"); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(path, "later.py"), []byte("x = 1\n"), 0600); err != nil {
		return err
	}
	if _, err := ledger("system", "status"); err != nil {
		return err
	}
	// A root read returns the cached observation, so this checks the
	// daemon re-observed on start rather than on this call.
	status, err := ledger("workspace", "status", ws)
	if err != nil {
		return err
	}
	if paths, _ := status["write_set"].([]any); !slices.Contains(paths, any("later.py")) {
		return fmt.Errorf("edit made while stopped not observed on restart: %v", paths)
	}
	if _, err := ledger("workspace", "rework", "--session", sid, ws); err == nil {
		return fmt.Errorf("lost session still signs")
	}
	recovery, err := ledger("session", "recover", ws)
	if err != nil {
		return err
	}
	replacement := recovery["session_id"].(string)
	if replacement == sid {
		return fmt.Errorf("session identity reused")
	}
	if _, err := ledger("workspace", "rework", "--session", replacement, ws); err != nil {
		return err
	}
	final, err := ledger("workspace", "propose", "--session", replacement, ws)
	if err != nil {
		return err
	}
	if _, err := os.ReadFile(filepath.Join(path, "later.py")); err != nil {
		return err
	}
	fmt.Println("ok: restart observes and preserves edits, ends lost-key authority, and explicitly rebinds a replacement session")
	// An edit after the last proposal is kept in the operation log, so
	// closing removes the checkout without losing it.
	if err := os.WriteFile(filepath.Join(path, "later.py"), []byte("x = 2\n"), 0600); err != nil {
		return err
	}
	if _, err := ledger("workspace", "close", "--session", replacement, payload(map[string]any{"workspace_id": wid, "reason": "demo_complete"})); err != nil {
		return err
	}
	status, err = ledger("workspace", "status", ws)
	if err != nil {
		return err
	}
	if status["state"] != "closed" {
		return fmt.Errorf("close was not committed")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("closed checkout %s was kept", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	last, err := lastLogEntry(filepath.Join(repo, ".ledger", "oplog", wid+".log"))
	if err != nil {
		return err
	}
	if last["kind"] != "snapshot" || last["tree"] == final["tree"] {
		return fmt.Errorf("the unproposed edit was not recorded at close: %v", last)
	}
	fmt.Println("ok: close is durable and records unproposed edits in the operation log before removing the checkout")
	fmt.Println("M1 demo passed.")
	return nil
}

// lastLogEntry decodes the final entry of an operation log.
func lastLogEntry(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	var e map[string]any
	return e, json.Unmarshal([]byte(lines[len(lines)-1]), &e)
}
