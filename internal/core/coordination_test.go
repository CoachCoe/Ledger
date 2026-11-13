package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func m4Service(t *testing.T, files map[string]string) *Service {
	t.Helper()
	s, _ := nativeFixtureWith(t, files)
	return s
}
func m4Session(t *testing.T, s *Service) map[string]any {
	t.Helper()
	return call(t, s, request("session.create", "", map[string]any{"agent_name": "M4"}))
}
func m4Edit(t *testing.T, w map[string]any, p, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w["path"].(string), p), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
}
func m4Warnings(t *testing.T, s *Service, sid, kind string) []map[string]any {
	t.Helper()
	q := call(t, s, request("coordination.notifications", sid, map[string]any{"session_id": sid}))
	out := []map[string]any{}
	for _, v := range q["notifications"].([]any) {
		n := v.(map[string]any)
		if n["kind"] == kind {
			out = append(out, n)
		}
	}
	return out
}

func TestAC183_184_187_207_AllLanguageOverlap(t *testing.T) {
	for _, tc := range []struct{ ext, base, changed, caller, called string }{
		{"js", "export function shared(x) { return x; }\n", "export function shared(x,y) { return x+y; }\n", "import {shared} from './api.js'; export function caller() { return 0; }", "import {shared} from './api.js'; export function caller() { return shared(1); }"},
		{"ts", "export function shared(x: number) { return x; }\n", "export function shared(x: number,y: number) { return x+y; }\n", "import {shared} from './api'; export function caller() { return 0; }", "import {shared} from './api'; export function caller() { return shared(1); }"},
		{"tsx", "export function shared(x: number) { return x; }\n", "export function shared(x: number,y: number) { return x+y; }\n", "import {shared} from './api'; export function Caller() { return <div/>; }", "import {shared} from './api'; export function Caller() { return <div>{shared(1)}</div>; }"},
		{"py", "def shared(x):\n return x\n", "def shared(x,y):\n return x+y\n", "from api import shared\ndef caller():\n return 0\n", "from api import shared\ndef caller():\n return shared(1)\n"},
	} {
		t.Run(tc.ext, func(t *testing.T) {
			s := m4Service(t, map[string]string{"api." + tc.ext: tc.base, "caller." + tc.ext: tc.caller})
			a, b := m4Session(t, s), m4Session(t, s)
			wa, wb := m2Workspace(t, s, a), m2Workspace(t, s, b)
			m4Edit(t, wa, "api."+tc.ext, tc.changed)
			m4Edit(t, wb, "caller."+tc.ext, tc.called)
			if err := s.Observe(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, ses := range []map[string]any{a, b} {
				n := m4Warnings(t, s, ses["session_id"].(string), "overlap")
				if len(n) == 0 {
					t.Fatal("missing symmetric overlap")
				}
				found := false
				for _, v := range n {
					if v["overlap_kind"] == "write_read" && v["confidence"] == "static_inferred" {
						found = true
					}
				}
				if !found {
					t.Fatal(n)
				}
			}
			proposal := call(t, s, request("workspace.propose", a["session_id"].(string), map[string]any{"workspace_id": wa["workspace_id"], "context_refs": []string{"caller." + tc.ext + "#caller"}}))
			m, err := s.Store.Index.Manifest(context.Background(), proposal["manifest"].(string))
			if err != nil || len(m.SymbolsWritten) == 0 {
				t.Fatalf("frozen symbols: %+v %v", m, err)
			}
		})
	}
}

func TestAC117_187_UnrelatedFunctionsAndDeletedSymbols(t *testing.T) {
	s := m4Service(t, map[string]string{"api.js": "export function f() { return 1; }\nexport function g() { return 2; }\n"})
	a, b := m4Session(t, s), m4Session(t, s)
	wa, wb := m2Workspace(t, s, a), m2Workspace(t, s, b)
	m4Edit(t, wa, "api.js", "export function f() { return 3; }\nexport function g() { return 2; }\n")
	m4Edit(t, wb, "api.js", "export function f() { return 1; }\nexport function g() { return 4; }\n")
	if err := s.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := m4Warnings(t, s, a["session_id"].(string), "overlap"); len(n) != 0 {
		t.Fatal("unrelated functions overlap", n)
	}
	m4Edit(t, wa, "api.js", "export function g() { return 2; }\n")
	m4Edit(t, wb, "api.js", "export function f() { return 1; }\nexport function g() { return f(); }\n")
	if err := s.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := m4Warnings(t, s, a["session_id"].(string), "overlap"); len(n) == 0 {
		t.Fatal("deleted declaration not included")
	}
}

func TestAC029_065_186_LeaseRecovery(t *testing.T) {
	for _, stage := range []string{"", "lease_before", "lease_after", "lease_delete_before", "lease_delete_after"} {
		t.Run(stage, func(t *testing.T) {
			s := fixture(t)
			a, b := m4Session(t, s), m4Session(t, s)
			m2Workspace(t, s, a)
			wb := m2Workspace(t, s, b)
			sid := a["session_id"].(string)
			r := request("coordination.lease", sid, map[string]any{"session_id": sid, "paths": []string{"a.ts"}, "symbols": []any{}, "reason": "refactor", "expires_at": schema.Timestamp(time.Now().Add(time.Hour))})
			crash := errors.New("crash")
			fired := false
			s.Store.Index.NotificationFault = func(at string) error {
				if at == stage && !fired {
					fired = true
					return crash
				}
				return nil
			}
			if strings.HasPrefix(stage, "lease_") && !strings.Contains(stage, "delete") {
				if _, err := s.Handle(context.Background(), r); !errors.Is(err, crash) {
					t.Fatalf("fault: %v", err)
				}
			}
			l := call(t, s, r)["lease"].(map[string]any)
			again := call(t, s, r)["lease"].(map[string]any)
			if l["id"] != again["id"] {
				t.Fatal("retry created new lease")
			}
			m4Edit(t, wb, "a.ts", "export const x=2;\n")
			if err := s.Observe(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, ses := range []map[string]any{a, b} {
				if len(m4Warnings(t, s, ses["session_id"].(string), "lease_conflict")) == 0 {
					t.Fatal("missing lease warning")
				}
			}
			call(t, s, request("workspace.propose", b["session_id"].(string), map[string]any{"workspace_id": wb["workspace_id"]}))
			expectCode(t, s, "coordination.release", b["session_id"].(string), map[string]any{"session_id": b["session_id"], "lease_id": l["id"]}, "unauthorized")
			release := request("coordination.release", sid, map[string]any{"session_id": sid, "lease_id": l["id"]})
			if strings.Contains(stage, "delete") {
				if _, err := s.Handle(context.Background(), release); !errors.Is(err, crash) {
					t.Fatalf("delete fault: %v", err)
				}
			}
			call(t, s, release)
			call(t, s, release)
			if len(call(t, s, request("coordination.leases", "", map[string]any{}))["leases"].([]any)) != 0 {
				t.Fatal("release did not converge")
			}
			s.Store.Index.NotificationFault = nil
			call(t, s, request("coordination.lease", sid, map[string]any{"session_id": sid, "paths": []string{}, "symbols": []any{map[string]any{"path": "a.ts", "language": "typescript", "name": "x", "kind": "value", "disambiguator": nil}}, "reason": "symbol", "expires_at": schema.Timestamp(time.Now().Add(time.Hour))}))
			restarted, err := New(context.Background(), s.Store, s.Keys)
			if err != nil {
				t.Fatal(err)
			}
			leases, err := restarted.Store.Index.Leases(context.Background())
			if err != nil || len(leases) != 0 {
				t.Fatal("lost session revived leases", leases, err)
			}
		})
	}
}

func TestAC106_185_OverlapQueueRecovery(t *testing.T) {
	s := fixture(t)
	a, b := m4Session(t, s), m4Session(t, s)
	wa, wb := m2Workspace(t, s, a), m2Workspace(t, s, b)
	m4Edit(t, wa, "a.ts", "export const x=2;\n")
	m4Edit(t, wb, "a.ts", "export const x=3;\n")
	if err := s.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	sid := a["session_id"].(string)
	q := m4Warnings(t, s, sid, "overlap")
	if len(q) != 1 {
		t.Fatal(q)
	}
	call(t, s, request("coordination.ack", sid, map[string]any{"session_id": sid, "notification_ids": []string{q[0]["id"].(string)}}))
	if len(m4Warnings(t, s, sid, "overlap")) != 0 {
		t.Fatal("ack redelivered")
	}
	if err := s.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m4Warnings(t, s, sid, "overlap")) != 0 {
		t.Fatal("repeat observation redelivered")
	}
	report, err := s.Store.Reindex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.DiscardedRuntime["notifications"] == 0 {
		t.Fatal("missing delivery-gap report")
	}
	if err := s.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m4Warnings(t, s, sid, "overlap")) != 1 {
		t.Fatal("reindex failed to regenerate")
	}
	restarted, err := New(context.Background(), s.Store, s.Keys)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := restarted.Store.Index.Notifications(context.Background(), sid, "")
	if err != nil || len(persisted) != 1 {
		t.Fatal("ordinary restart lost queue", err)
	}
}

