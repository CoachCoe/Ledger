package index

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// ActiveWorkspaces returns the creation records of unconflicted open or
// proposed workspaces, only those currently bound to owner if it is set.
func (x *Index) ActiveWorkspaces(ctx context.Context, owner string) ([]schema.Workspace, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT c.payload, c.version FROM subjects s JOIN creations c ON c.hash=s.creation
 WHERE s.kind='workspace' AND s.state IN ('open','proposed') AND s.conflict='' AND (?='' OR s.owner=?) ORDER BY s.id`, owner, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []schema.Workspace
	for rows.Next() {
		var raw []byte
		var version int
		if err := rows.Scan(&raw, &version); err != nil {
			return nil, err
		}
		var w schema.Workspace
		if err := schema.Decode(version, schema.KindWorkspace, raw, &w); err != nil {
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

// Proposal is the commit and branch a workspace's latest proposal published.
// Proposal is a workspace's proposal binding. Commit and Branch are empty
// for a v2 manifest, which binds a store tree.
type Proposal struct {
	Manifest string `json:"manifest"`
	Commit   string `json:"commit"`
	Branch   string `json:"branch"`
}

// LastProposal follows the accepted lifecycle chain to the latest proposal
// receipt. The branch's current tip is not authority to overwrite external work.
func (x *Index) LastProposal(ctx context.Context, id string) (Proposal, error) {
	return lastProposal(ctx, x.conn(), id)
}

func lastProposal(ctx context.Context, q querier, id string) (Proposal, error) {
	sub, err := loadSubject(ctx, q, id)
	if err != nil {
		return Proposal{}, err
	}
	if sub == nil {
		return Proposal{}, apierr.New("unknown_entity", "no workspace %s", id)
	}
	head := sub.Head
	for head != "" {
		var raw string
		if err := q.QueryRowContext(ctx, `SELECT payload FROM objects WHERE hash=?`, head).Scan(&raw); err != nil {
			return Proposal{}, err
		}
		var event schema.LifecycleEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return Proposal{}, err
		}
		if event.ToState == "proposed" {
			rows, err := q.QueryContext(ctx, `SELECT o.result FROM operations o JOIN members m ON m.receipt=o.receipt WHERE m.hash=? AND o.operation IN ('workspace.propose','speculation.submit')`, head)
			if err != nil {
				return Proposal{}, err
			}
			var result struct {
				Proposal    `json:",inline"`
				WorkspaceID string `json:"workspace_id"`
			}
			count := 0
			for rows.Next() {
				var raw string
				if err := rows.Scan(&raw); err != nil {
					rows.Close()
					return Proposal{}, err
				}
				if err := json.Unmarshal([]byte(raw), &result); err != nil {
					rows.Close()
					return Proposal{}, err
				}
				count++
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return Proposal{}, err
			}
			if count != 1 || result.WorkspaceID != id || result.Manifest == "" {
				return Proposal{}, apierr.New("proposal_conflict", "workspace %s has no unambiguous proposal binding", id)
			}
			if result.Commit == "" {
				m, err := manifestIn(ctx, q, result.Manifest)
				if err != nil {
					return Proposal{}, err
				}
				if m.BaseTree == "" {
					return Proposal{}, apierr.New("proposal_conflict", "workspace %s has no unambiguous proposal binding", id)
				}
			}
			return result.Proposal, nil
		}
		head = ""
		if event.Previous != nil {
			head = *event.Previous
		}
	}
	return Proposal{}, nil
}

// ManifestCommit returns the unique previously receipted commit for an exact
// manifest. Re-proposing identical content must preserve its evidence binding.
func (x *Index) ManifestCommit(ctx context.Context, manifest string) (string, error) {
	return manifestCommit(ctx, x.conn(), manifest)
}

func manifestCommit(ctx context.Context, q querier, manifest string) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT json_extract(op.result,'$.commit') FROM operations op JOIN members m ON m.receipt=op.receipt
 WHERE m.hash=? AND op.operation IN ('workspace.propose','speculation.submit') AND json_extract(op.result,'$.manifest')=?`, manifest, manifest)
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

// ChangeWorkspace returns the unique committed workspace carrying a change id.
func (x *Index) ChangeWorkspace(ctx context.Context, changeID string) (string, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT json_extract(payload,'$.id') FROM objects WHERE kind='workspace' AND json_extract(payload,'$.change_id')=?`, changeID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var id string
	for rows.Next() {
		if id != "" {
			return "", apierr.New("conflicting_creation", "change %s belongs to more than one workspace", changeID)
		}
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if id == "" {
		return "", apierr.New("unknown_entity", "no change %s", changeID)
	}
	return id, nil
}

// Manifest returns a committed change manifest by hash.
func (x *Index) Manifest(ctx context.Context, hash string) (schema.ChangeManifest, error) {
	return manifestIn(ctx, x.conn(), hash)
}

func manifestIn(ctx context.Context, q querier, hash string) (schema.ChangeManifest, error) {
	var m schema.ChangeManifest
	var raw string
	var version int
	err := q.QueryRowContext(ctx, `SELECT payload, version FROM objects WHERE hash=? AND kind='change_manifest'`, hash).Scan(&raw, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return m, apierr.New("unknown_manifest", "no committed change manifest %s", hash)
	}
	if err != nil {
		return m, err
	}
	return m, schema.Decode(version, schema.KindChangeManifest, []byte(raw), &m)
}

// CheckDefinition returns a committed check definition record by hash.
func (x *Index) CheckDefinition(ctx context.Context, hash string) (schema.CheckDefinition, error) {
	var d schema.CheckDefinition
	var raw string
	err := x.conn().QueryRowContext(ctx, `SELECT payload FROM objects WHERE hash=? AND kind='check_definition'`, hash).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return d, apierr.New("unknown_check_definition", "no committed check definition %s", hash)
	}
	if err != nil {
		return d, err
	}
	return d, json.Unmarshal([]byte(raw), &d)
}

// WorkspaceHolders returns every session ever bound to workspace id: the
// creating session and each later binding's session.
func (x *Index) WorkspaceHolders(ctx context.Context, id string) ([]string, error) {
	return workspaceHolders(ctx, x.conn(), id)
}

func workspaceHolders(ctx context.Context, q querier, id string) ([]string, error) {
	cr, err := creation(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if cr.Kind != schema.KindWorkspace {
		return nil, apierr.New("unknown_entity", "no workspace %s", id)
	}
	var w schema.Workspace
	if err := cr.Decode(&w); err != nil {
		return nil, err
	}
	holders := []string{w.SessionID}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT session_id FROM bindings WHERE workspace_id=? ORDER BY session_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		if !slices.Contains(holders, sid) {
			holders = append(holders, sid)
		}
	}
	return holders, rows.Err()
}

// AttestationsFor returns the hashes of committed evidence attestations that
// reference manifest, ordered by hash. Only objects of indexed groups exist
// in the index, so uncommitted attestations never appear.
func (x *Index) AttestationsFor(ctx context.Context, manifest string) ([]string, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT hash FROM objects WHERE kind='evidence_attestation' AND json_extract(payload,'$.manifest')=? ORDER BY hash`, manifest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hashes := []string{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}
	return hashes, rows.Err()
}

// SelectionsFor returns the hashes of the committed selection decisions that
// name manifest, ordered by hash. The lookup uses objects_selection_manifest.
func (x *Index) SelectionsFor(ctx context.Context, manifest string) ([]string, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT hash FROM objects WHERE kind='selection_decision' AND json_extract(payload,'$.manifest')=? ORDER BY hash`, manifest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hashes := []string{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}
	return hashes, rows.Err()
}
