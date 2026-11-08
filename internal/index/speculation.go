package index

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/lifecycle"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Run is a committed speculation run creation record.
type Run struct {
	Creation string
	Signer   string
	schema.SpeculationRun
}

// RunOf returns the run whose candidate bindings name workspace id, or nil
// when the workspace belongs to no run.
func (x *Index) RunOf(ctx context.Context, workspaceID string) (*Run, error) {
	return runOf(ctx, x.conn(), workspaceID)
}

func runOf(ctx context.Context, q querier, workspaceID string) (*Run, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.hash, c.signer, c.payload FROM creations c, json_each(c.payload, '$.attempts') a
 WHERE c.kind='speculation_run' AND json_extract(a.value, '$.workspace_id')=? ORDER BY c.hash`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out *Run
	for rows.Next() {
		if out != nil {
			return nil, apierr.New("subject_conflict", "workspace %s is bound to more than one speculation run", workspaceID)
		}
		var r Run
		var payload string
		if err := rows.Scan(&r.Creation, &r.Signer, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &r.SpeculationRun); err != nil {
			return nil, err
		}
		out = &r
	}
	return out, rows.Err()
}

// RunCreation returns the committed creation record of run id.
func (x *Index) RunCreation(ctx context.Context, id string) (*Run, error) {
	return runCreation(ctx, x.conn(), id)
}

func runCreation(ctx context.Context, q querier, id string) (*Run, error) {
	var r Run
	var payload string
	err := q.QueryRowContext(ctx, `SELECT hash, signer, payload FROM creations WHERE entity_id=? AND kind='speculation_run' ORDER BY hash LIMIT 1`, id).
		Scan(&r.Creation, &r.Signer, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apierr.New("unknown_entity", "no speculation run %s", id)
	}
	if err != nil {
		return nil, err
	}
	return &r, json.Unmarshal([]byte(payload), &r.SpeculationRun)
}

// WorkspaceManifests returns the hashes of every committed change manifest
// of workspace id, ordered by hash.
func (x *Index) WorkspaceManifests(ctx context.Context, workspaceID string) ([]string, error) {
	return workspaceManifests(ctx, x.conn(), workspaceID)
}

func workspaceManifests(ctx context.Context, q querier, workspaceID string) ([]string, error) {
	return hashes(ctx, q, `SELECT hash FROM objects WHERE kind='change_manifest' AND json_extract(payload,'$.workspace_id')=? ORDER BY hash`, workspaceID)
}

// Attempt is a committed attempt record and its hash.
type Attempt struct {
	Hash string
	schema.AttemptRecord
}

// Attempts returns the committed attempt records of run id, ordered by hash.
func (x *Index) Attempts(ctx context.Context, runID string) ([]Attempt, error) {
	return attempts(ctx, x.conn(), runID)
}

func attempts(ctx context.Context, q querier, runID string) ([]Attempt, error) {
	rows, err := q.QueryContext(ctx, `SELECT hash, payload FROM objects WHERE kind='attempt_record' AND json_extract(payload,'$.run_id')=? ORDER BY hash`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var payload string
		if err := rows.Scan(&a.Hash, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &a.AttemptRecord); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RunSelections returns the hashes of selection decisions made by run id.
func (x *Index) RunSelections(ctx context.Context, runID string) ([]string, error) {
	return hashes(ctx, x.conn(), `SELECT hash FROM objects WHERE kind='selection_decision' AND json_extract(payload,'$.selector.kind')='speculation_run' AND json_extract(payload,'$.selector.id')=? ORDER BY hash`, runID)
}

// Events returns the accepted lifecycle chain of subject id, oldest first.
func (x *Index) Events(ctx context.Context, id string) ([]lifecycle.Event, error) {
	sub, err := loadSubject(ctx, x.conn(), id)
	if err != nil || sub == nil {
		return nil, err
	}
	var chain []lifecycle.Event
	for head := sub.Head; head != ""; {
		var raw string
		if err := x.conn().QueryRowContext(ctx, `SELECT payload FROM events WHERE hash=?`, head).Scan(&raw); err != nil {
			return nil, err
		}
		e := lifecycle.Event{Hash: head}
		if err := json.Unmarshal([]byte(raw), &e.LifecycleEvent); err != nil {
			return nil, err
		}
		chain = append(chain, e)
		head = ""
		if e.Previous != nil {
			head = *e.Previous
		}
	}
	slices.Reverse(chain)
	return chain, nil
}

func hashes(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
