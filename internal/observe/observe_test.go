package observe

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/CoachCoe/Ledger/internal/gitx"
)

func TestIgnoredDependencyFilterInvalidatesOnRulesAndKeepsTrackedFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if _, err := gitx.Run(ctx, root, nil, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0600)
	run("add", ".gitignore")
	os.Mkdir(filepath.Join(root, "node_modules"), 0700)
	path := filepath.Join(root, "node_modules", "a.js")
	os.WriteFile(path, []byte("x\n"), 0600)
	filter, err := newFilter(root)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if !filter.ignored(ctx, path) {
			t.Fatal("ignored dependency event was not filtered")
		}
	}
	os.WriteFile(filepath.Join(root, ".gitignore"), nil, 0600)
	filter.ignored(ctx, filepath.Join(root, ".gitignore"))
	if filter.ignored(ctx, path) {
		t.Fatal("changed ignore rules were cached forever")
	}
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0600)
	run("add", "-f", "node_modules/a.js")
	filter, err = newFilter(root)
	if err != nil {
		t.Fatal(err)
	}
	if filter.ignored(ctx, path) {
		t.Fatal("tracked ignored file event was dropped")
	}
}
