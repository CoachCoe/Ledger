package evidence

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/verify"
)

// Fault stages inside Prepare, for crash-injection tests.
const (
	StageBeforeWorktreeAdd = "before_worktree_add"
	StageAfterWorktreeAdd  = "after_worktree_add"
	StageReady             = "ready"
)

// Fault stages inside Execute and the service's run publication.
const (
	StagePrepared   = "prepared"
	StageAfterSetup = "after_setup"
	StageAfterCheck = "after_check"
	StageExecuted   = "executed"
	StageBeforeSign = "before_sign"
	StageAfterSign  = "after_sign"
)

// Hooks are test seams. A nil Hooks or nil member is a no-op. A Fault error
// simulates a crash: Prepare returns it without cleanup.
type Hooks struct {
	// AfterCheckout runs once the worktree exists, before it is verified.
	AfterCheckout func(path string)
	// AfterSetup and AfterCheck run after the setup and check phases,
	// before the tracked-content comparison.
	AfterSetup func(path string)
	AfterCheck func(path string)
	Fault      func(stage string) error
}

// Stage fires the fault hook for stage; a nil Hooks never faults.
func (h *Hooks) Stage(stage string) error { return h.fault(stage) }

func (h *Hooks) fault(stage string) error {
	if h == nil || h.Fault == nil {
		return nil
	}
	return h.Fault(stage)
}

// Worktree is a verified, detached checkout of the bound commit.
type Worktree struct {
	RunID   string
	Path    string
	Binding Binding

	repo       *gitx.Repo
	runtimeDir string
}

// Prepare creates a fresh detached worktree at the bound commit under
// <runtimeDir>/verify/<runID>. Before anything can run it checks the three
// commit bindings, that the checkout's tree equals the manifest tree, and
// that no tracked file differs. On any failure no worktree remains and the
// journal is "aborted" with a structured error.
func Prepare(ctx context.Context, repo *gitx.Repo, runtimeDir string, b Binding, runID string, hooks *Hooks) (*Worktree, error) {
	return prepare(ctx, repo, runtimeDir, b, Journal{RunID: runID}, hooks)
}

