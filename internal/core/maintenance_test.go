package core

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/CoachCoe/Ledger/internal/canon"
)

func TestM6MaintenanceAndHistoryCoverage(t *testing.T) {
	s := fixture(t)
	a := call(t, s, request("session.create", "", map[string]any{}))
	w := m2Workspace(t, s, a)
	expectCode(t, s, "system.recover", a["session_id"].(string), map[string]any{}, "unauthorized")
	before := call(t, s, request("history.attempts", "", map[string]any{"path": "a.ts"}))
	coverage := before["coverage"].(map[string]any)
	if coverage["scope"] != "indexed_committed_records" || coverage["verification"] != "not_performed" {
		t.Fatal(before)
	}
	call(t, s, request("system.recover", "", map[string]any{}))
	call(t, s, request("system.reindex", "", map[string]any{}))
	after := call(t, s, request("history.attempts", "", map[string]any{"path": "a.ts"}))
	x, _ := canon.Marshal(before)
	y, _ := canon.Marshal(after)
	if string(x) != string(y) {
		t.Fatal("maintenance changed durable query")
	}
	// Existing local signer authority survives a derived-index rebuild.
	call(t, s, request("workspace.write", a["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"], "path": "new.ts", "content": "export const n=1;\n", "expected_hash": nil}))
	bundle := filepath.Join(t.TempDir(), "fixture.ledger")
	call(t, s, request("bundle.export", "", map[string]any{"out": bundle, "manifests": []string{}, "intents": []string{}, "allow_partial": false}))
	call(t, s, request("bundle.verify", "", map[string]any{"bundle": bundle, "trust": []string{}, "strict": true}))
	call(t, s, request("bundle.import", "", map[string]any{"bundle": bundle, "trust": []string{}}))
	if _, err := s.Store.Index.Subject(context.Background(), a["session_id"].(string)); err != nil {
		t.Fatal(err)
	}
}
