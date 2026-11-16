package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// lineChange is one proposed native workspace and a root selection of it.
type lineChange struct {
	workspace, manifest, tree, selection string
}

// proposeAndSelect forks a workspace for intent as session, writes a.ts,
// proposes it and has root select it, with a passing judgment when judged.
func proposeAndSelect(t *testing.T, s *Service, intent string, sess map[string]any, body string, judged bool) lineChange {
	t.Helper()
	sid := sess["session_id"].(string)
	fork := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": intent, "capability_id": sess["capability_id"]}))
	if err := os.WriteFile(filepath.Join(fork["path"].(string), "a.ts"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	wid := fork["workspace_id"].(string)
	prop := call(t, s, request("workspace.propose", sid, map[string]any{"workspace_id": wid}))
	evidence := []string{}
	if judged {
		evidence = append(evidence, judge(t, s, "", prop, "review")["attestation"].(string))
	}
	sel := call(t, s, request("selection.record", "", map[string]any{"manifest": prop["manifest"], "evidence": evidence}))
	return lineChange{workspace: wid, manifest: prop["manifest"].(string), tree: prop["tree"].(string), selection: sel["selection"].(string)}
}

// P2-AC-023: a line advances only through a session holding may_advance for
// it, only from its current head, and only for a manifest based on that
// head; new workspaces then fork from the accepted tree.
func TestLineAdvance(t *testing.T) {
	s, base := nativeFixture(t)
	ctx := context.Background()
	advance := func(sid, selection string) (map[string]any, error) {
		r, err := s.Handle(ctx, request("line.advance", sid, map[string]any{"selection": selection}))
		m, _ := r.(map[string]any)
		return m, err
	}
	intent := call(t, s, request("intent.create", "", judgment("accept")))["intent_id"].(string)
	author := call(t, s, request("session.create", "", map[string]any{"may_advance": []string{"main"}}))
	plain := call(t, s, request("session.create", "", map[string]any{}))["session_id"].(string)
	acceptor := call(t, s, request("session.create", "", map[string]any{"may_advance": []string{"main"}}))["session_id"].(string)
	other := call(t, s, request("session.create", "", map[string]any{"may_advance": []string{"release/1.x"}}))["session_id"].(string)

	st := call(t, s, request("line.status", "", map[string]any{}))
	if st["line"] != "main" || st["advance"] != nil || st["tree"] != base {
		t.Fatalf("initial line %v", st)
	}

	accepted := proposeAndSelect(t, s, intent, author, "export const x=2;\n", true)
	behind := proposeAndSelect(t, s, intent, author, "export const x=3;\n", true)
	unjudged := proposeAndSelect(t, s, intent, author, "export const x=4;\n", false)
	contested := proposeAndSelect(t, s, intent, author, "export const x=5;\n", true)
	call(t, s, request("evidence.judge", "", map[string]any{"manifest": contested.manifest, "criterion_id": "review", "result": "fail"}))

	if _, err := advance(author["session_id"].(string), accepted.selection); !apierr.HasCode(err, "unauthorized") {
		t.Fatalf("author advanced its own change: %v", err)
	}
	for _, sid := range []string{plain, other} {
		if _, err := advance(sid, accepted.selection); !apierr.HasCode(err, "unauthorized") {
			t.Fatalf("session without may_advance on main: %v", err)
		}
	}
	if _, err := advance(acceptor, unjudged.selection); !apierr.HasCode(err, "criteria_unsatisfied") {
		t.Fatalf("outstanding criterion accepted: %v", err)
	}
	if _, err := advance(acceptor, contested.selection); !apierr.HasCode(err, "criteria_unsatisfied") {
		t.Fatalf("change with contrary evidence after selection accepted: %v", err)
	}

	r := request("line.advance", acceptor, map[string]any{"selection": accepted.selection})
	got := call(t, s, r)
	if got["line"] != "main" || got["previous"] != nil || got["manifest"] != accepted.manifest || got["tree"] != accepted.tree {
		t.Fatalf("advance %v", got)
	}
	if again := call(t, s, r); again["advance"] != got["advance"] {
		t.Fatalf("retry %v", again)
	}
	o, err := s.Store.Read(got["advance"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var a schema.LineAdvance
	if err := o.Decode(&a); err != nil || o.Envelope.SchemaVersion != 2 || a.Actor != acceptor || a.Selection != accepted.selection || a.Previous != nil {
		t.Fatalf("advance record v%d %+v, %v", o.Envelope.SchemaVersion, a, err)
	}
	st = call(t, s, request("line.status", "", map[string]any{"line": "main"}))
	if st["advance"] != got["advance"] || st["tree"] != accepted.tree {
		t.Fatalf("line after advance %v", st)
	}

	if _, err := advance("", behind.selection); !apierr.HasCode(err, "stale_base") {
		t.Fatalf("manifest based on the old head: %v", err)
	}
	fork := call(t, s, request("workspace.fork", author["session_id"].(string), map[string]any{"intent_id": intent, "capability_id": author["capability_id"]}))
	if fork["base_tree"] != accepted.tree {
		t.Fatalf("fork after advance based on %v", fork["base_tree"])
	}
	cr, err := s.Store.Index.Creation(ctx, fork["workspace_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var w schema.Workspace
	if err := cr.Decode(&w); err != nil || w.BaseLineAdvance == nil || *w.BaseLineAdvance != got["advance"] {
		t.Fatalf("workspace base advance %+v, %v", w, err)
	}
	if _, err := s.Handle(ctx, request("line.advance", acceptor, map[string]any{"selection": accepted.manifest})); !apierr.HasCode(err, "unknown_selection") {
		t.Fatalf("a manifest hash as the selection: %v", err)
	}
}
