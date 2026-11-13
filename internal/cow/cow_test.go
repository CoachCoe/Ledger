package cow

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/oplog"
	"github.com/CoachCoe/Ledger/internal/store"
)

func wsp(n int) string { return fmt.Sprintf("wsp_%026d", n) }

type fixture struct {
	m     *Manager
	human string
	base  string
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func setup(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	human := filepath.Join(dir, "repo")
	writeFiles(t, human, map[string]string{
		".gitignore":                      "node_modules/\ndist/\n*.log\n",
		"package.json":                    `{"name":"app","dependencies":{"left-pad":"1.0.0"}}`,
		"package-lock.json":               `{"lockfileVersion":3}`,
		"src/index.ts":                    "export const a = 1\n",
		"src/billing/invoice.ts":          "export function total() { return 1 }\n",
		"README.md":                       "# app\n",
		"node_modules/left-pad/index.js":  "module.exports = s => s\n",
		"node_modules/.package-lock.json": "{}",
		"dist/index.js":                   "built\n",
	})
	s, err := store.Open(filepath.Join(dir, "store"), true)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{Store: s, Runtime: filepath.Join(dir, "runtime"), Logs: filepath.Join(dir, "oplog"), CaseSensitive: true, Human: human})
	if err != nil {
		t.Fatal(err)
	}
	// Shared dependency copies are read-only, so the temporary directory
	// can only be deleted once removing the last workspace has pruned them.
	t.Cleanup(func() {
		entries, _ := os.ReadDir(filepath.Join(dir, "runtime", "workspaces"))
		for _, e := range entries {
			if err := m.Remove(e.Name()); err != nil {
				t.Error(err)
			}
		}
	})
	base, err := m.Import(human)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{m: m, human: human, base: base}
}

func TestImportHonorsIgnoreRules(t *testing.T) {
	f := setup(t)
	files, err := f.m.cfg.Store.Files(f.base)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Path == "dist/index.js" || file.Path == "node_modules/left-pad/index.js" {
			t.Errorf("ignored %s was imported", file.Path)
		}
	}
	if len(files) != 6 {
		t.Fatalf("imported %d files, want 6", len(files))
	}
}

