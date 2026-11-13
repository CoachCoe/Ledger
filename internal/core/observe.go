package core

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"sync"

	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/workspace"
)

// observers bounds concurrent workspace captures in one observation pass.
const observers = 10

// workspaceView is the workspace.status result. Terminal workspaces carry
// only their identity, state and path.
type workspaceView struct {
	WorkspaceID string            `json:"workspace_id"`
	State       string            `json:"state"`
	Path        string            `json:"path"`
	BaseCommit  string            `json:"base_commit,omitzero"`
	BaseTree    string            `json:"base_tree,omitzero"`
	WriteSet    []string          `json:"write_set,omitzero"`
	Status      map[string]string `json:"status,omitzero"`
	ObservedAt  string            `json:"observed_at,omitzero"`
	Tree        string            `json:"tree,omitzero"`
}

// observe snapshots w into its operation log and saves the write set. The
// per-workspace lock stops an older snapshot overwriting a newer one.
func (s *Service) observe(ctx context.Context, w schema.Workspace) (workspace.Snapshot, string, error) {
	m, _ := s.observing.LoadOrStore(w.ID, new(sync.Mutex))
	mu := m.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	snap, err := s.Provider.Snapshot(ctx, w)
	if err != nil {
		return snap, "", err
	}
	if err := s.warnScope(ctx, w, snap); err != nil {
		return snap, "", err
	}
	at := now()
	if err := s.observeSymbols(ctx, w, snap, at); err != nil {
		return snap, "", err
	}
	return snap, at, s.Store.Index.SaveObservation(ctx, w.ID, s.Provider.Path(w.ID), at, snap.Status)
}

func (s *Service) observeSession(ctx context.Context, sid string) error {
	active, err := s.Store.Index.ActiveWorkspaces(ctx, sid)
	if err != nil {
		return err
	}
	for _, w := range active {
		if _, _, err := s.observe(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

// Workspaces returns reconstructible active worktree paths for watcher setup.
func (s *Service) Workspaces(ctx context.Context) (map[string]string, error) {
	active, err := s.Store.Index.ActiveWorkspaces(ctx, "")
	if err != nil {
		return nil, err
	}
	paths := make(map[string]string, len(active))
	for _, w := range active {
		paths[w.ID] = s.Provider.Path(w.ID)
	}
	return paths, nil
}

// Observe refreshes every active workspace, for watcher events and the
// fallback poll.
func (s *Service) Observe(ctx context.Context) error {
	active, err := s.Store.Index.ActiveWorkspaces(ctx, "")
	if err != nil {
		return err
	}
	errs := make([]error, len(active))
	slots := make(chan struct{}, observers)
	var wg sync.WaitGroup
	for i, w := range active {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			_, _, errs[i] = s.observe(ctx, w)
		})
	}
	wg.Wait()
	s.coordMu.Lock()
	leaseErr := s.expireLeases(ctx)
	s.coordMu.Unlock()
	errs = append(errs, leaseErr)
	return errors.Join(errs...)
}

// workspaceStatus returns the cached observation, capturing one only if the
// workspace has never been observed.
func (s *Service) workspaceStatus(ctx context.Context, id string) (workspaceView, error) {
	sub, w, _, err := s.boundWorkspace(ctx, id, "")
	if err != nil {
		return workspaceView{}, err
	}
	view := workspaceView{WorkspaceID: id, State: sub.State, Path: s.Provider.Path(id)}
	if sub.State == "closed" || sub.State == "selected" {
		return view, nil
	}
	view.BaseCommit, view.BaseTree = w.BaseCommit, w.BaseTree
	status, at, err := s.Store.Index.Observation(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		snap, at, err := s.observe(ctx, w)
		if err != nil {
			return workspaceView{}, err
		}
		view.WriteSet, view.Status, view.ObservedAt, view.Tree = snap.Files, snap.Status, at, snap.Tree
		return view, nil
	}
	if err != nil {
		return workspaceView{}, err
	}
	view.WriteSet = append([]string{}, slices.Sorted(maps.Keys(status))...)
	view.Status, view.ObservedAt = status, at
	return view, nil
}
