package core

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/workspace"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

func (s *Service) proposalMessage(ctx context.Context, w schema.Workspace, manifest, sid string) (string, error) {
	intent, err := creation[schema.Intent](ctx, s.Store.Index, w.IntentID, schema.KindIntent)
	if err != nil {
		return "", err
	}
	summary := strings.Split(strings.ReplaceAll(intent.Description, "\r", "\n"), "\n")[0]
	summary = strings.ReplaceAll(summary, "\x00", " ")
	return "Ledger: " + summary + "\n\nLedger-Change: " + w.ChangeID + "\nLedger-Intent: " + w.IntentID + "\nLedger-Session: " + sid + "\nLedger-Manifest: " + manifest + "\n", nil
}

func (s *Service) refreshWorkspace(ctx context.Context, id string) (any, error) {
	sub, w, _, err := s.boundWorkspace(ctx, id, "")
	if err != nil {
		return nil, err
	}
	path := s.Provider.Path(id)
	if sub.State == "closed" || sub.State == "selected" {
		return map[string]any{"workspace_id": id, "state": sub.State, "path": path}, nil
	}
	snap, err := s.Provider.Snapshot(ctx, w, workspace.IndexPath(s.Store.Dir+"/runtime", id), false)
	if err != nil {
		return nil, err
	}
	at := schema.Timestamp(time.Now())
	if err := s.Store.Index.SaveObservation(ctx, id, path, at, snap.Status); err != nil {
		return nil, err
	}
	return map[string]any{"workspace_id": id, "state": sub.State, "path": path, "base_commit": w.BaseCommit, "write_set": snap.Files, "status": snap.Status, "observed_at": at, "tree": snap.Tree}, nil
}
func (s *Service) observeSession(ctx context.Context, sid string) error {
	subjects, err := s.Store.Index.Subjects(ctx)
	if err != nil {
		return err
	}
	for _, sub := range subjects {
		if sub.Kind == "workspace" && sub.Owner == sid && (sub.State == "open" || sub.State == "proposed") && sub.Conflict == "" {
			if _, err := s.refreshWorkspace(ctx, sub.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Workspaces returns reconstructible active worktree paths for watcher setup.
func (s *Service) Workspaces(ctx context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subjects, err := s.Store.Index.Subjects(ctx)
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	for _, sub := range subjects {
		if sub.Kind == "workspace" && (sub.State == "open" || sub.State == "proposed") && sub.Conflict == "" {
			paths[sub.ID] = s.Provider.Path(sub.ID)
		}
	}
	return paths, nil
}

// Observe refreshes active workspaces on watcher events and the fallback poll.
func (s *Service) Observe(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects, err := s.Store.Index.Objects(ctx, schema.KindWorkspace)
	if err != nil {
		return err
	}
	type observed struct {
		w        schema.Workspace
		snapshot workspace.Snapshot
		at       string
		err      error
	}
	results := make(chan observed, len(objects))
	slots := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for _, raw := range objects {
		var w schema.Workspace
		if err := json.Unmarshal(raw, &w); err != nil {
			return err
		}
		sub, err := s.Store.Index.Subject(ctx, w.ID)
		if err != nil {
			return err
		}
		if sub == nil || sub.Conflict != "" || (sub.State != "open" && sub.State != "proposed") {
			continue
		}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			snap, err := s.Provider.Snapshot(ctx, w, workspace.IndexPath(s.Store.Dir+"/runtime", w.ID), false)
			results <- observed{w: w, snapshot: snap, at: schema.Timestamp(time.Now()), err: err}
		})
	}
	wg.Wait()
	close(results)
	var problems []error
	for result := range results {
		if result.err != nil {
			problems = append(problems, result.err)
			continue
		}
		if err := s.Store.Index.SaveObservation(ctx, result.w.ID, s.Provider.Path(result.w.ID), result.at, result.snapshot.Status); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)

}

func (s *Service) workspaceStatus(ctx context.Context, id string) (any, error) {
	sub, w, _, err := s.boundWorkspace(ctx, id, "")
	if err != nil {
		return nil, err
	}
	if sub.State == "closed" || sub.State == "selected" {
		return map[string]any{"workspace_id": id, "state": sub.State, "path": s.Provider.Path(id)}, nil
	}
	status, at, err := s.Store.Index.Observation(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return s.refreshWorkspace(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"workspace_id": id, "state": sub.State, "path": s.Provider.Path(id), "base_commit": w.BaseCommit, "write_set": slices.Sorted(maps.Keys(status)), "status": status, "observed_at": at}, nil
}
