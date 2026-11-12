package record

import (
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/selection"
)

func sealV2(t testing.TB, kind schema.Kind, payload any, s identity.Signer) envelope.Object {
	t.Helper()
	o, err := envelope.SealVersion(2, kind, payload, s)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// nativeSession starts a session whose v2 capability may advance lines.
func nativeSession(t *testing.T, s *Store, lines ...string) session {
	t.Helper()
	sid, cid := schema.NewID(schema.PrefixSession), schema.NewID(schema.PrefixCapability)
	key, err := identity.NewSession(sid)
	if err != nil {
		t.Fatal(err)
	}
	c := sealV2(t, schema.KindCapability, schema.CapabilityV2{
		ID: cid, SessionID: sid, Issuer: schema.Issuer{Kind: "root", Fingerprint: root.ID()},
		Allow: []string{"**"}, Deny: []string{}, MayAdvance: append([]string{}, lines...),
		ExpiresAt: "2026-11-06T12:00:00.000Z", CreatedAt: schema.Timestamp(fixedNow),
	}, root)
	if _, err := s.Do(ctx, op("session"), members(c, sessionRecord(t, sid, cid, key, root))); err != nil {
		t.Fatal(err)
	}
	return session{key, cid}
}

// proposeOn records a workspace on base and a root-selected manifest of
// tree, returning the manifest and selection hashes.
func proposeOn(t *testing.T, s *Store, author session, intentID, base, tree string) (string, string) {
	t.Helper()
	id := schema.NewID(schema.PrefixWorkspace)
	change := schema.NewID(schema.PrefixChange)
	w := sealV2(t, schema.KindWorkspace, schema.WorkspaceV2{ID: id, IntentID: intentID, SessionID: author.ID(),
		CapabilityID: author.capID, ChangeID: change, BaseTree: base, CreatedAt: schema.Timestamp(fixedNow)}, author)
	if _, err := s.Do(ctx, as(author, "fork"), members(w)); err != nil {
		t.Fatal(err)
	}
	m := sealV2(t, schema.KindChangeManifest, schema.ChangeManifestV2{ChangeID: change, IntentID: intentID, WorkspaceID: id,
		AuthorSession: author.ID(), ContextRefs: []string{}, BaseTree: base, Tree: tree, Files: []string{"a.txt"},
		SymbolsRead: []schema.SymbolV2{}, SymbolsWritten: []schema.SymbolV2{}, OpHead: strings.Repeat("f", 64),
		SymbolMaps: map[string]string{}}, author)
	ev := workspaceEvent(t, id, w.Hash, "proposed", author)
	propose := as(author, "propose")
	propose.Name = "workspace.propose"
	build := func(*index.Index) (any, []envelope.Object, error) {
		return map[string]any{"manifest": m.Hash, "workspace_id": id}, []envelope.Object{m, ev}, nil
	}
	if _, err := s.Do(ctx, propose, build); err != nil {
		t.Fatal(err)
	}
	intent, err := s.Index.Creation(ctx, intentID)
	if err != nil {
		t.Fatal(err)
	}
	var in schema.Intent
	if err := intent.Decode(&in); err != nil {
		t.Fatal(err)
	}
	res := selection.Evaluate(selection.Binding{Manifest: m.Hash, Tree: tree, Criteria: in.Criteria}, nil, nil)
	sel := sealV2(t, schema.KindSelectionDecision, schema.SelectionDecisionV2{IntentID: intentID, Manifest: m.Hash,
		Competitors: []string{}, Evidence: []string{}, Criteria: res.Criteria, ContraryEvidence: res.Contrary,
		Selector: schema.Selector{Kind: "root", ID: root.ID()}, Scoring: []byte("null"), At: schema.Timestamp(fixedNow)}, root)
	if _, err := s.Do(ctx, op("select"), members(sel)); err != nil {
		t.Fatal(err)
	}
	return m.Hash, sel.Hash
}

func advance(t *testing.T, line string, previous *string, manifest, sel string, signer identity.Signer) envelope.Object {
	t.Helper()
	return sealV2(t, schema.KindLineAdvance, schema.LineAdvance{Line: line, Previous: previous, Manifest: manifest,
		Selection: sel, Actor: signer.ID(), At: schema.Timestamp(fixedNow)}, signer)
}

func TestLineAdvancesChainFromTheRepositoryTree(t *testing.T) {
	s, _ := newNative(t)
	t0, t1, t2 := strings.Repeat("0", 64), strings.Repeat("1", 64), strings.Repeat("2", 64)
	repo := sealV2(t, schema.KindRepository, schema.Repository{ID: schema.NewID(schema.PrefixRepository),
		HashAlgorithm: "sha256", CasePolicy: "sensitive", Analyzers: []string{}, Tree: t0, CreatedAt: schema.Timestamp(fixedNow)}, root)
	if _, err := s.Do(ctx, op("repository"), members(repo)); err != nil {
		t.Fatal(err)
	}
	again := sealV2(t, schema.KindRepository, schema.Repository{ID: schema.NewID(schema.PrefixRepository),
		HashAlgorithm: "sha256", CasePolicy: "sensitive", Analyzers: []string{}, Tree: t1, CreatedAt: schema.Timestamp(fixedNow)}, root)
	_, err := s.Do(ctx, op("repository-again"), members(again))
	expectCode(t, err, "entity_exists")

	head, err := s.Index.LineHead(ctx, "main")
	if err != nil || head.Advance != nil || head.Tree != t0 {
		t.Fatalf("initial head %+v, %v", head, err)
	}
	author := nativeSession(t, s)
	advancer := nativeSession(t, s, "main")
	intent := intentRecord(t, schema.NewID(schema.PrefixIntent))
	if _, err := s.Do(ctx, op("intent"), members(intent)); err != nil {
		t.Fatal(err)
	}
	intentID := mustPayload[schema.Intent](t, intent).ID
	m1, sel1 := proposeOn(t, s, author, intentID, t0, t1)

	_, err = s.Do(ctx, as(author, "advance"), members(advance(t, "main", nil, m1, sel1, author)))
	expectCode(t, err, "unauthorized")
	_, err = s.Do(ctx, as(advancer, "advance"), members(advance(t, "release", nil, m1, sel1, advancer)))
	expectCode(t, err, "unauthorized")

	a1 := advance(t, "main", nil, m1, sel1, advancer)
	if _, err := s.Do(ctx, as(advancer, "advance"), members(a1)); err != nil {
		t.Fatal(err)
	}
	head, err = s.Index.LineHead(ctx, "main")
	if err != nil || head.Advance == nil || *head.Advance != a1.Hash || head.Tree != t1 {
		t.Fatalf("head after advance %+v, %v", head, err)
	}

	// A manifest still based on the old head cannot advance the line, from
	// either head.
	m2, sel2 := proposeOn(t, s, author, intentID, t0, t2)
	_, err = s.Do(ctx, op("advance-stale"), members(advance(t, "main", nil, m2, sel2, root)))
	expectCode(t, err, "stale_line")
	_, err = s.Do(ctx, op("advance-old-base"), members(advance(t, "main", &a1.Hash, m2, sel2, root)))
	expectCode(t, err, "stale_base")

	m3, sel3 := proposeOn(t, s, author, intentID, t1, t2)
	_, err = s.Do(ctx, op("advance-wrong-selection"), members(advance(t, "main", &a1.Hash, m3, sel1, root)))
	expectCode(t, err, "invalid_advance")
	if _, err := s.Do(ctx, op("advance-root"), members(advance(t, "main", &a1.Hash, m3, sel3, root))); err != nil {
		t.Fatal(err)
	}
	head, err = s.Index.LineHead(ctx, "main")
	if err != nil || head.Tree != t2 {
		t.Fatalf("head after second advance %+v, %v", head, err)
	}

	// A workspace forked from an advance must start from the tree it accepted.
	w := sealV2(t, schema.KindWorkspace, schema.WorkspaceV2{ID: schema.NewID(schema.PrefixWorkspace), IntentID: intentID,
		SessionID: author.ID(), CapabilityID: author.capID, ChangeID: schema.NewID(schema.PrefixChange),
		BaseTree: t0, BaseLineAdvance: &a1.Hash, CreatedAt: schema.Timestamp(fixedNow)}, author)
	_, err = s.Do(ctx, as(author, "fork-mismatch"), members(w))
	expectCode(t, err, "binding_mismatch")
}
