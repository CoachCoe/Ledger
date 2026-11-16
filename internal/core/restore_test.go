package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
)

// logEntries returns workspace.log's entries for id.
func logEntries(t *testing.T, s *Service, id string) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, e := range call(t, s, request("workspace.log", "", map[string]any{"workspace_id": id}))["entries"].([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

// P2-AC-012: restore returns an open workspace to an earlier entry's exact
// tree as a new entry, and the log keeps every entry after it.
func TestRestoreAndLog(t *testing.T) {
	s, base := nativeFixture(t)
	ctx := context.Background()
	intent := call(t, s, request("intent.create", "", judgment("undo")))["intent_id"].(string)
	sess := call(t, s, request("session.create", "", map[string]any{}))
	sid := sess["session_id"].(string)
	fork := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": intent, "capability_id": sess["capability_id"]}))
	wid, dir := fork["workspace_id"].(string), fork["path"].(string)
	edit := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		call(t, s, request("workspace.status", sid, map[string]any{"workspace_id": wid}))
	}
	edit("export function x() { return 2 }\n")
	edit("export function x() { return 3 }\nexport const y = 1;\n")

	entries := logEntries(t, s, wid)
	if len(entries) != 3 || entries[0]["tree"] != base || entries[0]["kind"] != "snapshot" || len(entries[0]["files"].([]any)) != 0 {
		t.Fatalf("log %v", entries)
	}
	second := entries[1]
	if f := second["files"].([]any); len(f) != 1 || f[0].(map[string]any)["path"] != "a.ts" || f[0].(map[string]any)["status"] != "M" {
		t.Fatalf("second entry files %v", second["files"])
	}
	if w := entries[2]["symbols_written"].([]any); len(w) == 0 {
		t.Fatalf("third entry wrote no symbols: %v", entries[2])
	}

	r := request("workspace.restore", sid, map[string]any{"workspace_id": wid, "entry": second["hash"]})
	got := call(t, s, r)
	if got["source"] != second["hash"] || got["tree"] != second["tree"] || got["seq"] != float64(4) {
		t.Fatalf("restore %v", got)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "a.ts")); string(body) != "export function x() { return 2 }\n" {
		t.Fatalf("restored a.ts %q", body)
	}
	if again := call(t, s, r); again["entry"] != got["entry"] {
		t.Fatalf("retry %v, first %v", again, got)
	}
	entries = logEntries(t, s, wid)
	if len(entries) != 4 || entries[3]["kind"] != "restore" || entries[3]["source"] != second["hash"] || entries[3]["hash"] != got["entry"] {
		t.Fatalf("log after restore %v", entries)
	}
	if n := call(t, s, request("workspace.log", "", map[string]any{"workspace_id": wid, "limit": 1})); n["total"] != float64(4) || len(n["entries"].([]any)) != 1 {
		t.Fatalf("limited log %v", n)
	}

	r.Input, _ = canon.Marshal(map[string]any{"workspace_id": wid, "entry": entries[0]["hash"]})
	if _, err := s.Handle(ctx, r); !apierr.HasCode(err, "operation_conflict") {
		t.Fatalf("operation id reused for another entry: %v", err)
	}
	other := call(t, s, request("session.create", "", map[string]any{}))["session_id"].(string)
	if _, err := s.Handle(ctx, request("workspace.restore", other, map[string]any{"workspace_id": wid, "entry": entries[0]["hash"]})); !apierr.HasCode(err, "unauthorized") {
		t.Fatalf("restore by another session: %v", err)
	}
	if _, err := s.Handle(ctx, request("workspace.restore", sid, map[string]any{"workspace_id": wid, "entry": "e" + base[1:]})); !apierr.HasCode(err, "operation_unknown") {
		t.Fatalf("restore to an unknown entry: %v", err)
	}
	call(t, s, request("workspace.propose", sid, map[string]any{"workspace_id": wid}))
	if _, err := s.Handle(ctx, request("workspace.restore", sid, map[string]any{"workspace_id": wid, "entry": entries[0]["hash"]})); !apierr.HasCode(err, "invalid_state") {
		t.Fatalf("restore of a proposed workspace: %v", err)
	}
}

// Ledger writes a restore's files for the session, so a restore that would
// change a path outside its capability is refused and changes nothing.
func TestRestoreStaysInScope(t *testing.T) {
	s, _ := nativeFixture(t)
	ctx := context.Background()
	intent := call(t, s, request("intent.create", "", judgment("scoped")))["intent_id"].(string)
	sess := call(t, s, request("session.create", "", map[string]any{"allow": []string{"src/**"}}))
	sid := sess["session_id"].(string)
	fork := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": intent, "capability_id": sess["capability_id"]}))
	wid, dir := fork["workspace_id"].(string), fork["path"].(string)
	first := logEntries(t, s, wid)[0]["hash"]
	// Direct disk writes are observed, not prevented (docs/security-model.md).
	if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("export const x=9;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	call(t, s, request("workspace.status", sid, map[string]any{"workspace_id": wid}))
	_, err := s.Handle(ctx, request("workspace.restore", sid, map[string]any{"workspace_id": wid, "entry": first}))
	if !apierr.HasCode(err, "scope_violation") {
		t.Fatalf("out-of-scope restore: %v", err)
	}
	if paths := apierr.As(err).Details["paths"]; len(paths.([]string)) != 1 || paths.([]string)[0] != "a.ts" {
		t.Fatalf("denied paths %v", paths)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "a.ts")); string(body) != "export const x=9;\n" {
		t.Fatalf("refused restore changed a.ts to %q", body)
	}
	if n := len(logEntries(t, s, wid)); n != 2 {
		t.Fatalf("refused restore appended entries: %d", n)
	}
}
