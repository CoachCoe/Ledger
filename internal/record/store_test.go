package record

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func TestPublishAndRetry(t *testing.T) {
	s := newStore(t)
	id := schema.NewID(schema.PrefixIntent)
	in := intentRecord(t, id)
	o := op("create")
	res, err := s.Do(ctx, o, members(in))
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed || len(res.Members) != 1 {
		t.Fatalf("result %+v", res)
	}
	for _, h := range []string{in.Hash, res.Receipt} {
		if _, err := s.Read(h); err != nil {
			t.Fatal(err)
		}
	}
	checkLog(t, s)
	if sub := subject(t, s, id); sub == nil || sub.State != "open" || sub.Owner != root.ID() {
		t.Fatalf("subject %+v", sub)
	}
	before := len(diskHashes(t, s))

	called := false
	again, err := s.Do(ctx, o, func(*index.Index) (any, []envelope.Object, error) {
		called = true
		return nil, nil, nil
	})
	if err != nil || !again.Replayed || again.Receipt != res.Receipt || string(again.Result) != string(res.Result) {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	if called {
		t.Fatal("retry rebuilt the operation")
	}
	changed := o
	changed.Digest = schema.NewID("x_")
	if _, err := s.Do(ctx, changed, members(in)); !apierr.HasCode(err, "operation_mismatch") {
		t.Fatalf("reused id with a different request: %v", err)
	}
	if after := len(diskHashes(t, s)); after != before {
		t.Fatalf("retries wrote %d new files", after-before)
	}
}

func TestRejectedGroupWritesNothing(t *testing.T) {
	s := newStore(t)
	id := schema.NewID(schema.PrefixIntent)
	in := intentRecord(t, id)
	if _, err := s.Do(ctx, op("create"), members(in)); err != nil {
		t.Fatal(err)
	}
	before := diskHashes(t, s)
	stranger := seal(t, schema.KindLifecycleEvent, schema.LifecycleEvent{
		SubjectKind: "intent", SubjectID: id, Creation: canon.Hash([]byte("nothing")), FromState: "open", ToState: "closed",
		Actor: root.ID(), At: schema.Timestamp(fixedNow),
	}, root)
	cases := map[string][]envelope.Object{
		"unknown_entity": {stranger},
		"entity_exists":  {in},
	}
	for code, objs := range cases {
		if _, err := s.Do(ctx, op(code), members(objs...)); !apierr.HasCode(err, code) {
			t.Errorf("want %s, got %v", code, err)
		}
	}
	if !maps.Equal(before, diskHashes(t, s)) {
		t.Fatal("a rejected operation wrote records")
	}
	journals, _ := filepath.Glob(filepath.Join(s.Dir, "journal", "*"))
	if len(journals) != 0 {
		t.Fatalf("journals left: %v", journals)
	}
}

func TestConcurrentTransitionsFromOnePredecessor(t *testing.T) {
	s := newStore(t)
	id := schema.NewID(schema.PrefixIntent)
	in := intentRecord(t, id)
	if _, err := s.Do(ctx, op("create"), members(in)); err != nil {
		t.Fatal(err)
	}
	// Each writer prepared its event from the same observed head, the way
	// two clients racing would. Separate stores take the file lock like
	// separate processes.
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		w := reopen(t, s, Options{})
		ev := closeEvent(t, id, in.Hash, []string{"completed", "abandoned"}[i%2])
		wg.Go(func() {
			_, errs[i] = w.Do(ctx, op("close"), members(ev))
		})
	}
	wg.Wait()
	accepted := 0
	for _, err := range errs {
		switch {
		case err == nil:
			accepted++
		case apierr.HasCode(err, "stale_predecessor"), apierr.HasCode(err, "entity_exists"):
		default:
			t.Errorf("unexpected error %v", err)
		}
	}
	if accepted != 1 {
		t.Fatalf("%d transitions accepted, want exactly 1", accepted)
	}
	if sub := subject(t, s, id); sub.State != "closed" || sub.Conflict != "" {
		t.Fatalf("subject %+v", sub)
	}
}

