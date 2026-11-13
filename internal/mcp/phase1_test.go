package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/mockagent"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// TestAC204_Phase1 drives the nine engineering acceptance assertions against
// real stdio processes and their auto-started daemon, in a native
// repository. Root acts through the CLI; each agent session has its own
// session-bound MCP server. The sole deliberate API bypass is the
// unauthorized disk edit used to prove cooperative enforcement.
func TestAC204_Phase1(t *testing.T) {
	work, err := os.MkdirTemp("/tmp", "ldg-m6-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	repo := filepath.Join(work, "repo")
	binary := filepath.Join(work, "ledger")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/ledger")
	build.Dir = "../.."
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, b)
	}
	env := append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(work, "config"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(dir, name string, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v %s", name, args, err, b)
		}
		return b
	}
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const baseAPI = "export function f(x: number){return x;}\n"
	const baseCaller = "import {f} from './api'; export function use(){return 0;}\n"
	write(repo, "api.ts", baseAPI)
	write(repo, "caller.ts", baseCaller)
	write(repo, "feature.js", "export function feature(){return 0;}\n")
	write(repo, "helper.py", "def helper():\n return 0\n")
	run(repo, binary, "init")
	write(filepath.Join(repo, ".ledger"), "checks.json", `[{"name":"unit","category":"mechanical","setup":null,"command":"test -f api.ts && test -f feature.js && test -f helper.py","working_directory":".","env_allowlist":[],"timeout_seconds":10,"output_limit_bytes":1024}]`)
	l := mockagent.Ledger{Binary: binary, Dir: repo, Env: env}
	t.Cleanup(func() { cmd := exec.Command(binary, "daemon", "stop"); cmd.Dir = repo; cmd.Env = env; _ = cmd.Run() })
	agents := map[string]mockagent.Client{}
	t.Cleanup(func() {
		for _, c := range agents {
			_ = c.Session.Close()
		}
	})
	agent := func(sid string) mockagent.Client {
		t.Helper()
		if c, ok := agents[sid]; ok {
			return c
		}
		c, err := l.Connect(ctx, sid)
		if err != nil {
			t.Fatalf("connect %s: %v", sid, err)
		}
		agents[sid] = c
		return c
	}
	call := func(op, sid string, input any) map[string]any {
		t.Helper()
		out, err := agent(sid).Call(ctx, op, input)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return out
	}
	root := func(op string, input any) map[string]any {
		t.Helper()
		out, err := l.Root(ctx, op, input)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return out
	}
	intent := func(description string) map[string]any {
		return root("intent.create", map[string]any{"description": description, "criteria": []any{map[string]any{"id": "unit", "statement": "fixture check passes", "verification": map[string]any{"kind": "check", "check": "unit"}}}})
	}
	var a, b, wa, wb map[string]any
	t.Run("AC205_root_scoped_sessions", func(t *testing.T) {
		a = root("session.create", map[string]any{"agent_name": "A", "allow": []string{"caller.ts"}})
		b = root("session.create", map[string]any{"agent_name": "B", "allow": []string{"api.ts"}})
		cs := agent(a["session_id"].(string)).Session
		args := map[string]any{"operation_id": schema.NewID(schema.PrefixOperation), "input": map[string]any{"description": "stable retry", "criteria": []any{map[string]any{"id": "review", "statement": "review", "verification": map[string]any{"kind": "judgment", "reviewer": "human"}}}}}
		var firstID any
		for j := range 2 {
			result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "intent.create", Arguments: args})
			if err != nil || result.IsError {
				t.Fatalf("retry: %v %v", result, err)
			}
			raw, _ := json.Marshal(result.StructuredContent)
			var wire map[string]any
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			outer := wire["result"].(map[string]any)
			id := outer["result"].(map[string]any)["intent_id"]
			if j == 0 {
				firstID = id
			} else if id != firstID || outer["replayed"] != true {
				t.Fatal("MCP retry duplicated intent", wire)
			}
		}
		for _, pair := range []struct {
			session map[string]any
			target  *map[string]any
		}{{a, &wa}, {b, &wb}} {
			i := intent("Concurrent function change")
			*pair.target = call("workspace.fork", pair.session["session_id"].(string), map[string]any{"intent_id": i["intent_id"], "capability_id": pair.session["capability_id"]})
		}
		if a["capability_id"] == b["capability_id"] {
			t.Fatal("scope identities shared")
		}
	})
	if t.Failed() {
		return
	}
	t.Run("agent_cannot_sign_as_root", func(t *testing.T) {
		unbound := exec.CommandContext(ctx, binary, "mcp")
		unbound.Dir = repo
		unbound.Env = env
		if out, err := unbound.CombinedOutput(); err == nil || !strings.Contains(string(out), "--session") {
			t.Fatalf("unbound adapter started: %v %s", err, out)
		}
		cs := agent(a["session_id"].(string)).Session
		tools, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range tools.Tools {
			if strings.Contains(tool.Description, "root") {
				t.Fatalf("%s advertises root: %s", tool.Name, tool.Description)
			}
		}
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "session.create", Arguments: map[string]any{"session_id": "", "operation_id": schema.NewID(schema.PrefixOperation), "input": map[string]any{"agent_name": "escalated"}}})
		if err != nil || !r.IsError || !strings.Contains(r.Content[0].(*mcp.TextContent).Text, "schema_invalid") {
			t.Fatalf("empty session_id accepted: %v %v", r, err)
		}
		for op, input := range map[string]any{
			"session.create":    map[string]any{"agent_name": "escalated"},
			"capability.issue":  map[string]any{"session_id": a["session_id"], "allow": []string{"**"}, "deny": []string{}, "may_add_dependencies": true, "expires_at": "2099-01-01T00:00:00.000Z"},
			"speculation.start": map[string]any{"intent_id": "int_0123456789abcdefghijklmnop", "deadline_seconds": 60, "scorer": map[string]any{"kind": "submission"}, "strategies": []any{map[string]any{"name": "x", "spec": "x"}}},
			"system.reindex":    map[string]any{},
			"daemon.stop":       map[string]any{},
		} {
			if _, err := agent(a["session_id"].(string)).Call(ctx, op, input); !apierr.HasCode(err, "unauthorized") {
				t.Fatalf("%s as an agent: %v", op, err)
			}
		}
	})
	t.Run("AC206_disk_bypass_warned_and_proposal_rejected", func(t *testing.T) {
		write(wa["path"].(string), "api.ts", "unauthorized\n")
		q := call("coordination.notifications", a["session_id"].(string), map[string]any{"session_id": a["session_id"]})
		found := false
		for _, raw := range q["notifications"].([]any) {
			if raw.(map[string]any)["kind"] == "scope_violation" {
				found = true
			}
		}
		if !found {
			t.Fatal("no scope warning")
		}
		_, err := agent(a["session_id"].(string)).Call(ctx, "workspace.propose", map[string]any{"workspace_id": wa["workspace_id"]})
		if !apierr.HasCode(err, "scope_violation") {
			t.Fatal(err)
		}
		write(wa["path"].(string), "api.ts", baseAPI)
	})
	edit := func(w, s map[string]any, name, content string, previous *string) {
		t.Helper()
		var expected any
		if previous != nil {
			expected = canon.Hash([]byte(*previous))
		}
		call("workspace.write", s["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"], "path": name, "content": content, "expected_hash": expected})
	}
	t.Run("AC207_concurrent_call_and_signature_notify_both", func(t *testing.T) {
		edit(wa, a, "caller.ts", "import {f} from './api'; export function use(){return f(1);}\n", ptr(baseCaller))
		edit(wb, b, "api.ts", "export function f(x: number,y: number){return x+y;}\n", ptr(baseAPI))
		call("coordination.nearby", a["session_id"].(string), map[string]any{"path": "api.ts"})
		for _, s := range []map[string]any{a, b} {
			q := call("coordination.notifications", s["session_id"].(string), map[string]any{"session_id": s["session_id"]})
			found := false
			for _, raw := range q["notifications"].([]any) {
				n := raw.(map[string]any)
				if n["kind"] == "overlap" && n["overlap_kind"] == "write_read" {
					found = true
				}
			}
			if !found {
				t.Fatalf("session %s not warned: %v", s["session_id"], q)
			}
		}
	})
	var outcome map[string]any
	t.Run("AC208_three_attempts_one_selected_two_records", func(t *testing.T) {
		i := intent("Choose an implementation of f")
		h := root("speculation.start", map[string]any{"intent_id": i["intent_id"], "deadline_seconds": 600, "scorer": map[string]any{"kind": "submission"}, "strategies": []any{map[string]any{"name": "one", "spec": "simple f"}, map[string]any{"name": "two", "spec": "alternative f"}, map[string]any{"name": "three", "spec": "third f"}}})
		scores := map[string]int{}
		for j, raw := range h["attempts"].([]any) {
			attempt := raw.(map[string]any)
			edit(attempt, attempt, "api.ts", fmt.Sprintf("export function f(x: number){return x+%d;}\n", j+1), ptr(baseAPI))
			edit(attempt, attempt, "feature.js", fmt.Sprintf("export function feature(){return %d;}\n", j+1), ptr("export function feature(){return 0;}\n"))
			edit(attempt, attempt, "helper.py", fmt.Sprintf("def helper():\n return %d\n", j+1), ptr("def helper():\n return 0\n"))
			p := call("speculation.submit", attempt["session_id"].(string), map[string]any{"run_id": h["run_id"], "workspace_id": attempt["workspace_id"]})
			scores[p["manifest"].(string)] = j
		}
		root("speculation.evaluate", map[string]any{"run_id": h["run_id"]})
		outcome = root("speculation.score", map[string]any{"run_id": h["run_id"], "scores": scores})
		if outcome["accepted"] != false || outcome["merged"] != false || outcome["winner"] == nil {
			t.Fatal(outcome)
		}
		losers := 0
		for _, raw := range outcome["attempts"].([]any) {
			r := raw.(map[string]any)
			if r["outcome"] != "selected" {
				if len(r["attempt_record"].(string)) != 64 {
					t.Fatal(r)
				}
				losers++
			}
		}
		if losers != 2 {
			t.Fatal(outcome)
		}
	})
	if t.Failed() {
		return
	}
	winner := outcome["winner"].(map[string]any)
	t.Run("AC209_independent_clean_evidence", func(t *testing.T) {
		// The ordinary evidence lifecycle is independently exercised by the
		// external mock below; inspect the run's signed verifier facts here.
		dir := filepath.Join(repo, ".ledger", "objects")
		found := false
		tree := call("evidence.resolve", a["session_id"].(string), map[string]any{"manifest": winner["manifest"]})["tree"]
		if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			raw, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			var record schema.Envelope
			if json.Unmarshal(raw, &record) != nil {
				return nil
			}
			if record.Kind == schema.KindEvidenceAttestation {
				var att schema.EvidenceAttestation
				if err := schema.Decode(record.SchemaVersion, record.Kind, record.Payload, &att); err != nil {
					return err
				}
				if att.Manifest == winner["manifest"] && att.Result == "pass" {
					if record.SchemaVersion != 2 || att.Tree != tree || att.Isolation != "none" {
						t.Fatal(att)
					}
					found = true
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("winner has no independent evidence")
		}
		facts := call("evidence.isIndependent", a["session_id"].(string), map[string]any{"manifest": winner["manifest"]})
		for _, raw := range facts["attestations"].([]any) {
			if raw.(map[string]any)["independent"] != true {
				t.Fatal(facts)
			}
		}
	})
	t.Run("AC210_winner_retained_not_accepted", func(t *testing.T) {
		// The winner's manifest keeps its tree and operation log; selecting it
		// advances no line, so nothing is accepted.
		var out struct{ Result map[string]any }
		if err := json.Unmarshal(run(repo, binary, "verify", "--strict"), &out); err != nil {
			t.Fatal(err)
		}
		retained := false
		for _, raw := range out.Result["manifests"].([]any) {
			m := raw.(map[string]any)
			c := m["components"].(map[string]any)
			retained = retained || (m["hash"] == winner["manifest"] && c["tree"] == "verified" && c["oplog"] == "verified")
		}
		if !retained {
			t.Fatal("winner not retained", out.Result["manifests"])
		}
		if err := filepath.WalkDir(filepath.Join(repo, ".ledger", "objects"), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			raw, err := os.ReadFile(p)
			if err == nil && strings.Contains(string(raw), `"kind":"line_advance"`) {
				t.Fatalf("selection advanced a line: %s", p)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("AC211_MCP_history_and_live_nearby", func(t *testing.T) {
		sym := map[string]any{"path": "api.ts", "language": "typescript", "name": "f", "kind": "function", "disambiguator": nil}
		why := call("history.why", a["session_id"].(string), map[string]any{"symbol": sym})
		if len(why["records"].([]any)) != 3 {
			t.Fatal(why)
		}
		attempts := call("history.attempts", a["session_id"].(string), map[string]any{"symbol": sym})
		if len(attempts["records"].([]any)) != 2 {
			t.Fatal(attempts)
		}
		near := call("coordination.nearby", a["session_id"].(string), map[string]any{"symbol": sym})
		if near["unsigned"] != true || len(near["workspaces"].([]any)) < 2 {
			t.Fatal(near)
		}
		digest := near["snapshot_digest"]
		delete(near, "snapshot_digest")
		raw, _ := canon.Marshal(near)
		if canon.Hash(raw) != digest {
			t.Fatal("bad runtime digest")
		}
	})
	t.Run("AC212_228_external_mock_MCP_only_lifecycle", func(t *testing.T) {
		out, err := l.Lifecycle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(out["why"].(map[string]any)["records"].([]any)) != 1 {
			t.Fatal(out)
		}
	})
	bundle := filepath.Join(work, "phase1.ledger")
	t.Run("AC197_maintenance_and_bundles_are_root_only", func(t *testing.T) {
		for op, input := range map[string]any{
			"system.recover": map[string]any{},
			"bundle.export":  map[string]any{"out": bundle, "manifests": []string{}, "intents": []string{}, "allow_partial": false},
			"bundle.verify":  map[string]any{"bundle": bundle, "strict": true, "trust": []string{}},
		} {
			if _, err := agent(a["session_id"].(string)).Call(ctx, op, input); !apierr.HasCode(err, "unauthorized") {
				t.Fatalf("agent %s allowed: %v", op, err)
			}
		}
		for _, args := range [][]string{{"recover"}, {"reindex"}, {"export", "--out", bundle}, {"verify", "--bundle", bundle, "--strict"}, {"import", bundle}} {
			var out struct {
				OK     bool
				Result map[string]any
			}
			if err := json.Unmarshal(run(repo, binary, args...), &out); err != nil || !out.OK {
				t.Fatalf("%v: %v %v", args, out, err)
			}
		}
	})
	root("daemon.stop", map[string]any{})
	for sid, c := range agents {
		_ = c.Session.Close()
		delete(agents, sid)
	}
	t.Run("AC213_offline_bundle_without_source", func(t *testing.T) {
		if err := os.Rename(filepath.Join(repo, ".ledger"), filepath.Join(work, "unavailable-ledger")); err != nil {
			t.Fatal(err)
		}
		var out struct{ Result map[string]any }
		if err := json.Unmarshal(run(work, binary, "verify", "--bundle", bundle, "--strict"), &out); err != nil {
			t.Fatal(err)
		}
		if out.Result["integrity"] != "verified" || out.Result["trust"] != "untrusted" {
			t.Fatal(out)
		}
	})
}

func ptr(s string) *string { return &s }
