package evidence

import (
	"context"
	"os"
	"path/filepath"

	"github.com/CoachCoe/Ledger/internal/apierr"
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

// Worktree is a verified checkout of the bound tree.
type Worktree struct {
	RunID   string
	Path    string
	Binding Binding

	checkouts  Checkouts
	runtimeDir string
}

// Checkouts makes the fresh checkout a check runs in and reads its tracked
// content back. Paths are always a run's own verify path.
type Checkouts interface {
	// Add checks b.Tree out at the new path.
	Add(ctx context.Context, path string, b Binding) error
	// Tree returns the tree of the tracked content now at path.
	Tree(ctx context.Context, path string, b Binding) (string, error)
	// Remove deletes the checkout at path if this source made it, and
	// leaves anything else alone.
	Remove(ctx context.Context, path string) error
}

// Prepare creates a fresh checkout of the bound tree at
// <runtimeDir>/verify/<runID>. Before anything can run it checks that no
// tracked file differs from the manifest tree. On any failure no worktree remains and the
// journal is "aborted" with a structured error. The journal template j
// carries the run's operation, check definition and verifier for recovery.
func Prepare(ctx context.Context, checkouts Checkouts, runtimeDir string, b Binding, j Journal, hooks *Hooks) (*Worktree, error) {
	runID := j.RunID
	path := VerifyPath(runtimeDir, runID)
	j.State, j.Manifest, j.Tree, j.WorktreePath = StatePreparing, b.Manifest, b.Tree, path
	if _, err := ReadJournal(runtimeDir, runID); err == nil {
		return nil, apierr.New("run_exists", "evidence run %s already has a journal", runID)
	} else if !apierr.HasCode(err, "unknown_run") {
		return nil, err
	}
	if err := WriteJournal(runtimeDir, j); err != nil {
		return nil, err
	}
	w := &Worktree{RunID: runID, Path: path, Binding: b, checkouts: checkouts, runtimeDir: runtimeDir}
	abort := func(err error) (*Worktree, error) {
		cleanCtx := context.WithoutCancel(ctx)
		_ = w.Remove(cleanCtx)
		j.State, j.Reason = StateAborted, apierr.As(err).Code
		if werr := WriteJournal(runtimeDir, j); werr != nil {
			return nil, werr
		}
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return abort(err)
	}
	if err := hooks.fault(StageBeforeWorktreeAdd); err != nil {
		return nil, err
	}
	if err := checkouts.Add(ctx, path, b); err != nil {
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

// verifyCheckout asserts the checkout's tracked working files equal the
// manifest tree.
func (w *Worktree) verifyCheckout(ctx context.Context) error {
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

// TrackedTree returns the tree of the tracked content now in the checkout,
// ignoring untracked and ignored files.
func (w *Worktree) TrackedTree(ctx context.Context) (string, error) {
	return w.checkouts.Tree(ctx, w.Path, w.Binding)
}

// Remove deletes the checkout. It refuses any path that is not exactly
// <runtimeDir>/verify/<run id> for this run.
func (w *Worktree) Remove(ctx context.Context) error {
	return removeVerifyWorktree(ctx, w.checkouts, w.runtimeDir, w.RunID, w.Path)
}

// removeVerifyWorktree removes path only if it is the run's own verify path
// and the checkout source made it. Anything else is left alone.
func removeVerifyWorktree(ctx context.Context, checkouts Checkouts, runtimeDir, runID, path string) error {
	if !runIDPattern.MatchString(runID) || filepath.Clean(path) != VerifyPath(runtimeDir, runID) {
		return apierr.New("unsafe_worktree_path", "%s is not the verification path of run %s", path, runID)
	}
	return checkouts.Remove(ctx, path)
}

// Reconcile marks every non-terminal run interrupted and removes the
// verification checkout it left behind, only when the journalled path is the
// run's own verify path and the checkout source made it. It returns
// the journals it interrupted. No attestation is ever created here.
func Reconcile(ctx context.Context, checkouts Checkouts, runtimeDir string) ([]Journal, error) {
	journals, err := ListJournals(runtimeDir)
	if err != nil {
		return nil, err
	}
	var interrupted []Journal
	for _, j := range journals {
		if j.Terminal() {
			continue
		}
		if err := removeVerifyWorktree(ctx, checkouts, runtimeDir, j.RunID, j.WorktreePath); err != nil && !apierr.HasCode(err, "unsafe_worktree_path") {
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
