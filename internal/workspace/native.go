// Package workspace implements copy-on-write workspaces over the native
// store and their fixed-base snapshots (ADR 0026).
package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/cow"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/store"
)

// NativeProvider creates, locates, snapshots and removes copy-on-write
// workspaces over the native store (ADR 0026). Snapshots are operation log
// entries, so OpHead is set on every snapshot it returns.
type NativeProvider struct {
	Workspaces *cow.Manager
	Store      *store.Store
}

// OpenNative opens the workspaces of the native repository whose .ledger
// directory is ledgerDir. Dependency trees are shared from the repository's
// own directory, the default workspace.
func OpenNative(ledgerDir string, caseSensitive bool) (NativeProvider, error) {
	objects, err := store.Open(filepath.Join(ledgerDir, record.StoreDir, "objects"), caseSensitive)
	if err != nil {
		return NativeProvider{}, err
	}
	workspaces, err := cow.New(cow.Config{Store: objects, Runtime: filepath.Join(ledgerDir, "runtime"), Logs: filepath.Join(ledgerDir, "oplog"),
		CaseSensitive: caseSensitive, Human: filepath.Dir(ledgerDir)})
	if err != nil {
		return NativeProvider{}, err
	}
	return NativeProvider{Workspaces: workspaces, Store: objects}, nil
}

func (p NativeProvider) Path(id string) string { return p.Workspaces.Path(id) }

func (p NativeProvider) Trees() TreeSource { return StoreTrees{Store: p.Store} }

func (p NativeProvider) exists(id string) error {
	if info, err := os.Lstat(p.Path(id)); err != nil || !info.IsDir() {
		return apierr.New("workspace_unavailable", "workspace %s is missing", id)
	}
	return nil
}

// Snapshot records the workspace's current tree in its operation log.
func (p NativeProvider) Snapshot(_ context.Context, w schema.Workspace) (Snapshot, error) {
	if err := p.exists(w.ID); err != nil {
		return Snapshot{}, err
	}
	e, h, _, err := p.Workspaces.Snapshot(w.ID, "observe", nil)
	if err != nil {
		return Snapshot{}, err
	}
	return p.diff(w, e.Tree, h)
}

// StableSnapshot records a durable propose entry; see cow.Manager.Propose.
func (p NativeProvider) StableSnapshot(_ context.Context, w schema.Workspace, session string) (Snapshot, error) {
	if err := p.exists(w.ID); err != nil {
		return Snapshot{}, err
	}
	e, h, err := p.Workspaces.Propose(w.ID, optionalSession(session))
	if err != nil {
		return Snapshot{}, err
	}
	return p.diff(w, e.Tree, h)
}

func (p NativeProvider) diff(w schema.Workspace, tree, head string) (Snapshot, error) {
	if w.BaseTree == "" {
		return Snapshot{}, apierr.New("workspace_conflict", "workspace %s has no native base tree", w.ID)
	}
	changes, err := p.Store.Diff(w.BaseTree, tree)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Tree: tree, Files: []string{}, Status: map[string]string{}, OpHead: head}
	for _, c := range changes {
		if c.Old.Type == store.TypeGitlink || c.New.Type == store.TypeGitlink {
			return Snapshot{}, apierr.New("unsupported_submodule", "submodule changes are not supported: %s", c.Path)
		}
		snap.Files = append(snap.Files, c.Path)
		snap.Status[c.Path] = c.Status
	}
	slices.Sort(snap.Files)
	return snap, nil
}

// Apply performs a journaled workspace creation or removal. Creating a
// workspace that already has a log entry keeps it, edits and all, as a
// retry after a crash must. Removal first records the workspace's tree,
// so tracked edits survive in its log, and refuses while other ignored
// files remain.
func (p NativeProvider) Apply(_ context.Context, e record.Effect) error {
	switch e.Kind {
	case record.EffectWorkspaceCreate:
		if e.Path != p.Path(e.WorkspaceID) {
			return apierr.New("workspace_conflict", "workspace moved after effect preparation; preserving it")
		}
		_, err := p.Workspaces.Create(e.WorkspaceID, e.Base, optionalSession(e.Session))
		if apierr.HasCode(err, "workspace_exists") {
			return nil
		}
		return err
	case record.EffectWorkspaceRemove:
		if e.Path != p.Path(e.WorkspaceID) {
			return apierr.New("workspace_conflict", "workspace moved after effect preparation; preserving it")
		}
		if _, err := os.Lstat(e.Path); os.IsNotExist(err) {
			return nil
		}
		if _, _, _, err := p.Workspaces.Snapshot(e.WorkspaceID, "close", nil); err != nil {
			return err
		}
		ignored, err := p.Workspaces.IgnoredFiles(e.WorkspaceID)
		if err != nil {
			return err
		}
		if len(ignored) > 0 {
			return apierr.New("workspace_dirty", "ignored files remain in %s; cleanup preserves them", e.Path).With("paths", ignored)
		}
		return p.Workspaces.Remove(e.WorkspaceID)
	}
	return apierr.New("invalid_effect", "a native repository has no %s effect", e.Kind)
}

// WriteText replaces one regular text file; see GitProvider.WriteText. A
// dependency manifest written through the API first gives the workspace
// private dependency trees, so a later install changes no other
// workspace (ADR 0026). The write is recorded as a write entry.
func (p NativeProvider) WriteText(_ context.Context, w schema.Workspace, name, content string, expected *string) (string, error) {
	if err := p.exists(w.ID); err != nil {
		return "", err
	}
	if capability.Dependency(name) {
		if err := p.Workspaces.Detach(w.ID, ""); err != nil {
			return "", err
		}
	}
	hash, err := writeText(p.Path(w.ID), name, content, expected)
	if err != nil {
		return "", err
	}
	if _, _, _, err := p.Workspaces.Wrote(w.ID, nil); err != nil {
		return "", err
	}
	return hash, nil
}

func optionalSession(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
