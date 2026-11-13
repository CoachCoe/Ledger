package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/evidence"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func writeChecks(t *testing.T, s *Service, defs []schema.CheckDefinition) {
	t.Helper()
	raw, err := canon.Marshal(defs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Store.Dir, "checks.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func unitCheck() schema.CheckDefinition {
	return schema.CheckDefinition{Name: "unit", Category: "mechanical", Command: "true", WorkingDirectory: ".", EnvAllowlist: []string{}, TimeoutSeconds: 10, OutputLimitBytes: 1024}
}

func checkIntent() map[string]any {
	return map[string]any{"description": "pinned check", "criteria": []any{
		map[string]any{"id": "tests", "statement": "unit passes", "verification": map[string]any{"kind": "check", "check": "unit"}},
		map[string]any{"id": "lint", "statement": "unit also covers lint", "verification": map[string]any{"kind": "check", "check": "unit"}},
		map[string]any{"id": "review", "statement": "looks good", "verification": map[string]any{"kind": "judgment", "reviewer": "human"}},
	}}
}

func TestAC046_InvalidDefinitionRejectedAtIntentCreate(t *testing.T) {
	// AC-046: a definition that breaks a field rule is never pinned.
	s := fixture(t)
	bad := unitCheck()
	bad.WorkingDirectory = "../escape"
	writeChecks(t, s, []schema.CheckDefinition{bad})
	expectCode(t, s, "intent.create", "", checkIntent(), "invalid_checks")
	bad = unitCheck()
	bad.OutputLimitBytes = 1 << 30
	writeChecks(t, s, []schema.CheckDefinition{bad})
	expectCode(t, s, "intent.create", "", checkIntent(), "invalid_checks")
	// An invalid definition no criterion references does not block others.
	writeChecks(t, s, []schema.CheckDefinition{unitCheck(), {Name: "broken", Category: "mechanical", Command: "", WorkingDirectory: ".", EnvAllowlist: []string{}, TimeoutSeconds: 1}})
	call(t, s, request("intent.create", "", checkIntent()))
}

