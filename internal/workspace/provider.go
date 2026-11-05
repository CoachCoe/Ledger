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

type WorkspaceProvider interface {
	Path(id string) string
	Snapshot(context.Context, schema.Workspace, string, bool) (Snapshot, error)
	Apply(context.Context, record.Effect) error
}

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

func (p GitProvider) Apply(ctx context.Context, e record.Effect) error {
	if e.Kind == "ref" {
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
	if e.Kind != "worktree_add" && e.Kind != "worktree_remove" {
		return apierr.New("invalid_effect", "unknown effect %s", e.Kind)
	}
	id := e.WorkspaceID
	if id == "" {
		id = filepath.Base(e.Path)
	}
	if !strings.HasPrefix(id, "wsp_") || len(id) != 30 || filepath.Base(id) != id {
		return apierr.New("invalid_effect", "invalid workspace identity")
	}
	path := p.Path(id)
	if (e.Kind == "worktree_add" && e.Path != p.defaultPath(id)) || (e.Kind == "worktree_remove" && e.Path != path) {
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

		if e.Kind == "worktree_add" {
			return nil
		} // preserve edits made after a partial fork
		ignored, err := gitx.Run(ctx, path, nil, nil, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
		if err != nil {
			return err
		}
		if len(ignored) > 0 {
			return apierr.New("workspace_dirty", "ignored files remain in %s; cleanup preserves them", e.Path)
		}
		_, err = gitx.Run(ctx, p.Repo.MainDir, nil, nil, "worktree", "remove", e.Path)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if e.Kind == "worktree_remove" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(e.Path), 0700); err != nil {
		return err
	}
	_, err = gitx.Run(ctx, p.Repo.MainDir, nil, nil, "worktree", "add", "--detach", e.Path, e.Base)
	return err
}

// Snapshot verifies that the workspace still belongs to this repository,
// then captures through private indexes. A replacement directory is not
// silently treated as the signed workspace.
func (p GitProvider) Snapshot(ctx context.Context, w schema.Workspace, indexFile string, stable bool) (Snapshot, error) {
	path := p.Path(w.ID)
	if err := p.validate(ctx, w.ID, path); err != nil {
		return Snapshot{}, err
	}
	if stable {
		return StableSnapshot(ctx, path, w.BaseCommit, filepath.Dir(indexFile))
	}
	return Capture(ctx, path, w.BaseCommit, indexFile)
}