func TestAC186_LeaseExpiryAndScope(t *testing.T) {
	s := fixture(t)
	a := call(t, s, request("session.create", "", map[string]any{"allow": []string{"src/**"}}))
	sid := a["session_id"].(string)
	in := map[string]any{"session_id": sid, "paths": []string{"a.ts"}, "symbols": []any{}, "reason": "lease", "expires_at": schema.Timestamp(time.Now().Add(time.Hour))}
	_, err := s.Handle(context.Background(), request("coordination.lease", sid, in))
	if !apierr.HasCode(err, "scope_violation") {
		t.Fatal(err)
	}
	in["paths"] = []string{"src/a.ts"}
	in["expires_at"] = schema.Timestamp(time.Now().Add(500 * time.Millisecond))
	call(t, s, request("coordination.lease", sid, in))
	time.Sleep(600 * time.Millisecond)
	if err := s.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	l, err := s.Store.Index.Leases(context.Background())
	if err != nil || len(l) != 0 {
		t.Fatal(l, err)
	}
	in["expires_at"] = schema.Timestamp(time.Now().Add(time.Hour))
	call(t, s, request("coordination.lease", sid, in))
	call(t, s, request("session.end", sid, map[string]any{"session_id": sid}))
	if err := s.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	l, err = s.Store.Index.Leases(context.Background())
	if err != nil || len(l) != 0 {
		t.Fatal("ended session kept leases", l, err)
	}
	b := m4Session(t, s)
	in["session_id"] = b["session_id"]
	call(t, s, request("coordination.lease", b["session_id"].(string), in))
	report, err := s.Store.Reindex(context.Background())
	if err != nil || report.DiscardedRuntime["leases"] != 1 {
		t.Fatal("lease delivery gap missing", report, err)
	}
	l, err = s.Store.Index.Leases(context.Background())
	if err != nil || len(l) != 0 {
		t.Fatal("reindex revived a lease", l, err)
	}
}

