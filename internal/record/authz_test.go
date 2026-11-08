package record

import (
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
)

type session struct {
	identity.Signer
	capID string
}

// startSession publishes a root-issued whole-repository capability and the
// session it authorizes.
func startSession(t *testing.T, s *Store) session {
	t.Helper()
	sid, cid := schema.NewID(schema.PrefixSession), schema.NewID(schema.PrefixCapability)
	key, err := identity.NewSession(sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, op("session"), members(capability(t, cid, sid, root), sessionRecord(t, sid, cid, key, root))); err != nil {
		t.Fatal(err)
	}
	return session{key, cid}
}

func capability(t *testing.T, id, sessionID string, signer identity.Signer) envelope.Object {
	t.Helper()
	return seal(t, schema.KindCapability, schema.Capability{
		ID: id, SessionID: sessionID, Issuer: schema.Issuer{Kind: "root", Fingerprint: root.ID()},
		Allow: []string{"**"}, Deny: []string{}, ExpiresAt: "2026-11-06T12:00:00.000Z", CreatedAt: schema.Timestamp(fixedNow),
	}, signer)
}

func sessionRecord(t *testing.T, id, capID string, key, signer identity.Signer) envelope.Object {
	t.Helper()
	return seal(t, schema.KindSession, schema.Session{
		ID: id, CapabilityID: capID, PublicKey: identity.EncodePublicKey(key.PublicKey()),
		ParticipantKind: "agent", StartedAt: schema.Timestamp(fixedNow),
	}, signer)
}

func as(signer identity.Signer, name string) Op {
	o := op(name)
	o.Signer = signer
	return o
}

func workspaceRecord(t *testing.T, intentID string, owner session, signer identity.Signer) (string, envelope.Object) {
	t.Helper()
	id := schema.NewID(schema.PrefixWorkspace)
	return id, seal(t, schema.KindWorkspace, schema.Workspace{
		ID: id, IntentID: intentID, SessionID: owner.ID(), CapabilityID: owner.capID,
		ChangeID: schema.NewID(schema.PrefixChange), BaseCommit: strings.Repeat("c", 40), CreatedAt: schema.Timestamp(fixedNow),
	}, signer)
}

func workspaceEvent(t *testing.T, id, creation string, previous *string, from, to string, signer identity.Signer) envelope.Object {
	t.Helper()
	return seal(t, schema.KindLifecycleEvent, schema.LifecycleEvent{
		SubjectKind: "workspace", SubjectID: id, Creation: creation, Previous: previous,
		FromState: from, ToState: to, Actor: signer.ID(), At: schema.Timestamp(fixedNow),
	}, signer)
}

func binding(t *testing.T, ws, previous string, to session, signer identity.Signer) envelope.Object {
	t.Helper()
	return seal(t, schema.KindWorkspaceBinding, schema.WorkspaceBinding{
		WorkspaceID: ws, Previous: previous, SessionID: to.ID(), CapabilityID: to.capID,
		Actor: signer.ID(), Reason: "recovery", At: schema.Timestamp(fixedNow),
	}, signer)
}

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	if !apierr.HasCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestSessionAuthentication(t *testing.T) {
	s := newStore(t)
	sid, cid := schema.NewID(schema.PrefixSession), schema.NewID(schema.PrefixCapability)
	key, _ := identity.NewSession(sid)
	other := startSession(t, s)

	_, err := s.Do(ctx, op("self-signed"), members(capability(t, cid, sid, root), sessionRecord(t, sid, cid, key, other)))
	expectCode(t, err, "group_invalid")

	// A capability issued for a different session cannot authenticate a
	// substituted session key.
	_, err = s.Do(ctx, op("substitute"), members(sessionRecord(t, sid, other.capID, key, root)))
	expectCode(t, err, "unauthorized")

	if _, err := s.Do(ctx, op("ok"), members(capability(t, cid, sid, root), sessionRecord(t, sid, cid, key, root))); err != nil {
		t.Fatal(err)
	}
}

