package cli

import (
	"context"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/workspace"
)

// restoreWorkspaces reconstructs runtime bindings from fresh snapshots.
func restoreWorkspaces(ctx context.Context, s *record.Store, provider workspace.NativeProvider) ([]map[string]string, error) {
	active, err := s.Index.ActiveWorkspaces(ctx, "")
	if err != nil {
		return nil, err
	}
	problems := []map[string]string{}
	for _, w := range active {
		snap, err := provider.Snapshot(ctx, w)
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