// prepare is Prepare with a journal template carrying the run's operation,
// check definition and verifier, which the journal keeps for recovery.
func prepare(ctx context.Context, repo *gitx.Repo, runtimeDir string, b Binding, j Journal, hooks *Hooks) (*Worktree, error) {
	runID := j.RunID
	path := VerifyPath(runtimeDir, runID)
	j.State, j.Manifest, j.Commit, j.Tree, j.WorktreePath = StatePreparing, b.Manifest, b.Commit, b.Tree, path
	if _, err := ReadJournal(runtimeDir, runID); err == nil {
		return nil, apierr.New("run_exists", "evidence run %s already has a journal", runID)
	} else if !apierr.HasCode(err, "unknown_run") {
		return nil, err
	}
	if err := WriteJournal(runtimeDir, j); err != nil {
		return nil, err
	}
	w := &Worktree{RunID: runID, Path: path, Binding: b, repo: repo, runtimeDir: runtimeDir}
	abort := func(err error) (*Worktree, error) {
		cleanCtx := context.WithoutCancel(ctx)
		_ = w.Remove(cleanCtx)
		j.State, j.Reason = StateAborted, apierr.As(err).Code
		if werr := WriteJournal(runtimeDir, j); werr != nil {
			return nil, werr
		}
		return nil, err
	}
	if err := checkCommit(ctx, repo, b); err != nil {
		return abort(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return abort(err)
	}
	if err := hooks.fault(StageBeforeWorktreeAdd); err != nil {
		return nil, err
	}
	if err := repo.WorktreeAddDetached(ctx, path, b.Commit); err != nil {
		return abort(err)
	}
	if err := hooks.fault(StageAfterWorktreeAdd); err != nil {
		return nil, err
	}
	if hooks != nil && hooks.AfterCheckout != nil {
		hooks.AfterCheckout(path)
	}
	if err := w.verifyCheckout(ctx); err != nil {
		return abort(err)
	}
	j.State = StateReady
	if err := WriteJournal(runtimeDir, j); err != nil {
		return abort(err)
	}
	if err := hooks.fault(StageReady); err != nil {
		return nil, err
	}
	return w, nil
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

// verifyCheckout asserts HEAD's tree and the tracked working files both
// equal the manifest tree.
func (w *Worktree) verifyCheckout(ctx context.Context) error {
	head, err := gitx.Run(ctx, w.Path, nil, nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(head)); got != w.Binding.Tree {
		return apierr.New("tree_mismatch", "verification checkout tree %s differs from manifest tree %s", got, w.Binding.Tree)
	}
	got, err := w.TrackedTree(ctx)
	if err != nil {
		return err
	}
	if got != w.Binding.Tree {
		return apierr.New("dirty_checkout", "tracked files in the verification checkout differ from the manifest tree").
			With("expected_tree", w.Binding.Tree).With("actual_tree", got)
	}
	return nil
}

// TrackedTree returns the tree of HEAD with every tracked file's current
// content, mode and presence applied. It uses a temporary index, so the
// real index is untouched, and ignores untracked and ignored files.
func (w *Worktree) TrackedTree(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "ledger-tracked-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	for _, args := range [][]string{{"read-tree", "HEAD"}, {"add", "-u"}} {
		if _, err := gitx.Run(ctx, w.Path, nil, env, args...); err != nil {
			return "", err
		}
	}
	out, err := gitx.Run(ctx, w.Path, nil, env, "write-tree")
	return strings.TrimSpace(string(out)), err
}

// Remove deletes the worktree. It refuses any path that is not exactly
// <runtimeDir>/verify/<run id> for this run.
func (w *Worktree) Remove(ctx context.Context) error {
	return removeVerifyWorktree(ctx, w.repo, w.runtimeDir, w.RunID, w.Path)
}

// removeVerifyWorktree removes path only if it is the run's own verify path
// and git lists it as a detached worktree there. Anything else is left alone.
func removeVerifyWorktree(ctx context.Context, repo *gitx.Repo, runtimeDir, runID, path string) error {
	if !runIDPattern.MatchString(runID) || filepath.Clean(path) != VerifyPath(runtimeDir, runID) {
		return apierr.New("unsafe_worktree_path", "%s is not the verification path of run %s", path, runID)
	}
	listed, err := repo.Worktrees(ctx)
	if err != nil {
		return err
	}
	want := realPath(path)
	for _, wt := range listed {
		if realPath(wt.Path) == want && wt.Detached {
			return repo.WorktreeRemove(ctx, wt.Path)
		}
	}
	return nil
}

// realPath resolves symlinks in the longest existing prefix of p.
func realPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if dir := filepath.Dir(p); dir != p {
		return filepath.Join(realPath(dir), filepath.Base(p))
	}
	return p
}

// Reconcile marks every non-terminal run interrupted and removes the
// verification worktree it left behind, only when the journalled path is the
// run's own verify path and git lists it as a detached worktree. It returns
// the journals it interrupted. No attestation is ever created here.
func Reconcile(ctx context.Context, repo *gitx.Repo, runtimeDir string) ([]Journal, error) {
	journals, err := ListJournals(runtimeDir)
	if err != nil {
		return nil, err
	}
	var interrupted []Journal
	for _, j := range journals {
		if j.Terminal() {
			continue
		}
		if err := removeVerifyWorktree(ctx, repo, runtimeDir, j.RunID, j.WorktreePath); err != nil && !apierr.HasCode(err, "unsafe_worktree_path") {
			return nil, err
		}
		j.State, j.Reason = StateInterrupted, "daemon_restart"
		if err := WriteJournal(runtimeDir, j); err != nil {
			return nil, err
		}
		interrupted = append(interrupted, j)
	}
	return interrupted, nil
}

// Interrupted lists runs a restart left interrupted.
func Interrupted(runtimeDir string) ([]Journal, error) {
	all, err := ListJournals(runtimeDir)
	if err != nil {
		return nil, err
	}
	var out []Journal
	for _, j := range all {
		if j.State == StateInterrupted {
			out = append(out, j)
		}
	}
	return out, nil
}