func TestTruncatedObjectQuarantined(t *testing.T) {
	s := newStore(t)
	in := intentRecord(t, schema.NewID(schema.PrefixIntent))
	if _, err := s.Do(ctx, op("create"), members(in)); err != nil {
		t.Fatal(err)
	}
	path := s.ObjectPath(in.Hash)
	if err := os.WriteFile(path, in.Bytes[:len(in.Bytes)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := s.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Quarantined) != 1 || r.Quarantined[0] != in.Hash {
		t.Fatalf("quarantined %v", r.Quarantined)
	}
	q, _ := filepath.Glob(filepath.Join(s.Dir, "quarantine", in.Hash+".*.json"))
	if len(q) != 1 {
		t.Fatalf("quarantine files %v", q)
	}
	if _, err := s.Read(in.Hash); err == nil {
		t.Fatal("truncated record still readable")
	}
}

func TestReindexReproducesState(t *testing.T) {
	s := newStore(t)
	id := schema.NewID(schema.PrefixIntent)
	in := intentRecord(t, id)
	if _, err := s.Do(ctx, op("create"), members(in)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, op("close"), members(closeEvent(t, id, in.Hash, "completed"))); err != nil {
		t.Fatal(err)
	}
	before := digest(t, s)
	subjects, _ := s.Index.Subjects(ctx)
	if err := os.Remove(filepath.Join(s.Dir, "index.db")); err != nil {
		t.Fatal(err)
	}
	r, err := s.Reindex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Indexed != 3 || len(r.Pending) != 0 {
		t.Fatalf("report %+v", r)
	}
	if digest(t, s) != before {
		t.Fatal("reindex produced different durable state")
	}
	after, _ := s.Index.Subjects(ctx)
	if fmt.Sprint(after) != fmt.Sprint(subjects) {
		t.Fatalf("subjects differ:\n%v\n%v", subjects, after)
	}
}

func TestImportedForkIsConflictAndBlocks(t *testing.T) {
	s := newStore(t)
	id := schema.NewID(schema.PrefixIntent)
	in := intentRecord(t, id)
	if _, err := s.Do(ctx, op("create"), members(in)); err != nil {
		t.Fatal(err)
	}
	local := closeEvent(t, id, in.Hash, "completed")
	if _, err := s.Do(ctx, op("close"), members(local)); err != nil {
		t.Fatal(err)
	}
	arrive(t, s, externalGroup(t, closeEvent(t, id, in.Hash, "abandoned")))
	r, err := s.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0].ID != id || r.Conflicts[0].Kind != "lifecycle_fork" {
		t.Fatalf("conflicts %+v", r.Conflicts)
	}
	sub := subject(t, s, id)
	if sub.Conflict != "lifecycle_fork" || sub.State != "open" {
		t.Fatalf("subject %+v", sub)
	}
	ws := intentRecord(t, schema.NewID(schema.PrefixIntent))
	child := seal(t, schema.KindIntent, schema.Intent{
		ID: schema.NewID(schema.PrefixIntent), ParentID: &id, Description: "child",
		Criteria: []schema.Criterion{{ID: "c", Statement: "s", Verification: schema.Verification{Kind: "judgment", Reviewer: "any"}}},
		Source:   schema.Source{Kind: "intent", Reference: &id}, Owner: root.ID(), CreatedAt: schema.Timestamp(fixedNow),
	}, root)
	if _, err := s.Do(ctx, op("child"), members(ws, child)); !apierr.HasCode(err, "subject_conflict") {
		t.Fatalf("mutation of a conflicted subject: %v", err)
	}
	before := digest(t, s)
	if _, err := s.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	if digest(t, s) != before || subject(t, s, id).Conflict != "lifecycle_fork" {
		t.Fatal("conflict not reproduced by reindex")
	}
}

