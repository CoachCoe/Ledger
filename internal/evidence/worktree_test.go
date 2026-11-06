package evidence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/schema"
)

const testManifest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var gitEnv = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@invalid"}

type wtFixture struct {
	repo       *gitx.Repo
	runtimeDir string
	base       string
	b          Binding
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(context.Background(), dir, nil, gitEnv, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// newWT builds a repo with a base commit and a proposal commit (carrying
// the manifest trailer) whose parent is the base.
func newWT(t *testing.T) *wtFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "one\n")
	write("run.sh", "echo hi\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-qm", "base")
	base := git(t, dir, "rev-parse", "HEAD")
	write("a.txt", "two\n")
	git(t, dir, "commit", "-qam", "proposal\n\nLedger-Manifest: "+testManifest)
	commit := git(t, dir, "rev-parse", "HEAD")
	tree := git(t, dir, "rev-parse", "HEAD^{tree}")
	git(t, dir, "checkout", "-q", "--detach", base)
	repo, err := gitx.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return &wtFixture{repo: repo, runtimeDir: filepath.Join(repo.MainDir, ".ledger", "runtime"), base: base,
		b: Binding{Manifest: testManifest, Commit: commit, Tree: tree, BaseCommit: base}}
}

func runID() string { return schema.NewID(schema.PrefixRun) }

func (f *wtFixture) worktreeListed(t *testing.T, path string) bool {
	t.Helper()
	list, err := f.repo.Worktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		if realPath(w.Path) == realPath(path) {
			return true
		}
	}
	return false
}

func (f *wtFixture) expectAbort(t *testing.T, id, code string, err error) {
	t.Helper()
	if !apierr.HasCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
	path := VerifyPath(f.runtimeDir, id)
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) || f.worktreeListed(t, path) {
		t.Fatalf("worktree %s remains after abort", path)
	}
	j, jerr := ReadJournal(f.runtimeDir, id)
	if jerr != nil || j.State != StateAborted || j.Reason != code {
		t.Fatalf("journal %+v %v", j, jerr)
	}
	if list, _ := os.ReadDir(filepath.Join(f.repo.MainDir, ".git", "refs", "ledger")); len(list) != 0 {
		t.Fatalf("abort created ledger refs: %v", list)
	}
}