func TestM4OverlapQueueFaults(t *testing.T) {
	for _, stage := range []string{"enqueue_before", "enqueue_after", "ack_before", "ack_after"} {
		t.Run(stage, func(t *testing.T) {
			s := fixture(t)
			a, b := m4Session(t, s), m4Session(t, s)
			wa, wb := m2Workspace(t, s, a), m2Workspace(t, s, b)
			m4Edit(t, wa, "a.ts", "export const x=2;\n")
			m4Edit(t, wb, "a.ts", "export const x=3;\n")
			crash := errors.New("overlap queue crash")
			fired := false
			s.Store.Index.NotificationFault = func(at string) error {
				if stage == at && !fired {
					fired = true
					return crash
				}
				return nil
			}
			if strings.HasPrefix(stage, "enqueue") {
				if err := s.Observe(context.Background()); !errors.Is(err, crash) {
					t.Fatal(err)
				}
			}
			if err := s.Observe(context.Background()); err != nil {
				t.Fatal(err)
			}
			sid := a["session_id"].(string)
			q := m4Warnings(t, s, sid, "overlap")
			if len(q) != 1 {
				t.Fatal("enqueue did not converge", q)
			}
			ack := request("coordination.ack", sid, map[string]any{"session_id": sid, "notification_ids": []string{q[0]["id"].(string)}})
			if strings.HasPrefix(stage, "ack") {
				if _, err := s.Handle(context.Background(), ack); !errors.Is(err, crash) {
					t.Fatal(err)
				}
			}
			call(t, s, ack)
			if len(m4Warnings(t, s, sid, "overlap")) != 0 {
				t.Fatal("ack did not converge")
			}
			s.Store.Index.NotificationFault = nil
			restarted, err := New(context.Background(), s.Store, s.Keys)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := restarted.Store.Index.Notifications(context.Background(), sid, "")
			if err != nil || len(stored) != 1 || stored[0].AcknowledgedAt == nil {
				t.Fatal("restart lost durable runtime ack", stored, err)
			}
		})
	}
}

func TestM4LeaseRestartWithoutRoot(t *testing.T) {
	s := fixture(t)
	a := m4Session(t, s)
	sid := a["session_id"].(string)
	call(t, s, request("coordination.lease", sid, map[string]any{"session_id": sid, "paths": []string{"a.ts"}, "symbols": []any{}, "reason": "restart", "expires_at": schema.Timestamp(time.Now().Add(time.Hour))}))
	keys := s.Keys.(identity.LocalKeys)
	if err := os.Remove(filepath.Join(keys.Dir, "root.key")); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(context.Background(), s.Store, s.Keys)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := restarted.Store.Index.Leases(context.Background())
	if err != nil || len(leases) != 0 {
		t.Fatal("missing root revived lost-key lease", leases, err)
	}
}

