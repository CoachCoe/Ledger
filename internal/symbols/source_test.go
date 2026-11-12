package symbols

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/store"
)

// The store and Git sources describe the same trees identically.
func TestStoreSourceMatchesGit(t *testing.T) {
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
	files := []string{"src/a.ts", "src/b.ts", "src/big.ts"}

	dir := t.TempDir()
	if _, err := gitx.Run(ctx, dir, nil, nil, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	repo, err := gitx.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	gitTree := func(content map[string]string) string {
		var entries []gitx.TreeEntry
		for p, c := range content {
			f := filepath.Join(t.TempDir(), "f")
			if err := os.WriteFile(f, []byte(c), 0o600); err != nil {
				t.Fatal(err)
			}
			blobs, err := repo.WriteBlobs(ctx, []string{f})
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, gitx.TreeEntry{Path: p, Blob: blobs[0]})
		}
		tree, err := repo.BuildTree(ctx, filepath.Join(t.TempDir(), "index"), "", entries)
		if err != nil {
			t.Fatal(err)
		}
		return tree
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "objects"), true)
	if err != nil {
		t.Fatal(err)
	}
	storeTree := func(content map[string]string) string {
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

	var e1, e2 Engine
	fromGit, err := e1.Compare(ctx, GitSource{Repo: repo}, gitTree(base), gitTree(candidate), files, nil)
	if err != nil {
		t.Fatal(err)
	}
	fromStore, err := e2.Compare(ctx, StoreSource{Store: s}, storeTree(base), storeTree(candidate), files, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fromStore.Fallbacks, Fallback{Path: "src/big.ts", Reason: "source_too_large"}) {
		t.Fatalf("oversized file analyzed: %+v", fromStore.Fallbacks)
	}
	if len(fromGit.Written) == 0 || len(fromGit.Read) == 0 {
		t.Fatalf("comparison found nothing: %+v", fromGit)
	}
	if !reflect.DeepEqual(fromGit, fromStore) {
		t.Fatalf("sources disagree:\n git   %+v\n store %+v", fromGit, fromStore)
	}
}