func TestSessionSigningAndEnd(t *testing.T) {
	s := newStore(t)
	a := startSession(t, s)
	in := seal(t, schema.KindIntent, schema.Intent{
		ID: schema.NewID(schema.PrefixIntent), Description: "by agent",
		Criteria: []schema.Criterion{{ID: "c", Statement: "s", Verification: schema.Verification{Kind: "judgment", Reviewer: "any"}}},
		Source:   schema.Source{Kind: "human"}, Owner: a.ID(), CreatedAt: schema.Timestamp(fixedNow),
	}, a)
	if _, err := s.Do(ctx, as(a, "intent"), members(in)); err != nil {
		t.Fatal(err)
	}
	misowned := intentRecord(t, schema.NewID(schema.PrefixIntent))
	_, err := s.Do(ctx, as(a, "foreign"), members(seal(t, schema.KindIntent, mustPayload[schema.Intent](t, misowned), a)))
	expectCode(t, err, "unauthorized")

	sub := subject(t, s, a.ID())
	end := seal(t, schema.KindLifecycleEvent, schema.LifecycleEvent{
		SubjectKind: "session", SubjectID: a.ID(), Creation: sub.Creation, FromState: "active", ToState: "ended",
		Actor: root.ID(), At: schema.Timestamp(fixedNow),
	}, root)
	if _, err := s.Do(ctx, op("end"), members(end)); err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(ctx, as(a, "after-end"), members(intentRecord(t, schema.NewID(schema.PrefixIntent))))
	expectCode(t, err, "session_inactive")
}

