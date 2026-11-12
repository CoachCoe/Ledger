package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/cow"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/store"
)

func nativeFixture(t *testing.T) (NativeProvider, string) {
	t.Helper()
	dir := t.TempDir()
	human := filepath.Join(dir, "repo")
	for p, c := range map[string]string{
		".gitignore":   "*.log\nnode_modules/\n",
		"package.json": `{"name":"app"}`,
		"src/app.ts":   "export const a = 1\n",
		"src/safe.ts":  "export const b = 1\n",
	} {
		full := filepath.Join(human, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../../outside", filepath.Join(human, "src", "escape")); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, "store"), true)
	if err != nil {
		t.Fatal(err)
	}
	m, err := cow.New(cow.Config{Store: s, Runtime: filepath.Join(dir, "runtime"), Logs: filepath.Join(dir, "oplog"), CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	base, err := m.Import(human)
	if err != nil {
		t.Fatal(err)
	}
	return NativeProvider{Workspaces: m, Store: s}, base
}

func TestNativeProviderLifecycle(t *testing.T) {
	ctx := context.Background()
	p, base := nativeFixture(t)
	w := schema.Workspace{ID: "wsp_" + "a0000000000000000000000000", BaseTree: base}
	create := record.Effect{Kind: record.EffectWorkspaceCreate, WorkspaceID: w.ID, Path: p.Path(w.ID), Base: base, Session: "ses_" + "a0000000000000000000000000"}
	if err := p.Apply(ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Path(w.ID), "src", "app.ts"), []byte("export const a = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A retried creation keeps the workspace and its edits.
	if err := p.Apply(ctx, create); err != nil {
		t.Fatal(err)
	}
	snap, err := p.Snapshot(ctx, w, "")
	if err != nil || len(snap.Files) != 1 || snap.Status["src/app.ts"] != "M" || snap.OpHead == "" {
		t.Fatalf("snapshot %+v, %v", snap, err)
	}
	prop, err := p.StableSnapshot(ctx, w, "", "ses_"+"a0000000000000000000000000")
	if err != nil || prop.Tree != snap.Tree || prop.OpHead == snap.OpHead {
		t.Fatalf("proposal %+v, %v", prop, err)
	}
	if _, err := p.WriteText(ctx, w, "package.json", `{"name":"app","version":"2"}`, nil); !apierr.HasCode(err, "content_conflict") {
		t.Fatalf("write without the current hash: %v", err)
	}
	if err := os.WriteFile(filepath.Join(p.Path(w.ID), "debug.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	remove := record.Effect{Kind: record.EffectWorkspaceRemove, WorkspaceID: w.ID, Path: p.Path(w.ID), After: true}
	if err := p.Apply(ctx, remove); !apierr.HasCode(err, "workspace_dirty") {
		t.Fatalf("removal with an ignored file: %v", err)
	}
	if err := os.Remove(filepath.Join(p.Path(w.ID), "debug.log")); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, remove); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Path(w.ID)); !os.IsNotExist(err) {
		t.Fatalf("workspace still present: %v", err)
	}
	if err := p.Apply(ctx, remove); err != nil {
		t.Fatalf("repeated removal: %v", err)
	}
	if err := p.Apply(ctx, record.Effect{Kind: record.EffectRef, Ref: "refs/heads/ledger/x"}); !apierr.HasCode(err, "invalid_effect") {
		t.Fatalf("ref effect: %v", err)
	}
}

func TestNativeViolationsReadStoreTrees(t *testing.T) {
	ctx := context.Background()
	p, base := nativeFixture(t)
	chain := []schema.Capability{{Allow: []string{"src/**"}, Deny: []string{}, CreatedAt: "2000-01-01T00:00:00Z", ExpiresAt: "2999-01-01T00:00:00Z"}}
	snap := Snapshot{Tree: base, Files: []string{"src/safe.ts", "src/escape"}}
	bad, err := Violations(ctx, p.Trees(), base, snap, chain, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if bad["src/safe.ts"] != nil || bad["src/escape"] == nil {
		t.Fatalf("violations %v", bad)
	}
}
