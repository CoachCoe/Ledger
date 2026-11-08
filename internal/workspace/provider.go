// Package workspace implements detached Git workspaces and fixed-base snapshots.
package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/lock"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// WorkspaceProvider creates, locates, captures and removes workspaces, so
// a copy-on-write implementation can replace Git worktrees.
type WorkspaceProvider interface {
	// Path returns where workspace id is checked out.
	Path(id string) string
	// Snapshot captures w through the persistent private index indexFile.
	Snapshot(ctx context.Context, w schema.Workspace, indexFile string) (Snapshot, error)
	// StableSnapshot captures w for a proposal, using scratch indexes in
	// dir to detect files changing during the capture.
	StableSnapshot(ctx context.Context, w schema.Workspace, dir string) (Snapshot, error)
	// Apply performs a journaled workspace or ref effect idempotently.
	Apply(ctx context.Context, e record.Effect) error
}

// GitProvider implements WorkspaceProvider with detached Git worktrees.
type GitProvider struct{ Repo *gitx.Repo }

func (p GitProvider) defaultPath(id string) string {
	return filepath.Join(p.Repo.MainDir, ".ledger", "runtime", "workspaces", id)
}

func (p GitProvider) Path(id string) string {
	if raw, err := os.ReadFile(filepath.Join(p.Repo.CommonDir, "worktrees", id, "gitdir")); err == nil {
		path := strings.TrimSuffix(string(raw), "\n")
		if filepath.IsAbs(path) && filepath.Base(path) == ".git" {
			return filepath.Dir(path)
		}
	}
	return p.defaultPath(id)
}

func (p GitProvider) validate(ctx context.Context, id, path string) error {
	repo, err := gitx.Open(ctx, path)
	if err != nil || repo.CommonDir != p.Repo.CommonDir || repo.Dir != path {
		return apierr.New("workspace_unavailable", "workspace %s is missing or belongs to another repository", id)
	}
	raw, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		return err
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSuffix(string(raw), "\n"), "gitdir: ")
	if !ok {
		return apierr.New("workspace_conflict", "workspace Git metadata changed")
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(path, gitdir)
	}
	if filepath.Clean(gitdir) != filepath.Join(p.Repo.CommonDir, "worktrees", id) {
		return apierr.New("workspace_conflict", "path is registered to another workspace")
	}
	return nil
}

// Apply performs a journaled effect. Ref effects stay inside Ledger's
// namespaces; worktree effects are serialized because concurrent
// "git worktree add" calls intermittently fail.
func (p GitProvider) Apply(ctx context.Context, e record.Effect) error {
	if e.Kind == record.EffectRef {
		if !strings.HasPrefix(e.Ref, "refs/heads/ledger/") && !strings.HasPrefix(e.Ref, "refs/ledger/proposals/") {
			return apierr.New("invalid_effect", "ref outside Ledger namespace")
		}
		current, err := p.Repo.ResolveRef(ctx, e.Ref)
		if err != nil {
			return err
		}
		if current == e.New {
			return nil
		}
		return p.Repo.UpdateRefCAS(ctx, e.Ref, e.New, e.Old)
	}
	if e.Kind != record.EffectWorktreeAdd && e.Kind != record.EffectWorktreeRemove {
		return apierr.New("invalid_effect", "unknown effect %s", e.Kind)
	}
	id := e.WorkspaceID
	if !strings.HasPrefix(id, "wsp_") || len(id) != 30 || filepath.Base(id) != id {
		return apierr.New("invalid_effect", "invalid workspace identity")
	}
	path := p.Path(id)
	if (e.Kind == record.EffectWorktreeAdd && e.Path != p.defaultPath(id)) || (e.Kind == record.EffectWorktreeRemove && e.Path != path) {
		return apierr.New("workspace_conflict", "workspace moved after effect preparation; preserving it")
	}
	unlock, err := lock.Acquire(filepath.Join(p.Repo.MainDir, ".ledger", "runtime", "worktrees.lock"), false)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Lstat(path); err == nil {
		if err := p.validate(ctx, id, path); err != nil {
			return err
		}
		// A partial fork may already have created the worktree; keep any
		// edits made in it since.
		if e.Kind == record.EffectWorktreeAdd {
			return nil
		}
		ignored, err := gitx.Run(ctx, path, nil, nil, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
		if err != nil {
			return err
		}
		if len(ignored) > 0 {
			return apierr.New("workspace_dirty", "ignored files remain in %s; cleanup preserves them", e.Path)
		}
		args := []string{"worktree", "remove", e.Path}
		if retained, err := p.retained(ctx, path, e); err != nil {
			return err
		} else if retained {
			args = []string{"worktree", "remove", "--force", e.Path}
		}
		_, err = gitx.Run(ctx, p.Repo.MainDir, nil, nil, args...)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if e.Kind == record.EffectWorktreeRemove {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(e.Path), 0700); err != nil {
		return err
	}
	_, err = gitx.Run(ctx, p.Repo.MainDir, nil, nil, "worktree", "add", "--detach", e.Path, e.Base)
	return err
}

// retained reports whether the worktree at path holds exactly the retained
// tree of e on its base: its uncommitted content is then preserved in Git
// and removing it loses nothing. Anything else is left to the ordinary,
// edit-preserving removal.
func (p GitProvider) retained(ctx context.Context, path string, e record.Effect) (bool, error) {
	if e.Tree == "" || e.Base == "" {
		return false, nil
	}
	head, err := gitx.Run(ctx, path, nil, nil, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != e.Base {
		return false, nil //nolint:nilerr // an unreadable or moved HEAD is not removed by force
	}
	scratch, err := os.MkdirTemp(filepath.Join(p.Repo.MainDir, ".ledger", "runtime"), "cleanup-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(scratch)
	snap, err := Capture(ctx, path, e.Base, filepath.Join(scratch, "index"))
	if err != nil {
		return false, nil //nolint:nilerr // content that cannot be captured is preserved
	}
	return snap.Tree == e.Tree, nil
}

// Snapshot verifies that the workspace still belongs to this repository,
// so a replacement directory is never treated as the signed workspace, then
// captures it.
func (p GitProvider) Snapshot(ctx context.Context, w schema.Workspace, indexFile string) (Snapshot, error) {
	path := p.Path(w.ID)
	if err := p.validate(ctx, w.ID, path); err != nil {
		return Snapshot{}, err
	}
	return Capture(ctx, path, w.BaseCommit, indexFile)
}

// StableSnapshot is Snapshot for proposals; see the package function.
func (p GitProvider) StableSnapshot(ctx context.Context, w schema.Workspace, dir string) (Snapshot, error) {
	path := p.Path(w.ID)
	if err := p.validate(ctx, w.ID, path); err != nil {
		return Snapshot{}, err
	}
	return StableSnapshot(ctx, path, w.BaseCommit, dir)
}