func TestImportedConflictingCreation(t *testing.T) {
	s := newStore(t)
	id := schema.NewID(schema.PrefixIntent)
	if _, err := s.Do(ctx, op("create"), members(intentRecord(t, id))); err != nil {
		t.Fatal(err)
	}
	other := seal(t, schema.KindIntent, schema.Intent{
		ID: id, Description: "a different intent with the same id",
		Criteria: []schema.Criterion{{ID: "c", Statement: "s", Verification: schema.Verification{Kind: "judgment", Reviewer: "any"}}},
		Source:   schema.Source{Kind: "human"}, Owner: root.ID(), CreatedAt: schema.Timestamp(fixedNow),
	}, root)
	arrive(t, s, externalGroup(t, other))
	r, err := s.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0].Kind != "conflicting_creation" || len(r.Conflicts[0].Detail.Creations) != 2 {
		t.Fatalf("conflicts %+v", r.Conflicts)
	}
}

func TestRecordsWithoutReceiptStayPending(t *testing.T) {
	s := newStore(t)
	id := schema.NewID(schema.PrefixIntent)
	orphan := intentRecord(t, id)
	if err := s.writeObject(orphan); err != nil {
		t.Fatal(err)
	}
	r, err := s.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Pending) != 1 || r.Pending[0] != orphan.Hash {
		t.Fatalf("pending %v", r.Pending)
	}
	if subject(t, s, id) != nil {
		t.Fatal("pending record became accepted state")
	}
}

func TestTamperedArrivedRecordIsNotAccepted(t *testing.T) {
	s := newStore(t)
	g := externalGroup(t, intentRecord(t, schema.NewID(schema.PrefixIntent)))
	forged := g.Members[0]
	forged.Envelope.Signature = g.Receipt.Envelope.Signature
	b, err := canonEnvelope(forged.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	f, err := envelope.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	arrive(t, s, externalGroup(t, f))
	r, err := s.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Unverifiable) != 1 || r.Unverifiable[0].Code != "signature_invalid" {
		t.Fatalf("unverifiable %+v", r.Unverifiable)
	}
}

func TestImportedMisattributedEventIgnored(t *testing.T) {
	s := newStore(t)
	id := schema.NewID(schema.PrefixIntent)
	in := intentRecord(t, id)
	if _, err := s.Do(ctx, op("create"), members(in)); err != nil {
		t.Fatal(err)
	}
	reason := "abandoned"
	forged := seal(t, schema.KindLifecycleEvent, schema.LifecycleEvent{
		SubjectKind: "intent", SubjectID: id, Creation: in.Hash, FromState: "open", ToState: "closed",
		Reason: &reason, Actor: "ses_" + strings.Repeat("u", 26), At: schema.Timestamp(fixedNow),
	}, root)
	arrive(t, s, externalGroup(t, forged))
	if _, err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	sub := subject(t, s, id)
	if sub.State != "open" || len(sub.Detail.Rejected) != 1 || sub.Detail.Rejected[0] != forged.Hash {
		t.Fatalf("subject %+v", sub)
	}
}

func TestPhase1StoreRefusesWrites(t *testing.T) {
	s, err := OpenPhase1(newNativeDir(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	in := intentRecord(t, schema.NewID(schema.PrefixIntent))
	writes := map[string]func() error{
		"do":      func() error { _, err := s.Do(ctx, op("create"), members(in)); return err },
		"import":  func() error { _, err := s.ImportRecords(ctx, []envelope.Object{in}); return err },
		"recover": func() error { _, err := s.Recover(ctx); return err },
		"reindex": func() error { _, err := s.Reindex(ctx); return err },
	}
	for name, write := range writes {
		if err := write(); !apierr.HasCode(err, "migrate_required") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if s.Native() || len(diskHashes(t, s)) != 0 {
		t.Fatal("Phase 1 store was written")
	}
}