func mustPayload[T any](t *testing.T, o envelope.Object) T {
	t.Helper()
	var v T
	if err := o.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestWorkspaceOwnershipAndRebinding(t *testing.T) {
	s := newStore(t)
	a, b := startSession(t, s), startSession(t, s)
	intentID := schema.NewID(schema.PrefixIntent)
	if _, err := s.Do(ctx, op("intent"), members(intentRecord(t, intentID))); err != nil {
		t.Fatal(err)
	}
	_, forB := workspaceRecord(t, intentID, b, a)
	_, err := s.Do(ctx, as(a, "steal"), members(forB))
	expectCode(t, err, "unauthorized")

	ws, rec := workspaceRecord(t, intentID, a, a)
	if _, err := s.Do(ctx, as(a, "fork"), members(rec)); err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(ctx, as(b, "propose"), members(workspaceEvent(t, ws, rec.Hash, nil, "open", "proposed", b)))
	expectCode(t, err, "unauthorized")

	actorMismatch := seal(t, schema.KindLifecycleEvent, schema.LifecycleEvent{
		SubjectKind: "workspace", SubjectID: ws, Creation: rec.Hash, FromState: "open", ToState: "proposed",
		Actor: b.ID(), At: schema.Timestamp(fixedNow),
	}, a)
	_, err = s.Do(ctx, as(a, "actor"), members(actorMismatch))
	expectCode(t, err, "unauthorized")

	_, err = s.Do(ctx, as(a, "self-rebind"), members(binding(t, ws, rec.Hash, b, a)))
	expectCode(t, err, "unauthorized")
	_, err = s.Do(ctx, op("stale-rebind"), members(binding(t, ws, strings.Repeat("0", 64), b, root)))
	expectCode(t, err, "stale_predecessor")
	if _, err := s.Do(ctx, op("rebind"), members(binding(t, ws, rec.Hash, b, root))); err != nil {
		t.Fatal(err)
	}
	if sub := subject(t, s, ws); sub.Owner != b.ID() {
		t.Fatalf("owner %s after rebinding to %s", sub.Owner, b.ID())
	}
	_, err = s.Do(ctx, as(a, "old-owner"), members(workspaceEvent(t, ws, rec.Hash, nil, "open", "proposed", a)))
	expectCode(t, err, "unauthorized")
	closeEv := workspaceEvent(t, ws, rec.Hash, nil, "open", "closed", b)
	if _, err := s.Do(ctx, as(b, "close"), members(closeEv)); err != nil {
		t.Fatal(err)
	}
	head := subject(t, s, ws).BindingHead
	_, err = s.Do(ctx, op("rebind-closed"), members(binding(t, ws, head, a, root)))
	expectCode(t, err, "invalid_state")

	before := digest(t, s)
	if _, err := s.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	if digest(t, s) != before || subject(t, s, ws).Owner != b.ID() {
		t.Fatalf("ownership not reconstructed by reindex: %+v", subject(t, s, ws))
	}
}

func TestDelegationRequiresHolder(t *testing.T) {
	s := newStore(t)
	a, b := startSession(t, s), startSession(t, s)
	child := func(signer identity.Signer) envelope.Object {
		return seal(t, schema.KindCapability, schema.Capability{
			ID: schema.NewID(schema.PrefixCapability), SessionID: b.ID(),
			Issuer: schema.Issuer{Kind: "capability", CapabilityID: a.capID},
			Allow:  []string{"src/**"}, Deny: []string{}, ExpiresAt: "2026-11-06T12:00:00.000Z", CreatedAt: schema.Timestamp(fixedNow),
		}, signer)
	}
	_, err := s.Do(ctx, as(b, "not-holder"), members(child(b)))
	expectCode(t, err, "unauthorized")
	if _, err := s.Do(ctx, as(a, "holder"), members(child(a))); err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(ctx, as(a, "root-claim"), members(capability(t, schema.NewID(schema.PrefixCapability), b.ID(), a)))
	expectCode(t, err, "unauthorized")
}

func TestCheckCriteriaPinDefinitions(t *testing.T) {
	s := newStore(t)
	def := seal(t, schema.KindCheckDefinition, schema.CheckDefinition{
		Name: "unit", Category: "mechanical", Command: "go test ./...", WorkingDirectory: ".",
		EnvAllowlist: []string{}, TimeoutSeconds: 60, OutputLimitBytes: 1024,
	}, root)
	intent := func(check, hash string) envelope.Object {
		return seal(t, schema.KindIntent, schema.Intent{
			ID: schema.NewID(schema.PrefixIntent), Description: "checked",
			Criteria: []schema.Criterion{{ID: "tests", Statement: "pass",
				Verification: schema.Verification{Kind: "check", Check: check, DefinitionHash: hash}}},
			Source: schema.Source{Kind: "human"}, Owner: root.ID(), CreatedAt: schema.Timestamp(fixedNow),
		}, root)
	}
	_, err := s.Do(ctx, op("unknown"), members(intent("unit", canon.Hash([]byte("x")))))
	expectCode(t, err, "unknown_entity")
	_, err = s.Do(ctx, op("renamed"), members(def, intent("lint", def.Hash)))
	expectCode(t, err, "invalid_intent")
	if _, err := s.Do(ctx, op("ok"), members(def, intent("unit", def.Hash))); err != nil {
		t.Fatal(err)
	}
	// A committed definition can be pinned by later intents.
	if _, err := s.Do(ctx, op("reuse"), members(intent("unit", def.Hash))); err != nil {
		t.Fatal(err)
	}
}

func TestStaleRefCacheFallsBack(t *testing.T) {
	s := newStore(t)
	if err := os.WriteFile(s.refTipFile(), []byte(strings.Repeat("0", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, op("create"), members(intentRecord(t, schema.NewID(schema.PrefixIntent)))); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(refHashes(t, s), diskHashes(t, s)) {
		t.Fatal("ref and disk differ")
	}
}

func TestM2DelegationReplayAndHistoricalExpiry(t *testing.T) {
	// AC-164–165: parent expiry, widening, wrong holder and recorded time.
	s := newStore(t)
	a, b := startSession(t, s), startSession(t, s)
	parent := capability(t, a.capID, a.ID(), root)
	p := mustPayload[schema.Capability](t, parent)
	makeChild := func(allow []string, expires string, signer identity.Signer) envelope.Object {
		return seal(t, schema.KindCapability, schema.Capability{ID: schema.NewID(schema.PrefixCapability), SessionID: b.ID(), Issuer: schema.Issuer{Kind: "capability", CapabilityID: p.ID}, Allow: allow, Deny: []string{}, ExpiresAt: expires, CreatedAt: schema.Timestamp(fixedNow)}, signer)
	}
	good := makeChild([]string{"src/**"}, "2026-11-06T11:00:00.000Z", a)
	if _, err := s.Do(ctx, as(a, "delegate"), members(good)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Do(ctx, as(a, "expiry-widen"), members(makeChild([]string{"src/**"}, "2026-11-07T12:00:00.000Z", a)))
	expectCode(t, err, "capability_widening")
	_, err = s.Do(ctx, as(b, "wrong-holder"), members(makeChild([]string{"src/**"}, "2026-11-06T11:00:00.000Z", b)))
	expectCode(t, err, "unauthorized")
	// Replay verifies historical claims even when the verifier wall clock is
	// later than every capability in the chain.
	s.opts.Now = func() time.Time { return fixedNow.Add(7 * 24 * time.Hour) }
	if _, err := s.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = s.Do(ctx, as(a, "expired-parent"), members(makeChild([]string{"src/**"}, "2026-11-06T11:00:00.000Z", a)))
	expectCode(t, err, "capability_expired")
}
