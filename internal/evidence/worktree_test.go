package evidence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/cow"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/store"
)

const testManifest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type wtFixture struct {
	m          *cow.Manager
	co         Checkouts
	runtimeDir string
	base       string
	b          Binding
}

// newWT stores a base tree and a proposal tree that changes a.txt, and
// binds the proposal.
func newWT(t *testing.T) *wtFixture {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), ".ledger")
	objects, err := store.Open(filepath.Join(ledger, "store", "objects"), true)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(ledger, "runtime")
	m, err := cow.New(cow.Config{Store: objects, Runtime: runtimeDir, Logs: filepath.Join(ledger, "oplog"), CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "one\n")
	write("run.sh", "echo hi\n")
	base, err := m.Import(src)
	if err != nil {
		t.Fatal(err)
	}
	write("a.txt", "two\n")
	tree, err := m.Import(src)
	if err != nil {
		t.Fatal(err)
	}
	return &wtFixture{m: m, co: NativeCheckouts{Workspaces: m}, runtimeDir: runtimeDir, base: base,
		b: Binding{Manifest: testManifest, Tree: tree, BaseTree: base}}
}

// author checks the base tree out beside the verification checkouts, as
// another workspace a user is editing.
func (f *wtFixture) author(t *testing.T) string {
	t.Helper()
	path := filepath.Join(f.runtimeDir, "workspaces", "author")
	if err := f.m.Checkout(f.base, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func runID() string { return schema.NewID(schema.PrefixRun) }

func (f *wtFixture) expectAbort(t *testing.T, id, code string, err error) {
	t.Helper()
	if !apierr.HasCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
	path := VerifyPath(f.runtimeDir, id)
	if exists(path) {
		t.Fatalf("checkout %s remains after abort", path)
	}
	j, jerr := ReadJournal(f.runtimeDir, id)
	if jerr != nil || j.State != StateAborted || j.Reason != code {
		t.Fatalf("journal %+v %v", j, jerr)
	}
}

func TestAC090_091_111_VerificationWorktree(t *testing.T) {
	f := newWT(t)
	ctx := context.Background()
	// An author workspace at the base being edited.
	author := f.author(t)
	if err := os.WriteFile(filepath.Join(author, "a.txt"), []byte("in progress\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := runID()
	w, err := Prepare(ctx, f.co, f.runtimeDir, f.b, Journal{RunID: id}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(f.runtimeDir, "verify", id); w.Path != want {
		t.Fatalf("path %s want %s", w.Path, want)
	}
	if tracked, err := w.TrackedTree(ctx); err != nil || tracked != f.b.Tree {
		t.Fatalf("tracked tree %s %v", tracked, err)
	}
	if body, _ := os.ReadFile(filepath.Join(w.Path, "a.txt")); string(body) != "two\n" {
		t.Fatalf("checkout content %q", body)
	}
	j, err := ReadJournal(f.runtimeDir, id)
	if err != nil || j.State != StateReady || j.Tree != f.b.Tree || j.WorktreePath != w.Path {
		t.Fatalf("journal %+v %v", j, err)
	}
	// The author workspace is untouched, still editable, and edits never
	// reach the verification checkout.
	if body, _ := os.ReadFile(filepath.Join(author, "a.txt")); string(body) != "in progress\n" {
		t.Fatalf("author workspace changed: %q", body)
	}
	if err := os.WriteFile(filepath.Join(author, "a.txt"), []byte("edited during verification\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tracked, err := w.TrackedTree(ctx); err != nil || tracked != f.b.Tree {
		t.Fatalf("author edit leaked: %s %v", tracked, err)
	}
	// Untracked and ignored files do not make the checkout dirty.
	if err := os.WriteFile(filepath.Join(w.Path, "scratch.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tracked, err := w.TrackedTree(ctx); err != nil || tracked != f.b.Tree {
		t.Fatalf("untracked file made tree dirty: %s %v", tracked, err)
	}
	if err := w.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(w.Path) {
		t.Fatal("checkout not removed")
	}
	if !exists(author) {
		t.Fatal("author workspace removed")
	}
	// A run id is single use.
	if _, err := Prepare(ctx, f.co, f.runtimeDir, f.b, Journal{RunID: id}, nil); !apierr.HasCode(err, "run_exists") {
		t.Fatalf("reused run id: %v", err)
	}
}

func TestAC091_DirtyTrackedAborts(t *testing.T) {
	tests := map[string]func(path string){
		"modified": func(p string) { _ = os.WriteFile(filepath.Join(p, "a.txt"), []byte("tampered\n"), 0o600) },
		"deleted":  func(p string) { _ = os.Remove(filepath.Join(p, "a.txt")) },
		"mode":     func(p string) { _ = os.Chmod(filepath.Join(p, "run.sh"), 0o755) },
		"type": func(p string) {
			_ = os.Remove(filepath.Join(p, "run.sh"))
			_ = os.Symlink("a.txt", filepath.Join(p, "run.sh"))
		},
	}
	for name, tamper := range tests {
		t.Run(name, func(t *testing.T) {
			f := newWT(t)
			id := runID()
			_, err := Prepare(context.Background(), f.co, f.runtimeDir, f.b, Journal{RunID: id}, &Hooks{AfterCheckout: tamper})
			f.expectAbort(t, id, "dirty_checkout", err)
		})
	}
}

func TestAC057_MissingTreeAbortsBeforeCheckout(t *testing.T) {
	f := newWT(t)
	f.b.Tree = strings.Repeat("d", 64)
	id := runID()
	created := false
	_, err := Prepare(context.Background(), f.co, f.runtimeDir, f.b, Journal{RunID: id}, &Hooks{AfterCheckout: func(string) { created = true }})
	f.expectAbort(t, id, "object_missing", err)
	if created {
		t.Fatal("checkout was created for a tree the store lacks")
	}
}

func TestVerificationSetupRecovery(t *testing.T) {
	crash := errors.New("injected crash")
	for _, stage := range []string{StageBeforeWorktreeAdd, StageAfterWorktreeAdd, StageReady} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			f := newWT(t)
			// An unrelated, user-modified checkout outside verify/.
			other := f.author(t)
			if err := os.WriteFile(filepath.Join(other, "a.txt"), []byte("user edit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			foreignID := runID()
			if err := WriteJournal(f.runtimeDir, Journal{RunID: foreignID, State: StatePreparing, Manifest: f.b.Manifest, Tree: f.b.Tree, WorktreePath: other}); err != nil {
				t.Fatal(err)
			}
			id := runID()
			_, err := Prepare(ctx, f.co, f.runtimeDir, f.b, Journal{RunID: id}, &Hooks{Fault: func(s string) error {
				if s == stage {
					return crash
				}
				return nil
			}})
			if !errors.Is(err, crash) {
				t.Fatalf("fault not returned: %v", err)
			}
			jr, jerr := ReadJournal(f.runtimeDir, id)
			if jerr != nil || jr.Terminal() {
				t.Fatalf("journal after crash %+v %v", jr, jerr)
			}
			// Restart.
			got, err := Reconcile(ctx, f.co, f.runtimeDir)
			if err != nil || len(got) != 2 {
				t.Fatalf("reconcile %v %v", got, err)
			}
			path := VerifyPath(f.runtimeDir, id)
			if exists(path) {
				t.Fatalf("leftover verification checkout after reconcile (%s)", stage)
			}
			for _, rid := range []string{id, foreignID} {
				if j, err := ReadJournal(f.runtimeDir, rid); err != nil || j.State != StateInterrupted {
					t.Fatalf("journal %s %+v %v", rid, j, err)
				}
			}
			if body, _ := os.ReadFile(filepath.Join(other, "a.txt")); string(body) != "user edit\n" {
				t.Fatal("unrelated checkout was touched")
			}
			// Reconcile is idempotent and the interrupted runs are listed.
			if again, err := Reconcile(ctx, f.co, f.runtimeDir); err != nil || len(again) != 0 {
				t.Fatalf("second reconcile %v %v", again, err)
			}
			if list, err := Interrupted(f.runtimeDir); err != nil || len(list) != 2 {
				t.Fatalf("interrupted %v %v", list, err)
			}
			// A fresh run id then works.
			w, err := Prepare(ctx, f.co, f.runtimeDir, f.b, Journal{RunID: runID()}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Remove(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