func TestSnapshotRecordsOnlyChanges(t *testing.T) {
	f := setup(t)
	id := wsp(1)
	if _, err := f.m.Create(id, f.base, nil); err != nil {
		t.Fatal(err)
	}
	ws := f.m.Path(id)
	if _, _, appended, err := f.m.Snapshot(id, "poll", nil); err != nil || appended {
		t.Fatalf("unchanged clone appended=%v err=%v", appended, err)
	}
	// Past the racy-timestamp window of the cache the clone inherited.
	time.Sleep(10 * time.Millisecond)
	writeFiles(t, ws, map[string]string{"src/index.ts": "export const a = 2\n", "debug.log": "ignored\n"})
	e, _, appended, err := f.m.Snapshot(id, "watch", nil)
	if err != nil || !appended || e.Tree == f.base {
		t.Fatalf("edit not recorded: %v %v", appended, err)
	}
	prev, err := loadCache(f.m.cachePath(id))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	res, err := scan(ws, f.m.rules(ws), nil, f.m.depPath(""), prev, f.m.cfg.Store.Batch(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.rehashed != 0 {
		t.Fatalf("an unchanged workspace rehashed %d files", res.rehashed)
	}
}

func TestTrackedFilesStayTrackedWhenIgnored(t *testing.T) {
	f := setup(t)
	id := wsp(2)
	if _, err := f.m.Create(id, f.base, nil); err != nil {
		t.Fatal(err)
	}
	ws := f.m.Path(id)
	writeFiles(t, ws, map[string]string{".gitignore": "node_modules/\ndist/\n*.log\nREADME.md\n"})
	e, _, _, err := f.m.Snapshot(id, "watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	files, err := f.m.cfg.Store.Files(e.Tree)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range files {
		found = found || file.Path == "README.md"
	}
	if !found {
		t.Fatal("a tracked file vanished when an ignore rule matched it")
	}
}

// P2-AC-012
func TestRestoreReproducesEarlierTreeAndKeepsLog(t *testing.T) {
	f := setup(t)
	id := wsp(3)
	if _, err := f.m.Create(id, f.base, nil); err != nil {
		t.Fatal(err)
	}
	ws := f.m.Path(id)
	var hashes, trees []string
	for i, edit := range []func(){
		func() { writeFiles(t, ws, map[string]string{"src/new/deep/file.ts": "new\n"}) },
		func() {
			_ = os.Remove(filepath.Join(ws, "README.md"))
			writeFiles(t, ws, map[string]string{"src/index.ts": "changed\n"})
		},
		func() {
			_ = os.RemoveAll(filepath.Join(ws, "src", "new"))
			writeFiles(t, ws, map[string]string{"README.md/now-a-dir": "x\n", "dist/out.js": "ignored output\n"})
		},
	} {
		edit()
		e, h, ok, err := f.m.Snapshot(id, "watch", nil)
		if err != nil || !ok {
			t.Fatalf("edit %d: %v %v", i, ok, err)
		}
		hashes, trees = append(hashes, h), append(trees, e.Tree)
	}
	for _, i := range []int{0, 2, 1, 0} {
		e, _, err := f.m.Restore(id, hashes[i], nil)
		if err != nil {
			t.Fatalf("restore to %d: %v", i, err)
		}
		if e.Tree != trees[i] || e.Kind != oplog.Restore || *e.Source != hashes[i] {
			t.Fatalf("restore to %d produced %+v", i, e)
		}
		tree, _, err := f.m.capture(id)
		if err != nil || tree != trees[i] {
			t.Fatalf("workspace after restore to %d scans as %s (%v)", i, tree, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "dist", "out.js")); err != nil {
		t.Fatalf("restore removed an ignored file: %v", err)
	}
	entries, err := f.m.Log(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oplog.Chain(entries); err != nil {
		t.Fatal(err)
	}
	for _, h := range hashes {
		if _, ok := f.m.logs[id].Get(h); !ok {
			t.Fatalf("restore lost entry %s", h)
		}
	}
	if _, _, err := f.m.Restore(id, "nope", nil); !apierr.HasCode(err, "operation_unknown") {
		t.Fatalf("restore to unknown entry: %v", err)
	}
}

// P2-AC-016, at unit scale; the NestJS run is in tools/workspacebench.
func TestDependencyTreesAreSharedReadOnly(t *testing.T) {
	f := setup(t)
	a, b, c := wsp(4), wsp(5), wsp(6)
	for _, id := range []string{a, b, c} {
		if _, err := f.m.Create(id, f.base, nil); err != nil {
			t.Fatal(err)
		}
	}
	link := func(id string) string { return filepath.Join(f.m.Path(id), "node_modules") }
	sharedOf := func(id string) string {
		raw, err := os.ReadFile(filepath.Join(link(id), sharedMarker))
		if err != nil {
			return ""
		}
		return string(raw)
	}
	if sharedOf(a) == "" || sharedOf(a) != sharedOf(b) {
		t.Fatalf("workspaces do not share one copy: %q %q", sharedOf(a), sharedOf(b))
	}
	if got, err := os.ReadFile(filepath.Join(link(a), "left-pad", "index.js")); err != nil || len(got) == 0 {
		t.Fatalf("shared dependency unreadable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(link(a), "left-pad", "index.js"), []byte("hacked"), 0o644); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("write into the shared tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(link(a), "left-pad", "new.js"), []byte("x"), 0o644); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("create in the shared tree: %v", err)
	}
	// Tools write caches at the top of node_modules; that stays private.
	writeFiles(t, link(a), map[string]string{".vite-temp/config.mjs": "cache"})
	if _, err := os.Stat(filepath.Join(link(b), ".vite-temp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a's cache is visible in b: %v", err)
	}
	if _, _, appended, err := f.m.Snapshot(a, "watch", nil); err != nil || appended {
		t.Fatalf("dependency writes reached the snapshot: %v %v", appended, err)
	}

	// An observed manifest change detaches before the snapshot is recorded.
	writeFiles(t, f.m.Path(a), map[string]string{"package.json": `{"name":"app","dependencies":{"left-pad":"2.0.0"}}`})
	if _, _, _, err := f.m.Snapshot(a, "watch", nil); err != nil {
		t.Fatal(err)
	}
	if sharedOf(a) != "" {
		t.Fatal("workspace a still links the shared tree")
	}
	if got, _ := os.ReadFile(filepath.Join(link(a), ".vite-temp", "config.mjs")); string(got) != "cache" {
		t.Fatal("detach lost what tools wrote into node_modules")
	}
	if err := os.WriteFile(filepath.Join(link(a), "left-pad", "index.js"), []byte("v2"), 0o644); err != nil {
		t.Fatalf("private copy is not writable: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(link(b), "left-pad", "index.js")); string(got) != "module.exports = s => s\n" {
		t.Fatalf("workspace b saw a's install: %q", got)
	}

	// An explicit request detaches without any manifest change.
	if err := f.m.Detach(c, "node_modules"); err != nil {
		t.Fatal(err)
	}
	if sharedOf(c) != "" {
		t.Fatal("explicit detach left a link")
	}
	if err := os.WriteFile(filepath.Join(link(c), "left-pad", "index.js"), []byte("c"), 0o644); err != nil {
		t.Fatalf("detached copy is not writable: %v", err)
	}
	if sharedOf(b) == "" {
		t.Fatal("detaching c changed b")
	}

	// Different manifests in the default workspace share nothing.
	writeFiles(t, f.human, map[string]string{"package-lock.json": `{"lockfileVersion":3,"changed":true}`})
	other, err := f.m.Import(f.human)
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, f.human, map[string]string{"package-lock.json": `{"lockfileVersion":3}`})
	if _, err := f.m.Create(wsp(7), other, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link(wsp(7))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("workspace with unmatched manifests got dependencies: %v", err)
	}
}

func TestUnlinkedDependencyTreesArePruned(t *testing.T) {
	f := setup(t)
	a, b, c := wsp(8), wsp(9), wsp(10)
	for _, id := range []string{a, b, c} {
		if _, err := f.m.Create(id, f.base, nil); err != nil {
			t.Fatal(err)
		}
	}
	copies := func() int {
		entries, err := os.ReadDir(f.m.depPath(""))
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	if copies() != 1 {
		t.Fatalf("%d shared copies, want 1", copies())
	}
	if err := f.m.Remove(a); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Detach(b, ""); err != nil {
		t.Fatal(err)
	}
	if copies() != 1 {
		t.Fatal("a copy c still links was pruned")
	}
	if got, err := os.ReadFile(filepath.Join(f.m.Path(c), "node_modules", "left-pad", "index.js")); err != nil || len(got) == 0 {
		t.Fatalf("c lost its dependencies: %v", err)
	}
	if err := f.m.Remove(c); err != nil {
		t.Fatal(err)
	}
	if copies() != 0 {
		t.Fatal("an unlinked copy was kept")
	}
	if err := f.m.Remove(b); err != nil {
		t.Fatal(err)
	}
	// Nothing read-only is left, so an ordinary recursive delete succeeds.
	if err := os.RemoveAll(f.m.cfg.Runtime); err != nil {
		t.Fatal(err)
	}
}

func TestManyWorkspacesFromOneBase(t *testing.T) {
	f := setup(t)
	start := time.Now()
	for i := range 20 {
		if _, err := f.m.Create(wsp(100+i), f.base, nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("20 workspaces in %v", time.Since(start))
	if _, err := f.m.Create(wsp(100), f.base, nil); !apierr.HasCode(err, "workspace_exists") {
		t.Fatalf("second create: %v", err)
	}
	if err := f.m.Remove(wsp(100)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.m.Path(wsp(100))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("remove left the checkout")
	}
}

func TestProposeRecordsADurableEntry(t *testing.T) {
	f := setup(t)
	id := wsp(1)
	if _, err := f.m.Create(id, f.base, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	writeFiles(t, f.m.Path(id), map[string]string{"src/index.ts": "export const a = 3\n"})
	e, h, err := f.m.Propose(id, nil)
	if err != nil || e.Kind != oplog.Propose || e.Tree == f.base || e.Base != f.base {
		t.Fatalf("propose %+v, %v", e, err)
	}
	// Proposing the same tree again still records the proposal.
	again, h2, err := f.m.Propose(id, nil)
	if err != nil || again.Tree != e.Tree || h2 == h {
		t.Fatalf("second propose %+v, %v", again, err)
	}
	entries, err := f.m.Log(id)
	if err != nil || len(entries) != 3 {
		t.Fatalf("%d log entries, %v", len(entries), err)
	}
}

func TestCreateRebuildsAnOrphanedCheckout(t *testing.T) {
	f := setup(t)
	id := wsp(1)
	writeFiles(t, f.m.Path(id), map[string]string{"half-cloned.txt": "x"})
	if _, err := f.m.Create(id, f.base, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.m.Path(id), "half-cloned.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("orphaned checkout kept: %v", err)
	}
	if _, err := f.m.Create(id, f.base, nil); !apierr.HasCode(err, "workspace_exists") {
		t.Fatalf("second create: %v", err)
	}
}

func TestIgnoredFilesSkipDependencyTrees(t *testing.T) {
	f := setup(t)
	id := wsp(1)
	if _, err := f.m.Create(id, f.base, nil); err != nil {
		t.Fatal(err)
	}
	ws := f.m.Path(id)
	if got, err := f.m.IgnoredFiles(id); err != nil || len(got) != 0 {
		t.Fatalf("fresh workspace has ignored files %v, %v", got, err)
	}
	writeFiles(t, ws, map[string]string{"dist/out.js": "built\n", "debug.log": "x", "src/index.ts": "export const a = 4\n"})
	if err := f.m.Detach(id, ""); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, ws, map[string]string{"node_modules/extra/index.js": "x"})
	got, err := f.m.IgnoredFiles(id)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[debug.log dist/out.js]" {
		t.Fatalf("ignored files %v", got)
	}
}

func TestCheckoutTrackedTreeCountsOnlyTrackedFiles(t *testing.T) {
	f := setup(t)
	dir := filepath.Join(t.TempDir(), "check")
	if err := f.m.Checkout(f.base, dir); err != nil {
		t.Fatal(err)
	}
	tree := func() string {
		t.Helper()
		got, err := f.m.TrackedTree(dir, f.base)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := tree(); got != f.base {
		t.Fatalf("fresh checkout scans as %s, want %s", got, f.base)
	}
	writeFiles(t, dir, map[string]string{"coverage/out.txt": "built\n", "dist/more.js": "x\n"})
	if got := tree(); got != f.base {
		t.Fatalf("new files changed the tracked tree: %s", got)
	}
	writeFiles(t, dir, map[string]string{"src/index.ts": "export const a = 2\n"})
	if got := tree(); got == f.base {
		t.Fatal("an edited tracked file did not change the tree")
	}
	writeFiles(t, dir, map[string]string{"src/index.ts": "export const a = 1\n"})
	if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	if got := tree(); got == f.base {
		t.Fatal("a deleted tracked file did not change the tree")
	}
}
