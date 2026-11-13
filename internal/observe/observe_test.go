package observe

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

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
