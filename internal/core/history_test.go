package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func TestAC199_200_203_HistorySurvivesReindex(t *testing.T) {
	s := fixture(t)
	f := newSpec(t, s, submissionScorer(), nil)
	for i := range 3 {
		f.write(t, i, "a.ts", "export const x="+string(rune('2'+i))+";\n")
	}
	m := f.scripted(t, "5", "9")
	call(t, s, f.runReq("speculation.evaluate"))
	call(t, s, request("speculation.score", "", map[string]any{"run_id": f.runID, "scores": map[string]int{m[0]: 5, m[1]: 9}}))
	manifest := readRecord[schema.ChangeManifest](t, s, m[0])
	symbol := manifest.SymbolsWritten[0]
	queries := map[string]any{"history.why": map[string]any{"symbol": symbol}, "history.attempts": map[string]any{"path": "a.ts"}}
	before := map[string]string{}
	for op, in := range queries {
		out := call(t, s, request(op, "", in))
		rows := out["records"].([]any)
		want := 3
		if op == "history.attempts" {
			want = 2
		}
		if len(rows) != want {
			t.Fatalf("%s rows: %v", op, rows)
		}
		b, _ := canon.Marshal(out)
		before[op] = string(b)
		if !strings.Contains(string(b), m[0]) {
			t.Fatal("missing portable manifest hash")
		}
	}
	if _, err := s.Store.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	for op, in := range queries {
		b, _ := canon.Marshal(call(t, s, request(op, "", in)))
		if string(b) != before[op] {
			t.Fatalf("reindex changed %s", op)
		}
	}
	other := symbol
	other.Name = "missing"
	if len(call(t, s, request("history.why", "", map[string]any{"symbol": other}))["records"].([]any)) != 0 {
		t.Fatal("symbol query matched merely by path")
	}
}

func TestM6HistoryReportsMissingAndPendingProvenance(t *testing.T) {
	s := fixture(t)
	root, err := s.root()
	if err != nil {
		t.Fatal(err)
	}
	missing := strings.Repeat("a", 64)
	receipt, err := envelope.SealVersion(2, schema.KindOperationReceipt, schema.OperationReceipt{OperationID: schema.NewID(schema.PrefixOperation), Operation: "intent.create", RequestDigest: canon.Hash([]byte("missing")), Result: []byte(`{}`), Members: []string{missing}, At: now()}, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.ImportRecords(context.Background(), []envelope.Object{receipt}); err != nil {
		t.Fatal(err)
	}
	out := call(t, s, request("history.attempts", "", map[string]any{"path": "a.ts"}))
	coverage := out["coverage"].(map[string]any)
	if len(out["records"].([]any)) != 0 || len(coverage["receipt_missing_members"].([]any)) != 1 || coverage["receipt_missing_members"].([]any)[0] != missing || len(coverage["pending_records"].([]any)) != 1 {
		t.Fatal(out)
	}
}

func TestAC202_SelectedDependentsOnly(t *testing.T) {
	s := fixture(t)
	a := call(t, s, request("session.create", "", map[string]any{}))
	w := m2Workspace(t, s, a)
	if err := os.WriteFile(filepath.Join(w["path"].(string), "a.ts"), []byte("export const x=1;\nexport function use(){ return x; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := call(t, s, request("workspace.propose", a["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"]}))
	m := readRecord[schema.ChangeManifest](t, s, p["manifest"].(string))
	var target schema.Symbol
	for _, sym := range m.SymbolsRead {
		if sym.Name == "x" {
			target = sym
		}
	}
	if target.Name == "" {
		t.Fatalf("no x read: %+v", m)
	}
	in := map[string]any{"symbol": target}
	if len(call(t, s, request("history.dependents", "", in))["records"].([]any)) != 0 {
		t.Fatal("unselected change included")
	}
	v := call(t, s, request("session.create", "", map[string]any{"participant_kind": "human"}))
	call(t, s, request("selection.record", v["session_id"].(string), map[string]any{"manifest": p["manifest"], "evidence": []string{}}))
	out := call(t, s, request("history.dependents", "", in))["records"].([]any)
	if len(out) != 1 || out[0].(map[string]any)["confidence"] != "manifest_cooccurrence" {
		t.Fatal(out)
	}
}

func TestQueryPathsUseCapabilityNormalization(t *testing.T) {
	s := fixture(t)
	s.caseSensitive = false
	if !s.samePath("Straße/cafe\u0301.ts", "STRASSE/café.ts") {
		t.Fatal("path policy drifted from capability normalization")
	}
	expectCode(t, s, "history.attempts", "", map[string]any{"path": "../outside"}, "invalid_path")
	expectCode(t, s, "coordination.nearby", "", map[string]any{"path": "../outside"}, "invalid_path")
}

func TestAC201_203_NearbyRuntimeDigestAndLeases(t *testing.T) {
	s := fixture(t)
	a := call(t, s, request("session.create", "", map[string]any{}))
	w := m2Workspace(t, s, a)
	call(t, s, request("workspace.write", a["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"], "path": "a.ts", "content": "export const x=2;\n", "expected_hash": canon.Hash([]byte("export const x=1;\n"))}))
	call(t, s, request("coordination.lease", a["session_id"].(string), map[string]any{"session_id": a["session_id"], "paths": []string{"a.ts"}, "symbols": []schema.Symbol{}, "reason": "editing", "expires_at": schema.Timestamp(time.Now().Add(time.Hour))}))
	out := call(t, s, request("coordination.nearby", "", map[string]any{"path": "a.ts"}))
	if out["runtime"] != true || out["unsigned"] != true || len(out["workspaces"].([]any)) != 1 || len(out["leases"].([]any)) != 1 {
		t.Fatal(out)
	}
	digest := out["snapshot_digest"]
	delete(out, "snapshot_digest")
	b, _ := canon.Marshal(out)
	if canon.Hash(b) != digest {
		t.Fatal("snapshot digest mismatch")
	}
	call(t, s, request("session.end", "", map[string]any{"session_id": a["session_id"]}))
	out = call(t, s, request("coordination.nearby", "", map[string]any{"path": "a.ts"}))
	if len(out["workspaces"].([]any))+len(out["leases"].([]any)) != 0 {
		t.Fatal("ended authority remained nearby", out)
	}
}
