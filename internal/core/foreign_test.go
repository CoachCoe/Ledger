package core

import (
	"context"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// A foreign change moves a line only by a root-signed advance from the
// line's head, and only for the tree its import mapping holds.
func TestForeignAdvanceRules(t *testing.T) {
	s, base := nativeFixture(t)
	ctx := context.Background()
	root, err := s.root()
	if err != nil {
		t.Fatal(err)
	}
	intent := call(t, s, request("intent.create", "", judgment("accept")))["intent_id"].(string)
	author := call(t, s, request("session.create", "", map[string]any{}))
	acceptor := call(t, s, request("session.create", "", map[string]any{"may_advance": []string{"main"}}))["session_id"].(string)
	change := proposeAndSelect(t, s, intent, author, "export const x=2;\n", true)

	publish := func(signer identity.Signer, objs ...envelope.Object) error {
		_, err := s.Store.Do(ctx, record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "test.foreign", Digest: canon.Hash([]byte(objs[0].Hash)), Signer: signer},
			func(*index.Index) (any, []envelope.Object, error) { return map[string]any{}, objs, nil })
		return err
	}
	seal := func(kind schema.Kind, p any, signer identity.Signer) envelope.Object {
		t.Helper()
		o, err := envelope.SealVersion(2, kind, p, signer)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	mapping := seal(schema.KindBridgeMapping, schema.BridgeMapping{NativeTree: change.tree, GitCommit: strings.Repeat("1", 40), GitTree: strings.Repeat("2", 40),
		GitParents: []string{}, Direction: "import", Author: "A <a@example.com> 1 +0000", Committer: "A <a@example.com> 1 +0000", Message: "in Git\n"}, root)
	if err := publish(root, mapping); err != nil {
		t.Fatal(err)
	}
	foreign := func(signer identity.Signer, baseTree, tree string) envelope.Object {
		return seal(schema.KindForeignChange, schema.ForeignChange{Line: "main", Participant: "bridge", Mapping: mapping.Hash, BaseTree: baseTree, Tree: tree,
			Actor: signer.ID(), At: "2026-11-05T12:00:00.000Z"}, signer)
	}
	advance := func(signer identity.Signer, f string) envelope.Object {
		return seal(schema.KindLineAdvance, schema.LineAdvance{Line: "main", ForeignChange: f, Actor: signer.ID(), At: "2026-11-05T12:00:00.000Z"}, signer)
	}

	sess := s.signers[acceptor]
	claimsRoot := seal(schema.KindForeignChange, schema.ForeignChange{Line: "main", Participant: "bridge", Mapping: mapping.Hash, BaseTree: base, Tree: change.tree,
		Actor: root.ID(), At: "2026-11-05T12:00:00.000Z"}, sess)
	if err := publish(sess, claimsRoot); !apierr.HasCode(err, "unauthorized") {
		t.Fatalf("session-signed foreign change: %v", err)
	}
	if err := publish(root, foreign(root, base, base)); !apierr.HasCode(err, "invalid_foreign_change") {
		t.Fatalf("foreign change for a tree its mapping does not hold: %v", err)
	}
	stale := foreign(root, change.tree, change.tree)
	if err := publish(root, stale, advance(root, stale.Hash)); !apierr.HasCode(err, "stale_base") {
		t.Fatalf("foreign change not based on the line head: %v", err)
	}
	f := foreign(root, base, change.tree)
	if err := publish(root, f); err != nil {
		t.Fatal(err)
	}
	if err := publish(sess, advance(sess, f.Hash)); !apierr.HasCode(err, "unauthorized") {
		t.Fatalf("session accepted a foreign change: %v", err)
	}
	if err := publish(root, advance(root, f.Hash)); err != nil {
		t.Fatal(err)
	}
	if st := call(t, s, request("line.status", "", map[string]any{})); st["tree"] != change.tree {
		t.Fatalf("line after the foreign advance %v", st)
	}
	if _, err := envelope.SealVersion(2, schema.KindLineAdvance, schema.LineAdvance{Line: "main", Manifest: change.manifest, Selection: change.selection,
		ForeignChange: f.Hash, Actor: root.ID(), At: "2026-11-05T12:00:00.000Z"}, root); err == nil {
		t.Fatal("an advance naming both a selection and a foreign change was sealed")
	}
}
