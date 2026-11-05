package index

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// ActiveWorkspaces returns the creation records of unconflicted open or
// proposed workspaces, only those currently bound to owner if it is set.
func (x *Index) ActiveWorkspaces(ctx context.Context, owner string) ([]schema.Workspace, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT c.payload FROM subjects s JOIN creations c ON c.hash=s.creation
 WHERE s.kind='workspace' AND s.state IN ('open','proposed') AND s.conflict='' AND (?='' OR s.owner=?) ORDER BY s.id`, owner, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []schema.Workspace
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var w schema.Workspace
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// HasChildIntent reports whether any committed intent names id as its parent.
func (x *Index) HasChildIntent(ctx context.Context, id string) (bool, error) {
	return x.exists(ctx, `SELECT 1 FROM objects WHERE kind='intent' AND json_extract(payload,'$.parent_id')=?`, id)
}

// IntentHasChange reports whether any committed change manifest attaches to id.
func (x *Index) IntentHasChange(ctx context.Context, id string) (bool, error) {
	return x.exists(ctx, `SELECT 1 FROM objects WHERE kind='change_manifest' AND json_extract(payload,'$.intent_id')=?`, id)
}

func (x *Index) exists(ctx context.Context, query string, args ...any) (bool, error) {
	var found bool
	err := x.conn().QueryRowContext(ctx, `SELECT EXISTS(`+query+`)`, args...).Scan(&found)
	return found, err
}

// SaveObservation replaces a workspace's runtime write set.
func (x *Index) SaveObservation(ctx context.Context, id, path, at string, status map[string]string) error {
	tx, err := x.conn().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO workspace_paths VALUES (?,?,?)`, id, path, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM write_sets WHERE workspace_id=?`, id); err != nil {
		return err
	}
	for path, status := range status {
		if _, err := tx.ExecContext(ctx, `INSERT INTO write_sets VALUES (?,?,?)`, id, path, status); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Binding returns the latest binding payload, or the original workspace authority.
func (x *Index) Binding(ctx context.Context, s *Subject, w schema.Workspace) (string, string, error) {
	if s.BindingHead == s.Creation {
		return w.SessionID, w.CapabilityID, nil
	}
	var raw string
	if err := x.conn().QueryRowContext(ctx, `SELECT payload FROM objects WHERE hash=?`, s.BindingHead).Scan(&raw); err != nil {
		return "", "", err
	}
	var b schema.WorkspaceBinding
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return "", "", err
	}
	return b.SessionID, b.CapabilityID, nil
}

// Observation reads the last persisted runtime write set without touching files.
func (x *Index) Observation(ctx context.Context, id string) (map[string]string, string, error) {
	var at string
	if err := x.conn().QueryRowContext(ctx, `SELECT observed_at FROM workspace_paths WHERE workspace_id=?`, id).Scan(&at); err != nil {
		return nil, "", err
	}
	rows, err := x.conn().QueryContext(ctx, `SELECT path,status FROM write_sets WHERE workspace_id=? ORDER BY path`, id)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	status := map[string]string{}
	for rows.Next() {
		var p, s string
		if err := rows.Scan(&p, &s); err != nil {
			return nil, "", err
		}
		status[p] = s
	}
	return status, at, rows.Err()
}

// ProposalCommit follows the accepted lifecycle chain to the latest proposal
// receipt. The branch's current tip is not authority to overwrite external work.
func (x *Index) ProposalCommit(ctx context.Context, id string) (string, error) {
	sub, err := x.Subject(ctx, id)
	if err != nil {
		return "", err
	}
	if sub == nil {
		return "", apierr.New("unknown_entity", "no workspace %s", id)
	}
	head := sub.Head
	for head != "" {
		var raw string
		if err := x.conn().QueryRowContext(ctx, `SELECT payload FROM objects WHERE hash=?`, head).Scan(&raw); err != nil {
			return "", err
		}
		var event schema.LifecycleEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return "", err
		}
		if event.ToState == "proposed" {
			rows, err := x.conn().QueryContext(ctx, `SELECT o.result FROM operations o JOIN members m ON m.receipt=o.receipt WHERE m.hash=? AND o.operation='workspace.propose'`, head)
			if err != nil {
				return "", err
			}
			var result struct {
				Commit      string `json:"commit"`
				WorkspaceID string `json:"workspace_id"`
			}
			count := 0
			for rows.Next() {
				var raw string
				if err := rows.Scan(&raw); err != nil {
					rows.Close()
					return "", err
				}
				if err := json.Unmarshal([]byte(raw), &result); err != nil {
					rows.Close()
					return "", err
				}
				count++
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return "", err
			}
			if count != 1 || result.WorkspaceID != id || result.Commit == "" {
				return "", apierr.New("proposal_conflict", "workspace %s has no unambiguous proposal binding", id)
			}
			return result.Commit, nil
		}
		head = ""
		if event.Previous != nil {
			head = *event.Previous
		}
	}
	return "", nil
}

// ManifestCommit returns the unique previously receipted commit for an exact
// manifest. Re-proposing identical content must preserve its evidence binding.
func (x *Index) ManifestCommit(ctx context.Context, manifest string) (string, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT DISTINCT json_extract(op.result,'$.commit') FROM operations op JOIN members m ON m.receipt=op.receipt
 WHERE m.hash=? AND op.operation='workspace.propose' AND json_extract(op.result,'$.manifest')=?`, manifest, manifest)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var commit string
	for rows.Next() {
		if commit != "" {
			return "", apierr.New("proposal_conflict", "manifest has incompatible commit bindings")
		}
		if err := rows.Scan(&commit); err != nil {
			return "", err
		}
		if commit == "" {
			return "", apierr.New("proposal_conflict", "manifest has an empty commit binding")
		}
	}
	return commit, rows.Err()
}

// RootOperation finds an existing publication of this root authority. It
// allows init to reconstruct its local retry identity after config/index loss.
func (x *Index) RootOperation(ctx context.Context, fingerprint string) (string, error) {
	var id string
	err := x.conn().QueryRowContext(ctx, `SELECT op.operation_id FROM operations op JOIN members m ON m.receipt=op.receipt JOIN objects o ON o.hash=m.hash
 WHERE op.operation='ledger.init' AND o.kind='root_key' AND o.signer=? ORDER BY op.receipt LIMIT 1`, fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