func TestAC053_055_064_182_238_FrozenSymbolsAndUncertainty(t *testing.T) {
	s := m4Service(t, map[string]string{"api.ts": "export function f(x: number) { return x; }\n", "caller.ts": "import { f as alias } from './api'; export function g() { return 0; }\n", "note.txt": "base\n"})
	a, b := m4Session(t, s), m4Session(t, s)
	wa, wb := m2Workspace(t, s, a), m2Workspace(t, s, b)
	m4Edit(t, wa, "caller.ts", "import { f as alias } from './api'; export function g() { return alias(1); }\n")
	proposal := call(t, s, request("workspace.propose", a["session_id"].(string), map[string]any{"workspace_id": wa["workspace_id"], "context_refs": []string{"a.ts#x"}}))
	m, err := s.Store.Index.Manifest(context.Background(), proposal["manifest"].(string))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, symbol := range m.SymbolsRead {
		seen[symbol.Path+"#"+symbol.Name] = true
	}
	if !seen["api.ts#f"] || !seen["a.ts#x"] {
		t.Fatal("static or declared context missing", m.SymbolsRead)
	}
	declared := call(t, s, request("coordination.status", "", map[string]any{"workspace_id": wa["workspace_id"]}))
	foundContext := false
	for _, raw := range declared["analysis"].(map[string]any)["read"].([]any) {
		sym := raw.(map[string]any)
		if sym["path"] == "a.ts" && sym["name"] == "x" {
			foundContext = true
		}
	}
	if !foundContext {
		t.Fatal("context-only proposal did not refresh live analysis")
	}
	call(t, s, request("workspace.rework", a["session_id"].(string), map[string]any{"workspace_id": wa["workspace_id"]}))
	m4Edit(t, wa, "caller.ts", "import { f as alias } from './api'; export function g(alias: any) { return alias(1); }\n")
	status := call(t, s, request("coordination.status", "", map[string]any{"workspace_id": wa["workspace_id"]}))
	analysis := status["analysis"].(map[string]any)
	for _, raw := range analysis["read"].([]any) {
		sym := raw.(map[string]any)
		if sym["path"] == "api.ts" {
			t.Fatal("shadowed parameter falsely resolved as import")
		}
	}
	if analysis["advisory"] != true || len(analysis["unresolved"].([]any)) == 0 || status["snapshot_digest"] == "" {
		t.Fatal("uncertainty/runtime citation missing", status)
	}
	for _, w := range []map[string]any{wa, wb} {
		m4Edit(t, w, "note.txt", "changed\n")
	}
	m4Edit(t, wa, "api.ts", "export function (")
	m4Edit(t, wb, "api.ts", "export function f(x: number,y: number) { return x+y; }\n")
	if err := s.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	warnings := m4Warnings(t, s, a["session_id"].(string), "overlap")
	parser, unsupported := false, false
	for _, n := range warnings {
		if n["confidence"] == "file_level" && n["reason"] == "parser_error" {
			parser = true
		}
		if n["confidence"] == "file_level" && n["reason"] == "unsupported_file" {
			unsupported = true
		}
	}
	if !parser || !unsupported {
		t.Fatal("labeled fallbacks missing", warnings)
	}
}

func TestAC117_RenameMoveAndAmbiguousMapping(t *testing.T) {
	s := m4Service(t, map[string]string{"api.js": "function f() { return 1; }\n"})
	a := m4Session(t, s)
	w := m2Workspace(t, s, a)
	m4Edit(t, w, "api.js", "function h() { return 1; }\n")
	status := call(t, s, request("coordination.status", "", map[string]any{"workspace_id": w["workspace_id"]}))
	seen := map[string]bool{}
	for _, raw := range status["analysis"].(map[string]any)["written"].([]any) {
		sym := raw.(map[string]any)
		seen[sym["path"].(string)+"#"+sym["name"].(string)] = true
	}
	if !seen["api.js#f"] || !seen["api.js#h"] {
		t.Fatal("rename was not delete/add", seen)
	}
	dir := w["path"].(string)
	if err := os.Rename(filepath.Join(dir, "api.js"), filepath.Join(dir, "moved.js")); err != nil {
		t.Fatal(err)
	}
	status = call(t, s, request("coordination.status", "", map[string]any{"workspace_id": w["workspace_id"]}))
	seen = map[string]bool{}
	for _, raw := range status["analysis"].(map[string]any)["written"].([]any) {
		sym := raw.(map[string]any)
		seen[sym["path"].(string)+"#"+sym["name"].(string)] = true
	}
	if !seen["api.js#f"] || !seen["moved.js#h"] {
		t.Fatal("move was not delete/add", seen)
	}
	m4Edit(t, w, "moved.js", "function h() {}\nfunction h() {}\n")
	status = call(t, s, request("coordination.status", "", map[string]any{"workspace_id": w["workspace_id"]}))
	found := false
	for _, raw := range status["analysis"].(map[string]any)["fallbacks"].([]any) {
		if raw.(map[string]any)["reason"] == "ambiguous_declaration" {
			found = true
		}
	}
	if !found {
		t.Fatal("ambiguous mapping was not labeled")
	}
}
