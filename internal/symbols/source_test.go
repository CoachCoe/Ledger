package symbols

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "objects"), true)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func storeTree(t *testing.T, s *store.Store, content map[string]string) string {
	t.Helper()
	b := s.Batch()
	var list []store.File
	for p, c := range content {
		id, err := b.PutFile([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, store.File{Path: p, Type: store.TypeFile, ID: id})
	}
	tree, err := b.BuildTree(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestStoreSourceCompares(t *testing.T) {
	ctx := context.Background()
	base := map[string]string{
		"src/a.ts":  "export function f() { return 1 }\n",
		"README.md": "notes\n",
	}
	candidate := map[string]string{
		"src/a.ts":   "export function f() { return 2 }\n",
		"src/b.ts":   "import { f } from './a'\nexport function g() { return f() }\n",
		"README.md":  "notes\n",
		"src/big.ts": strings.Repeat("// x\n", (maxSource/5)+1),
	}
	s := newStore(t)
	var e Engine
	got, err := e.Compare(ctx, StoreSource{Store: s}, storeTree(t, s, base), storeTree(t, s, candidate), []string{"src/a.ts", "src/b.ts", "src/big.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Fallbacks, Fallback{Path: "src/big.ts", Reason: "source_too_large"}) {
		t.Fatalf("oversized file analyzed: %+v", got.Fallbacks)
	}
	if len(got.Written) == 0 || len(got.Read) == 0 {
		t.Fatalf("comparison found nothing: %+v", got)
	}
}

// Engine.Maps gives the maps MapFile computes from the bytes, so stored
// maps recompute exactly (P2-AC-005).
func TestEngineMapsMatchMapFile(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	content := map[string]string{
		"src/a.ts":   "export function f() { return 1 }\n",
		"src/b.py":   "def g():\n    return 2\n",
		"README.md":  "notes\n",
		"src/bad.ts": "export function (\n",
	}
	b := s.Batch()
	var list []store.File
	for p, c := range content {
		id, err := b.PutFile([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, store.File{Path: p, Type: store.TypeFile, ID: id})
	}
	tree, err := b.BuildTree(list)
	if err != nil {
		t.Fatal(err)
	}
	var e Engine
	got, err := e.Maps(ctx, StoreSource{Store: s}, tree)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["README.md"]; ok || len(got) != 3 {
		t.Fatalf("maps for %v", slices.Sorted(maps.Keys(got)))
	}
	ids := []string{}
	for _, f := range list {
		want, ok, err := MapFile(f.Path, f.ID, []byte(content[f.Path]))
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			continue
		}
		id, err := got[f.Path].Put(b)
		if err != nil {
			t.Fatal(err)
		}
		wantID, err := want.Put(b)
		if err != nil || id != wantID {
			t.Fatalf("%s: map %s, MapFile %s, %v", f.Path, id, wantID, err)
		}
		ids = append(ids, id)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if same, err := Recompute(s, id); err != nil || !same {
			t.Fatalf("%s does not recompute: %v", id, err)
		}
	}
}