func TestAC110_ResolveFreezesBinding(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	writeChecks(t, s, []schema.CheckDefinition{unitCheck()})
	session := call(t, s, request("session.create", "", map[string]any{"agent_name": "author"}))
	sid := session["session_id"].(string)
	intent := call(t, s, request("intent.create", "", checkIntent()))
	ws := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]}))
	wid, dir := ws["workspace_id"].(string), ws["path"].(string)

	// A change with no proposal yet has no binding to freeze.
	w, err := creation[schema.Workspace](ctx, s.Store.Index, wid, schema.KindWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	expectCode(t, s, "evidence.resolve", "", map[string]any{"change_id": w.ChangeID}, "not_proposed")
	expectCode(t, s, "evidence.resolve", "", map[string]any{"change_id": "chg_00000000000000000000000000"}, "unknown_entity")
	propose := func(content string) map[string]any {
		if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return call(t, s, request("workspace.propose", sid, map[string]any{"workspace_id": wid}))
	}
	first := propose("export const x=2;\n")
	change := first["change_id"].(string)

	frozen := call(t, s, request("evidence.resolve", "", map[string]any{"change_id": change}))
	if frozen["manifest"] != first["manifest"] || frozen["tree"] != first["tree"] || frozen["frozen"] != true {
		t.Fatalf("binding does not match the proposal: %v", frozen)
	}
	if frozen["change_id"] != change || frozen["intent_id"] != intent["intent_id"] || frozen["workspace_id"] != wid || frozen["author_session"] != sid {
		t.Fatalf("binding identity: %v", frozen)
	}
	created, err := creation[schema.Intent](ctx, s.Store.Index, intent["intent_id"].(string), schema.KindIntent)
	if err != nil {
		t.Fatal(err)
	}
	pinned := created.Criteria[0].Verification.DefinitionHash
	checks := frozen["checks"].([]any)
	if len(checks) != 1 {
		t.Fatalf("one definition serves both check criteria: %v", checks)
	}
	check := checks[0].(map[string]any)
	if check["definition_hash"] != pinned || check["name"] != "unit" || check["category"] != "mechanical" {
		t.Fatalf("check: %v", check)
	}
	if got := check["criteria"].([]any); len(got) != 2 || got[0] != "tests" || got[1] != "lint" {
		t.Fatalf("criteria: %v", got)
	}

	// An exact lookup agrees with the change-id convenience lookup.
	exact := call(t, s, request("evidence.resolve", "", map[string]any{"manifest": first["manifest"]}))
	a, _ := canon.Marshal(frozen)
	b, _ := canon.Marshal(exact)
	if string(a) != string(b) {
		t.Fatal("resolve by manifest differs from resolve by change id")
	}

	// Editing checks.json does not change the pinned definition.
	edited := unitCheck()
	edited.Command = "false"
	writeChecks(t, s, []schema.CheckDefinition{edited})
	if again := call(t, s, request("evidence.resolve", "", map[string]any{"change_id": change})); again["checks"].([]any)[0].(map[string]any)["definition"].(map[string]any)["command"] != "true" {
		t.Fatal("checks.json edit retargeted the pinned definition")
	}

	// Rework and re-propose: a new manifest; the earlier binding still names the old one.
	call(t, s, request("workspace.rework", sid, map[string]any{"workspace_id": wid}))
	second := propose("export const x=3;\n")
	if second["manifest"] == first["manifest"] || second["tree"] == first["tree"] {
		t.Fatal("rework did not create a new revision")
	}
	if now := call(t, s, request("evidence.resolve", "", map[string]any{"change_id": change})); now["manifest"] != second["manifest"] || now["tree"] != second["tree"] {
		t.Fatalf("change id should resolve to the current proposal: %v", now)
	}
	old := call(t, s, request("evidence.resolve", "", map[string]any{"manifest": first["manifest"]}))
	if old["manifest"] != first["manifest"] || old["tree"] != first["tree"] {
		t.Fatalf("earlier binding moved: %v", old)
	}

	// An unknown manifest, and the author's intent object itself, are not manifests.
	expectCode(t, s, "evidence.resolve", "", map[string]any{"manifest": "0000000000000000000000000000000000000000000000000000000000000000"}, "unknown_manifest")
	expectCode(t, s, "evidence.resolve", "", map[string]any{"manifest": intent["object_hash"]}, "unknown_manifest")

	// The input is closed.
	for _, in := range []map[string]any{
		{"change_id": change, "manifest": first["manifest"]},
		{"change_id": change, "commit": strings.Repeat("1", 40)},
		{"manifest": first["manifest"], "commit": strings.Repeat("1", 40)},
		{"change_id": change, "definition": "x"},
		{"manifest": first["manifest"], "extra": 1},
		{},
	} {
		if _, err := s.Handle(ctx, request("evidence.resolve", "", in)); !apierr.HasCode(err, "schema_invalid") {
			t.Fatalf("input %v: %v", in, err)
		}
	}
}

func TestVerificationSetupRecoveryReportedByStatus(t *testing.T) {
	// A crash that leaves a verification worktree and a non-terminal run
	// journal is reconciled by core.New and reported by system.status.
	s, tree := nativeFixture(t)
	ctx := context.Background()
	id := schema.NewID(schema.PrefixRun)
	path := evidence.VerifyPath(filepath.Join(s.Store.Dir, "runtime"), id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Provider.Workspaces.Checkout(tree, path); err != nil {
		t.Fatal(err)
	}
	j := evidence.Journal{RunID: id, State: evidence.StateReady, Manifest: strings.Repeat("a", 64), Tree: tree, WorktreePath: path}
	if err := evidence.WriteJournal(filepath.Join(s.Store.Dir, "runtime"), j); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(ctx, s.Store, s.Keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("leftover verification worktree: %v", err)
	}
	status := call(t, restarted, request("system.status", "", map[string]any{}))
	runs, _ := status["interrupted_runs"].([]any)
	if len(runs) != 1 || runs[0].(map[string]any)["run_id"] != id {
		t.Fatalf("interrupted_runs %v", status["interrupted_runs"])
	}
}
