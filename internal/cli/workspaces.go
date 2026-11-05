package cli

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/workspace"
)

// restoreWorkspaces reconstructs runtime bindings from Git metadata and
// fresh private snapshots. It never shares a live observer's private index.
func restoreWorkspaces(ctx context.Context, s *record.Store) ([]map[string]string, error) {
	subjects, err := s.Index.Subjects(ctx)
	if err != nil {
		return nil, err
	}
	provider := workspace.GitProvider{Repo: s.Repo}
	problems := []map[string]string{}
	for _, sub := range subjects {
		if sub.Kind != "workspace" || sub.Conflict != "" || (sub.State != "open" && sub.State != "proposed") {
			continue
		}
		object, err := s.Read(sub.Creation)
		if err != nil {
			return nil, err
		}
		var w schema.Workspace
		if err := object.Decode(&w); err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(filepath.Join(s.Dir, "runtime"), "reindex-*.index")
		if err != nil {
			return nil, err
		}
		name := tmp.Name()
		tmp.Close()
		os.Remove(name)
		snap, err := provider.Snapshot(ctx, w, name, false)
		os.Remove(name)
		os.Remove(name + ".lock")
		if err != nil {
			problems = append(problems, map[string]string{"workspace_id": w.ID, "code": apierr.As(err).Code, "message": err.Error()})
			continue
		}
		if err := s.Index.SaveObservation(ctx, w.ID, provider.Path(w.ID), schema.Timestamp(time.Now()), snap.Status); err != nil {
			return nil, err
		}
	}
	return problems, nil
}
