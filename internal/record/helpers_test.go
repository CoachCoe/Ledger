package record

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/gitx"
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

func newRepo(t testing.TB) *gitx.Repo {
	t.Helper()
	dir := t.TempDir()
	env := []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
	if _, err := gitx.Run(ctx, dir, nil, env, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, DirName), 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := gitx.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func openStore(t testing.TB, repo *gitx.Repo, opts Options) *Store {
	t.Helper()
	if opts.Now == nil {
		opts.Now = func() time.Time { return fixedNow }
	}
	s, err := Open(ctx, repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newStore returns a store whose root key is already published.
func newStore(t testing.TB) *Store {
	t.Helper()
	s := openStore(t, newRepo(t), Options{})
	publishRootKey(t, s)
	return s
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

// pushToRef commits g's objects onto the records ref as a fetch would,
// without touching .ledger/objects.
func pushToRef(t testing.TB, s *Store, g index.Group) string {
	t.Helper()
	tip, err := s.Repo.ResolveRef(ctx, RecordsRef)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var entries []gitx.TreeEntry
	for _, o := range append(g.Members, g.Receipt) {
		p := filepath.Join(dir, o.Hash)
		if err := os.WriteFile(p, o.Bytes, 0o600); err != nil {
			t.Fatal(err)
		}
		blobs, err := s.Repo.WriteBlobs(ctx, []string{p})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, gitx.TreeEntry{Path: refPath(o.Hash), Blob: blobs[0]})
	}
	tree, err := s.Repo.BuildTree(ctx, filepath.Join(dir, "index"), tip, entries)
	if err != nil {
		t.Fatal(err)
	}
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	commit, err := s.Repo.CommitTree(ctx, tree, parents, "external\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Repo.UpdateRefCAS(ctx, RecordsRef, commit, tip); err != nil {
		t.Fatal(err)
	}
	return commit
}

// refHashes returns the object hashes in the records ref.
func refHashes(t testing.TB, s *Store) map[string]bool {
	t.Helper()
	tip, err := s.Repo.ResolveRef(ctx, RecordsRef)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	if tip == "" {
		return out
	}
	entries, err := s.Repo.ListTree(ctx, tip)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		h, ok := hashFromRefPath(e.Path)
		if !ok {
			t.Fatalf("bad ref path %s", e.Path)
		}
		out[h] = true
	}
	return out
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

func newRepoAt(t testing.TB, dir string) *gitx.Repo {
	t.Helper()
	r, err := gitx.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func canonEnvelope(env schema.Envelope) ([]byte, error) { return canon.Marshal(env) }
