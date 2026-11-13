package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Poll as root, without --session: queue reads must not themselves refresh
// observations. Only the real daemon watchers/fallback poll can produce alerts.
func TestAC187_WatcherNotificationsWithinFiveSeconds(t *testing.T) {
	work, err := os.MkdirTemp("/tmp", "ldg-m4-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	binary := filepath.Join(work, "ledger")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ledger")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, tc := range []struct{ ext, api, changed, caller, called string }{
		{"js", "export function f(x) {return x;}\n", "export function f(x,y) {return x+y;}\n", "import {f} from './api.js'; export function g(){return 0;}\n", "import {f} from './api.js'; export function g(){return f(1);}\n"},
		{"ts", "export function f(x: number) {return x;}\n", "export function f(x: number,y: number) {return x+y;}\n", "import {f} from './api'; export function g(){return 0;}\n", "import {f} from './api'; export function g(){return f(1);}\n"},
		{"tsx", "export function f(x: number) {return x;}\n", "export function f(x: number,y: number) {return x+y;}\n", "import {f} from './api'; export function G(){return <span/>;}\n", "import {f} from './api'; export function G(){return <span>{f(1)}</span>;}\n"},
		{"py", "def f(x):\n return x\n", "def f(x,y):\n return x+y\n", "from api import f\ndef g():\n return 0\n", "from api import f\ndef g():\n return f(1)\n"},
	} {
		t.Run(tc.ext, func(t *testing.T) {
			repo := filepath.Join(work, tc.ext)
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			env := append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(work, "config-"+tc.ext))
			write := func(dir, p, src string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, p), []byte(src), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(repo, "api."+tc.ext, tc.api)
			write(repo, "caller."+tc.ext, tc.caller)
			run := func(args ...string) map[string]any {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, binary, args...)
				cmd.Dir = repo
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%v %s", err, out)
				}
				var response struct {
					Result map[string]any `json:"result"`
				}
				if err := json.Unmarshal(out, &response); err != nil {
					t.Fatal(err)
				}
				if nested, ok := response.Result["result"].(map[string]any); ok {
					return nested
				}
				return response.Result
			}
			jsonArg := func(v any) string {
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			run("init")
			t.Cleanup(func() { run("daemon", "stop") })
			sessions := []map[string]any{}
			workspaces := []map[string]any{}
			for range 2 {
				ses := run("session", "create", "{}")
				sessions = append(sessions, ses)
				intent := run("intent", "create", `{"description":"M4 watcher","criteria":[{"id":"r","statement":"review","verification":{"kind":"judgment","reviewer":"human"}}]}`)
				w := run("workspace", "fork", "--session", ses["session_id"].(string), jsonArg(map[string]any{"intent_id": intent["intent_id"], "capability_id": ses["capability_id"]}))
				workspaces = append(workspaces, w)
				run("coordination", "status", jsonArg(map[string]any{"workspace_id": w["workspace_id"]}))
			}
			write(workspaces[0]["path"].(string), "api."+tc.ext, tc.changed)
			write(workspaces[1]["path"].(string), "caller."+tc.ext, tc.called)
			start := time.Now()
			notified := map[string]bool{}
			for time.Since(start) < 5*time.Second {
				for _, ses := range sessions {
					sid := ses["session_id"].(string)
					if notified[sid] {
						continue
					}
					q := run("coordination", "notifications", jsonArg(map[string]any{"session_id": sid}))
					for _, raw := range q["notifications"].([]any) {
						n := raw.(map[string]any)
						if n["kind"] == "overlap" && n["overlap_kind"] == "write_read" {
							notified[sid] = true
						}
					}
				}
				if len(notified) == 2 {
					if time.Since(start) > 5*time.Second {
						t.Fatal("delivery exceeded deadline")
					}
					t.Logf("both sessions notified in %s", time.Since(start))
					return
				}
				time.Sleep(25 * time.Millisecond)
			}
			t.Fatalf("both sessions must be notified within five seconds; got %v", notified)
		})
	}
}
