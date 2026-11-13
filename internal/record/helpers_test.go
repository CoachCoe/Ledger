package record

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/schema"
)

var (
	ctx      = context.Background()
	rootKey  = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	root     = identity.RootSigner(rootKey)
	fixedNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
)

// newStore returns a store whose root key is already published.
func newStore(t testing.TB) *Store {
	t.Helper()
	s, _ := newNative(t)
	return s
}

// reopen opens s's repository again, as another process would.
func reopen(t testing.TB, s *Store, opts Options) *Store {
	t.Helper()
	return openNative(t, filepath.Dir(s.Dir), opts)
}

func publishRootKey(t testing.TB, s *Store) {
	t.Helper()
	rk := seal(t, schema.KindRootKey, schema.RootKey{
		PublicKey: identity.EncodePublicKey(root.PublicKey()), CreatedAt: schema.Timestamp(fixedNow),
	}, root)
	if _, err := s.Do(ctx, op("init"), members(rk)); err != nil {
		t.Fatal(err)
	}
}

func seal(t testing.TB, kind schema.Kind, payload any, s identity.Signer) envelope.Object {
	t.Helper()
	o, err := envelope.Seal(kind, payload, s)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func members(objs ...envelope.Object) BuildFunc {
	return func(*index.Index) (any, []envelope.Object, error) {
		hashes := []string{}
		for _, o := range objs {
			hashes = append(hashes, o.Hash)
		}
		return map[string]any{"records": hashes}, objs, nil
	}
}

func op(name string) Op {
	return Op{ID: schema.NewID(schema.PrefixOperation), Name: "test." + strings.ReplaceAll(name, "-", "_"), Digest: canon.Hash([]byte(name)), Signer: root}
}

func intentRecord(t testing.TB, id string) envelope.Object {
	t.Helper()
	return seal(t, schema.KindIntent, schema.Intent{
		ID: id, Description: "intent " + id,
		Criteria: []schema.Criterion{{ID: "review", Statement: "reviewed",
			Verification: schema.Verification{Kind: "judgment", Reviewer: "human"}}},
		Source: schema.Source{Kind: "human"}, Owner: root.ID(), CreatedAt: schema.Timestamp(fixedNow),
	}, root)
}

func closeEvent(t testing.TB, id, creation, reason string) envelope.Object {
	t.Helper()
	return seal(t, schema.KindLifecycleEvent, schema.LifecycleEvent{
		SubjectKind: "intent", SubjectID: id, Creation: creation,
		FromState: "open", ToState: "closed", Reason: &reason, Actor: root.ID(), At: schema.Timestamp(fixedNow),
	}, root)
}

// arrive writes g's objects to .ledger/objects without indexing or
// logging them, as records copied in by another tool would be.
func arrive(t testing.TB, s *Store, g index.Group) {
	t.Helper()
	for _, o := range append(slices.Clone(g.Members), g.Receipt) {
		if err := s.writeObject(o); err != nil {
			t.Fatal(err)
		}
	}
}

// externalGroup seals a receipt over objs as another process would.
func externalGroup(t testing.TB, objs ...envelope.Object) index.Group {
	t.Helper()
	hashes := []string{}
	for _, o := range objs {
		hashes = append(hashes, o.Hash)
	}
	receipt := seal(t, schema.KindOperationReceipt, schema.OperationReceipt{
		OperationID: schema.NewID(schema.PrefixOperation), Operation: "test.external",
		RequestDigest: canon.Hash([]byte("external")), Result: []byte(`{}`), Members: hashes,
		At: schema.Timestamp(fixedNow),
	}, root)
	return index.Group{Receipt: receipt, Members: objs}
}

func diskHashes(t testing.TB, s *Store) map[string]bool {
	t.Helper()
	hs, err := s.listObjects()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, h := range hs {
		out[h] = true
	}
	return out
}

func subject(t testing.TB, s *Store, id string) *index.Subject {
	t.Helper()
	sub, err := s.Index.Subject(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func digest(t testing.TB, s *Store) string {
	t.Helper()
	d, err := s.Index.Digest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func canonEnvelope(env schema.Envelope) ([]byte, error) { return canon.Marshal(env) }
