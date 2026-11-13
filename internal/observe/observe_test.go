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
		if !filter.Ignored(ctx, path) {
			t.Fatal("ignored dependency event was not filtered")
		}
	}
	os.WriteFile(filepath.Join(root, ".gitignore"), nil, 0600)
	filter.Ignored(ctx, filepath.Join(root, ".gitignore"))
	if filter.Ignored(ctx, path) {
		t.Fatal("changed ignore rules were cached forever")
	}
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0600)
	run("add", "-f", "node_modules/a.js")
	filter, err = newFilter(root)
	if err != nil {
		t.Fatal(err)
	}
	if filter.Ignored(ctx, path) {
		t.Fatal("tracked ignored file event was dropped")
	}
}

func TestRulesFilterFollowsIgnoreFilesWithoutGit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write := func(rel, body string) string {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write(".gitignore", "node_modules/\n")
	dep := write("node_modules/a.js", "x\n")
	src := write("src/a.ts", "x\n")
	state := write(".ledger/index.db", "")
	filter, err := RulesFilter(true)(root)
	if err != nil {
		t.Fatal(err)
	}
	if !filter.Ignored(ctx, dep) || !filter.Ignored(ctx, state) || filter.Ignored(ctx, src) {
		t.Fatal("rules not followed")
	}
	if filter.Ignored(ctx, write(".ledgerignore", "src/\n")) || !filter.Ignored(ctx, src) {
		t.Fatal("a changed rule file was not reread")
	}
}
