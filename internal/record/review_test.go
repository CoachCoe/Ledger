package record

import (
	"encoding/json/v2"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/identity"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func TestRecoveryRejectsForgedJournal(t *testing.T) {
	s := newStore(t)
	object := intentRecord(t, schema.NewID(schema.PrefixIntent))
	object.Envelope.Signature = seal(t, schema.KindRootKey, schema.RootKey{PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", CreatedAt: schema.Timestamp(fixedNow)}, root).Envelope.Signature
	b, err := canonEnvelope(object.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	object, err = envelope.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	g := externalGroup(t, object)
	j := journal{Receipt: g.Receipt.Hash, Objects: []string{string(object.Bytes), string(g.Receipt.Bytes)}}
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.journalPath(g.Receipt.Hash), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recover(ctx); err == nil {
		t.Fatal("forged journal became committed")
	}
	if ok, _ := s.Index.Committed(ctx, object.Hash); ok {
		t.Fatal("forged member indexed")
	}
}

func TestReindexPreservesOtherOpenStore(t *testing.T) {
	s := newStore(t)
	other := openStore(t, s.Repo, Options{})
	if _, err := s.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	id := schema.NewID(schema.PrefixIntent)
	if _, err := other.Do(ctx, op("after-reindex"), members(intentRecord(t, id))); err != nil {
		t.Fatal(err)
	}
	if subject(t, s, id) == nil {
		t.Fatal("writer used a detached database after reindex")
	}
}

func TestImportedSessionMustMatchCapability(t *testing.T) {
	s := newStore(t)
	a := startSession(t, s)
	sid := schema.NewID(schema.PrefixSession)
	bad := sessionRecord(t, sid, a.capID, a, root)
	pushToRef(t, s, externalGroup(t, bad))
	report, err := s.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if subject(t, s, sid) != nil {
		t.Fatal("session with another holder's capability accepted")
	}
	if len(report.Unverifiable) == 0 {
		t.Fatal("missing invalid session report")
	}
}

func TestMembersMustMatchTheirStoredBytes(t *testing.T) {
	s := newStore(t)
	o := intentRecord(t, schema.NewID(schema.PrefixIntent))
	o.Hash = "../../escape"
	_, err := s.Do(ctx, op("inconsistent-object"), func(*index.Index) (any, []envelope.Object, error) { return map[string]any{}, []envelope.Object{o}, nil })
	if err == nil {
		t.Fatal("inconsistent object accepted")
	}
}

func TestJournalFaultLeavesNoAcceptedState(t *testing.T) {
	s := newStore(t)
	w := openStore(t, s.Repo, Options{Fault: func(stage string) error {
		if stage == StageJournal {
			return errors.New("crash")
		}
		return nil
	}})
	id := schema.NewID(schema.PrefixIntent)
	if _, err := w.Do(ctx, op("not-prepared"), members(intentRecord(t, id))); err == nil {
		t.Fatal("missing fault")
	}
	if subject(t, s, id) != nil {
		t.Fatal("unprepared record visible")
	}
}

func TestSameSessionCanEndItself(t *testing.T) {
	s := newStore(t)
	a := startSession(t, s)
	sub := subject(t, s, a.ID())
	event := seal(t, schema.KindLifecycleEvent, schema.LifecycleEvent{SubjectKind: "session", SubjectID: a.ID(), Creation: sub.Creation, FromState: "active", ToState: "ended", Actor: a.ID(), At: schema.Timestamp(fixedNow)}, a)
	if _, err := s.Do(ctx, as(a, "self-end"), members(event)); err != nil {
		t.Fatal(err)
	}
	if subject(t, s, a.ID()).State != "ended" {
		t.Fatal("not ended")
	}
	_, err := s.Do(ctx, as(a, "after-self-end"), members(intentRecord(t, schema.NewID(schema.PrefixIntent))))
	if !apierr.HasCode(err, "session_inactive") {
		t.Fatal(err)
	}
}

func TestConflictingReceiptBlocksBothSubjects(t *testing.T) {
	s := newStore(t)
	a, b := intentRecord(t, schema.NewID(schema.PrefixIntent)), intentRecord(t, schema.NewID(schema.PrefixIntent))
	o := op("conflicting-op")
	if _, err := s.Do(ctx, o, members(a)); err != nil {
		t.Fatal(err)
	}
	receipt := seal(t, schema.KindOperationReceipt, schema.OperationReceipt{OperationID: o.ID, Operation: o.Name, RequestDigest: o.Digest, Result: []byte(`{}`), Members: []string{b.Hash}, At: schema.Timestamp(fixedNow)}, root)
	pushToRef(t, s, index.Group{Receipt: receipt, Members: []envelope.Object{b}})
	if _, err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for _, creation := range []envelope.Object{a, b} {
		in := mustPayload[schema.Intent](t, creation)
		if subject(t, s, in.ID).Conflict != "operation_conflict" {
			t.Fatal("conflicting operation did not block subject")
		}
	}
	before := digest(t, s)
	if _, err := s.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	if digest(t, s) != before {
		t.Fatal("conflict differs after reindex")
	}
}

func TestRootKeyRepublicationKeepsOneAuthority(t *testing.T) {
	s := newStore(t)
	key := seal(t, schema.KindRootKey, schema.RootKey{PublicKey: identity.EncodePublicKey(root.PublicKey()), CreatedAt: schema.Timestamp(fixedNow.Add(time.Hour))}, root)
	pushToRef(t, s, externalGroup(t, key))
	if _, err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, op("after-root-republication"), members(intentRecord(t, schema.NewID(schema.PrefixIntent)))); err != nil {
		t.Fatal(err)
	}
}

func TestIntentCannotParentOrSupersedeItself(t *testing.T) {
	s := newStore(t)
	id := schema.NewID(schema.PrefixIntent)
	p := mustPayload[schema.Intent](t, intentRecord(t, id))
	p.ParentID = &id
	bad := seal(t, schema.KindIntent, p, root)
	if _, err := s.Do(ctx, op("self-parent"), members(bad)); err == nil {
		t.Fatal("self-parent intent accepted")
	}
	valid := intentRecord(t, id)
	if _, err := s.Do(ctx, op("valid-intent"), members(valid)); err != nil {
		t.Fatal(err)
	}
	reason := "superseded"
	event := seal(t, schema.KindLifecycleEvent, schema.LifecycleEvent{SubjectKind: "intent", SubjectID: id, Creation: valid.Hash, FromState: "open", ToState: "closed", Reason: &reason, ReplacementID: &id, Actor: root.ID(), At: schema.Timestamp(fixedNow)}, root)
	if _, err := s.Do(ctx, op("self-supersede"), members(event)); err == nil {
		t.Fatal("self-supersession accepted")
	}
}

func TestSingleRefFaultStopsPublication(t *testing.T) {
	s := newStore(t)
	failed := false
	writer := openStore(t, s.Repo, Options{Fault: func(stage string) error {
		if stage == StageRefCAS && !failed {
			failed = true
			return errors.New("single CAS interruption")
		}
		return nil
	}})
	if _, err := writer.Do(ctx, op("single-ref-fault"), members(intentRecord(t, schema.NewID(schema.PrefixIntent)))); err == nil {
		t.Fatal("one-shot fault was swallowed by ref fallback")
	}
}
