package symbols

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/gitx"
)

func TestNormalizedPathAliasesFallBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := gitx.Run(ctx, dir, nil, nil, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	repo, err := gitx.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("function f() {}"), 0600); err != nil {
		t.Fatal(err)
	}
	blobs, err := repo.WriteBlobs(ctx, []string{source})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repo.BuildTree(ctx, filepath.Join(t.TempDir(), "index"), "", []gitx.TreeEntry{{Path: "café.ts", Blob: blobs[0]}, {Path: "cafe\u0301.ts", Blob: blobs[0]}})
	if err != nil {
		t.Fatal(err)
	}
	var engine Engine
	project, err := engine.load(ctx, repo, tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(project) != 1 || project["café.ts"].Fallback != "ambiguous_path" {
		t.Fatalf("normalized aliases silently overwritten: %+v", project)
	}
}

func TestOversizeSourceKeepsOtherFilesAnalyzable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := gitx.Run(ctx, dir, nil, nil, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	repo, err := gitx.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	files := []string{filepath.Join(t.TempDir(), "large"), filepath.Join(t.TempDir(), "small")}
	for i, src := range []string{strings.Repeat("x", (4<<20)+1), "function f() {}"} {
		if err := os.WriteFile(files[i], []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
	}
	blobs, err := repo.WriteBlobs(ctx, files)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repo.BuildTree(ctx, filepath.Join(t.TempDir(), "index"), "", []gitx.TreeEntry{{Path: "large.ts", Blob: blobs[0]}, {Path: "small.ts", Blob: blobs[1]}})
	if err != nil {
		t.Fatal(err)
	}
	var engine Engine
	project, err := engine.load(ctx, repo, tree)
	if err != nil {
		t.Fatal(err)
	}
	if project["large.ts"].Fallback != "source_too_large" || len(project["small.ts"].Declarations) == 0 {
		t.Fatal(project)
	}
}
