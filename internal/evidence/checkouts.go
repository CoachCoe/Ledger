package evidence

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/cow"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/verify"
)

// GitCheckouts checks a Phase 1 proposal commit out as a detached worktree.
type GitCheckouts struct{ Repo *gitx.Repo }

func (g GitCheckouts) Add(ctx context.Context, path string, b Binding) error {
	if err := checkCommit(ctx, g.Repo, b); err != nil {
		return err
	}
	return g.Repo.WorktreeAddDetached(ctx, path, b.Commit)
}

// Verify checks that HEAD still names the bound tree.
func (g GitCheckouts) Verify(ctx context.Context, path string, b Binding) error {
	head, err := gitx.Run(ctx, path, nil, nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(head)); got != b.Tree {
		return apierr.New("tree_mismatch", "verification checkout tree %s differs from manifest tree %s", got, b.Tree)
	}
	return nil
}

// Tree applies every tracked file's current content, mode and presence to
// HEAD through a temporary index, so the real index is untouched.
func (g GitCheckouts) Tree(ctx context.Context, path string, _ Binding) (string, error) {
	dir, err := os.MkdirTemp("", "ledger-tracked-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	for _, args := range [][]string{{"read-tree", "HEAD"}, {"add", "-u"}} {
		if _, err := gitx.Run(ctx, path, nil, env, args...); err != nil {
			return "", err
		}
	}
	out, err := gitx.Run(ctx, path, nil, env, "write-tree")
	return strings.TrimSpace(string(out)), err
}

// Remove removes path only while git lists it as a detached worktree.
func (g GitCheckouts) Remove(ctx context.Context, path string) error {
	listed, err := g.Repo.Worktrees(ctx)
	if err != nil {
		return err
	}
	want := realPath(path)
	for _, wt := range listed {
		if realPath(wt.Path) == want && wt.Detached {
			return g.Repo.WorktreeRemove(ctx, wt.Path)
		}
	}
	return nil
}

// checkCommit loads the bound commit and applies the three binding
// conditions. A tree-only mismatch is reported as tree_mismatch; any other
// failure as binding_mismatch. Both carry the three conditions.
func checkCommit(ctx context.Context, repo *gitx.Repo, b Binding) error {
	c, err := repo.ReadCommit(ctx, b.Commit)
	if err != nil {
		return err
	}
	res := verify.CheckBinding(c, b.Manifest, schema.ChangeManifest{Tree: b.Tree, BaseCommit: b.BaseCommit})
	if res.OK() {
		return nil
	}
	code, msg := "binding_mismatch", "commit %s is not bound to manifest %s"
	if res.Tree == verify.BindingMismatch && res.Trailer == verify.BindingOK && res.Parent == verify.BindingOK {
		code, msg = "tree_mismatch", "commit %s has a different tree than manifest %s"
	}
	return apierr.New(code, msg, b.Commit, b.Manifest).With("trailer", res.Trailer).With("tree", res.Tree).With("parent", res.Parent).With("detail", res.Detail)
}

// NativeCheckouts checks a v2 manifest's tree out as a copy-on-write clone.
// There is no commit: the manifest names the tree it binds directly.
type NativeCheckouts struct{ Workspaces *cow.Manager }

func (n NativeCheckouts) Add(_ context.Context, path string, b Binding) error {
	if b.Commit != "" {
		return apierr.New("binding_mismatch", "manifest %s binds a tree, not commit %s", b.Manifest, b.Commit)
	}
	return n.Workspaces.Checkout(b.Tree, path)
}

// Verify has nothing to check: a clone has no HEAD to move.
func (n NativeCheckouts) Verify(context.Context, string, Binding) error { return nil }

func (n NativeCheckouts) Tree(_ context.Context, path string, b Binding) (string, error) {
	return n.Workspaces.TrackedTree(path, b.Tree)
}

// Remove removes the clone. Prepare only ever gives it the run's own
// verify path, which nothing else writes.
func (n NativeCheckouts) Remove(_ context.Context, path string) error {
	return os.RemoveAll(path)
}