func TestAC090_091_111_VerificationWorktree(t *testing.T) {
	f := newWT(t)
	ctx := context.Background()
	// An author workspace (a linked worktree at the base) being edited.
	author := filepath.Join(t.TempDir(), "author")
	git(t, f.repo.Dir, "worktree", "add", "--detach", "-q", author, f.base)
	if err := os.WriteFile(filepath.Join(author, "a.txt"), []byte("in progress\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := runID()
	w, err := Prepare(ctx, f.repo, f.runtimeDir, f.b, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(f.runtimeDir, "verify", id); w.Path != want {
		t.Fatalf("path %s want %s", w.Path, want)
	}
	if head := git(t, w.Path, "rev-parse", "HEAD"); head != f.b.Commit {
		t.Fatalf("HEAD %s want %s", head, f.b.Commit)
	}
	if branch := git(t, w.Path, "branch", "--show-current"); branch != "" {
		t.Fatalf("worktree is on branch %q, want detached", branch)
	}
	if tree := git(t, w.Path, "rev-parse", "HEAD^{tree}"); tree != f.b.Tree {
		t.Fatalf("tree %s want %s", tree, f.b.Tree)
	}
	if tracked, err := w.TrackedTree(ctx); err != nil || tracked != f.b.Tree {
		t.Fatalf("tracked tree %s %v", tracked, err)
	}
	if body, _ := os.ReadFile(filepath.Join(w.Path, "a.txt")); string(body) != "two\n" {
		t.Fatalf("checkout content %q", body)
	}
	j, err := ReadJournal(f.runtimeDir, id)
	if err != nil || j.State != StateReady || j.Commit != f.b.Commit || j.WorktreePath != w.Path {
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
	if _, err := os.Stat(w.Path); !errors.Is(err, os.ErrNotExist) || f.worktreeListed(t, w.Path) {
		t.Fatal("worktree not removed")
	}
	if !f.worktreeListed(t, author) {
		t.Fatal("author worktree removed")
	}
	// A run id is single use.
	if _, err := Prepare(ctx, f.repo, f.runtimeDir, f.b, id, nil); !apierr.HasCode(err, "run_exists") {
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
			_, err := Prepare(context.Background(), f.repo, f.runtimeDir, f.b, id, &Hooks{AfterCheckout: tamper})
			f.expectAbort(t, id, "dirty_checkout", err)
		})
	}
}

func TestAC091_TreeMismatchAborts(t *testing.T) {
	ctx := context.Background()
	t.Run("manifest names a different tree", func(t *testing.T) {
		f := newWT(t)
		f.b.Tree = git(t, f.repo.Dir, "rev-parse", f.base+"^{tree}")
		id := runID()
		_, err := Prepare(ctx, f.repo, f.runtimeDir, f.b, id, nil)
		f.expectAbort(t, id, "tree_mismatch", err)
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Details["tree"] != "mismatch" || ae.Details["trailer"] != "ok" || ae.Details["parent"] != "ok" {
			t.Fatalf("details %+v", ae)
		}
	})
	t.Run("checkout head moves off the bound tree", func(t *testing.T) {
		f := newWT(t)
		id := runID()
		move := func(p string) { git(t, p, "checkout", "-q", "--detach", f.base) }
		_, err := Prepare(ctx, f.repo, f.runtimeDir, f.b, id, &Hooks{AfterCheckout: move})
		f.expectAbort(t, id, "tree_mismatch", err)
	})
}

func TestAC057_BindingFailuresAbortBeforeCheckout(t *testing.T) {
	tests := map[string]struct {
		mutate  func(*Binding)
		code    string
		trailer string
		tree    string
		parent  string
	}{
		"trailer": {func(b *Binding) { b.Manifest = strings.Repeat("b", 64) }, "binding_mismatch", "mismatch", "ok", "ok"},
		"parent":  {func(b *Binding) { b.BaseCommit = strings.Repeat("c", 40) }, "binding_mismatch", "ok", "ok", "mismatch"},
		"tree":    {func(b *Binding) { b.Tree = strings.Repeat("d", 40) }, "tree_mismatch", "ok", "mismatch", "ok"},
		"all": {func(b *Binding) {
			b.Manifest, b.BaseCommit, b.Tree = strings.Repeat("b", 64), strings.Repeat("c", 40), strings.Repeat("d", 40)
		}, "binding_mismatch", "mismatch", "mismatch", "mismatch"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := newWT(t)
			tc.mutate(&f.b)
			id := runID()
			created := false
			_, err := Prepare(context.Background(), f.repo, f.runtimeDir, f.b, id, &Hooks{AfterCheckout: func(string) { created = true }})
			f.expectAbort(t, id, tc.code, err)
			var ae *apierr.Error
			if !errors.As(err, &ae) || ae.Details["trailer"] != tc.trailer || ae.Details["tree"] != tc.tree || ae.Details["parent"] != tc.parent {
				t.Fatalf("details %+v", ae)
			}
			if created {
				t.Fatal("worktree was created before the binding check")
			}
		})
	}
	t.Run("commit object missing", func(t *testing.T) {
		f := newWT(t)
		f.b.Commit = strings.Repeat("a", 40)
		id := runID()
		_, err := Prepare(context.Background(), f.repo, f.runtimeDir, f.b, id, nil)
		f.expectAbort(t, id, "dependency_unavailable", err)
	})
}

func TestVerificationSetupRecovery(t *testing.T) {
	crash := errors.New("injected crash")
	for _, stage := range []string{StageBeforeWorktreeAdd, StageAfterWorktreeAdd, StageReady} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			f := newWT(t)
			// An unrelated worktree and a user-modified path outside verify/.
			other := filepath.Join(t.TempDir(), "other")
			git(t, f.repo.Dir, "worktree", "add", "--detach", "-q", other, f.base)
			if err := os.WriteFile(filepath.Join(other, "a.txt"), []byte("user edit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			foreignID := runID()
			if err := WriteJournal(f.runtimeDir, Journal{RunID: foreignID, State: StatePreparing, Manifest: f.b.Manifest, Commit: f.b.Commit, Tree: f.b.Tree, WorktreePath: other}); err != nil {
				t.Fatal(err)
			}
			id := runID()
			_, err := Prepare(ctx, f.repo, f.runtimeDir, f.b, id, &Hooks{Fault: func(s string) error {
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
			got, err := Reconcile(ctx, f.repo, f.runtimeDir)
			if err != nil || len(got) != 2 {
				t.Fatalf("reconcile %v %v", got, err)
			}
			path := VerifyPath(f.runtimeDir, id)
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || f.worktreeListed(t, path) {
				t.Fatalf("leftover verification worktree after reconcile (%s)", stage)
			}
			for _, rid := range []string{id, foreignID} {
				if j, err := ReadJournal(f.runtimeDir, rid); err != nil || j.State != StateInterrupted {
					t.Fatalf("journal %s %+v %v", rid, j, err)
				}
			}
			if body, _ := os.ReadFile(filepath.Join(other, "a.txt")); string(body) != "user edit\n" || !f.worktreeListed(t, other) {
				t.Fatal("unrelated worktree was touched")
			}
			// Reconcile is idempotent and the interrupted runs are listed.
			if again, err := Reconcile(ctx, f.repo, f.runtimeDir); err != nil || len(again) != 0 {
				t.Fatalf("second reconcile %v %v", again, err)
			}
			if list, err := Interrupted(f.runtimeDir); err != nil || len(list) != 2 {
				t.Fatalf("interrupted %v %v", list, err)
			}
			// A fresh run id then works.
			w, err := Prepare(ctx, f.repo, f.runtimeDir, f.b, runID(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Remove(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
