package index

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func (x *Index) caseSensitive() bool {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(x.path), "config.json"))
	if err != nil {
		return true
	}
	var cfg struct {
		CaseSensitive *bool `json:"case_sensitive"`
	}
	if json.Unmarshal(raw, &cfg) != nil || cfg.CaseSensitive == nil {
		return true
	}
	return *cfg.CaseSensitive
}

// verifyAuthority runs on replay/import as well as local publication. It
// evaluates authority at the recorded operation time, never today's wall clock
// or today's session state. Signatures were already authenticated by verify.
func verifyAuthority(ctx context.Context, q querier, g Group, r schema.OperationReceipt, sensitive bool) error {
	v := &validator{ctx: ctx, q: q, local: map[string]envelope.Object{}, objects: map[string]envelope.Object{}, sims: map[string]*Subject{}, at: r.At, sensitive: sensitive}
	for _, o := range g.Members {
		v.objects[o.Hash] = o
		id, err := entityID(o)
		if err != nil {
			return err
		}
		if id != "" {
			if prior, ok := v.local[id]; ok && prior.Hash != o.Hash {
				return apierr.New("conflicting_creation", "two creations for %s", id)
			}
			v.local[id] = o
		}
	}
	at, err := schema.ParseTimestamp(r.At)
	if err != nil {
		return err
	}
	load := func(id string) (schema.Capability, error) {
		c, err := v.capability(id)
		if err != nil {
			return schema.Capability{}, err
		}
		return *c, nil
	}
	for _, o := range append(append([]envelope.Object{}, g.Members...), g.Receipt) {
		if isSession(o.Envelope.Signer) {
			var session schema.Session
			if local, ok := v.local[o.Envelope.Signer]; ok {
				if err := local.Decode(&session); err != nil {
					return err
				}
			} else {
				cr, err := creation(ctx, q, o.Envelope.Signer)
				if err != nil {
					return err
				}
				if cr.Kind != schema.KindSession {
					return unauthorized("signer is not a session")
				}
				if err := cr.Decode(&session); err != nil {
					return err
				}
			}
			started, err := schema.ParseTimestamp(session.StartedAt)
			if err != nil {
				return err
			}
			if at.Before(started) {
				return unauthorized("operation precedes session")
			}
			if _, err := capability.Chain(session.CapabilityID, load, sensitive, at); err != nil {
				return err
			}
			// A later end does not invalidate history; an earlier end does.
			rows, err := q.QueryContext(ctx, `SELECT payload FROM events WHERE subject_id=?`, session.ID)
			if err != nil {
				return err
			}
			ended := false
			for rows.Next() {
				var raw string
				if err := rows.Scan(&raw); err != nil {
					rows.Close()
					return err
				}
				var e schema.LifecycleEvent
				if err := json.Unmarshal([]byte(raw), &e); err != nil {
					rows.Close()
					return err
				}
				t, err := schema.ParseTimestamp(e.At)
				if err != nil {
					rows.Close()
					return err
				}
				if e.ToState == "ended" && t.Before(at) {
					ended = true
				}
			}
			rowErr := rows.Err()
			rows.Close()
			if rowErr != nil {
				return rowErr
			}
			if ended {
				return apierr.New("session_inactive", "operation follows session end")
			}
		}
		switch o.Envelope.Kind {
		case schema.KindCapability:
			var c schema.Capability
			if err := o.Decode(&c); err != nil {
				return err
			}
			if err := v.checkCapability(&c, o.Envelope.Signer); err != nil {
				return err
			}
			if _, err := capability.Chain(c.ID, load, sensitive, at); err != nil {
				return err
			}
		case schema.KindWorkspace:
			var w schema.Workspace
			if err := o.Decode(&w); err != nil {
				return err
			}
			if isSession(o.Envelope.Signer) && o.Envelope.Signer != w.SessionID {
				return unauthorized("workspace signer is not its holder")
			}
			chain, err := capability.Chain(w.CapabilityID, load, sensitive, at)
			if err != nil {
				return err
			}
			if chain[0].SessionID != w.SessionID {
				return unauthorized("workspace capability holder differs")
			}
		case schema.KindWorkspaceBinding:
			var b schema.WorkspaceBinding
			if err := o.Decode(&b); err != nil {
				return err
			}
			chain, err := capability.Chain(b.CapabilityID, load, sensitive, at)
			if err != nil {
				return err
			}
			if chain[0].SessionID != b.SessionID {
				return unauthorized("replacement capability holder differs")
			}
		case schema.KindChangeManifest:
			var m schema.ChangeManifest
			if err := o.Decode(&m); err != nil {
				return err
			}
			if m.AuthorSession != o.Envelope.Signer {
				return unauthorized("manifest author differs from signer")
			}
			// Recover the binding that existed at this operation time. Immutable prior
			// manifests retain their original holder when later recovery appends a bind.
			cr, err := creation(ctx, q, m.WorkspaceID)
			if err != nil {
				return err
			}
			if cr.Kind == "" {
				return apierr.New(CodeUnavailable, "missing manifest workspace %s", m.WorkspaceID)
			}
			if cr.Kind != schema.KindWorkspace {
				return unauthorized("manifest has no workspace")
			}
			var w schema.Workspace
			if err := cr.Decode(&w); err != nil {
				return err
			}
			if m.BaseCommit != w.BaseCommit || m.BaseTree != w.BaseTree || m.IntentID != w.IntentID || m.ChangeID != w.ChangeID {
				return unauthorized("manifest workspace binding differs")
			}
			owner, cid, err := historicalBinding(ctx, q, w, at)
			if err != nil {
				return err
			}
			if owner != m.AuthorSession {
				if m.AuthorSession != w.SessionID {
					var n int
					if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM bindings WHERE workspace_id=? AND session_id=?`, w.ID, m.AuthorSession).Scan(&n); err != nil {
						return err
					}
					if n == 0 {
						return apierr.New(CodeUnavailable, "missing historical author binding")
					}
				}
				return unauthorized("manifest author is not the historical holder")
			}
			chain, err := capability.Chain(cid, load, sensitive, at)
			if err != nil {
				return err
			}
			for _, p := range m.Files {
				if err := capability.Authorize(chain, p, sensitive, at); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func historicalBinding(ctx context.Context, q querier, w schema.Workspace, at time.Time) (string, string, error) {
	rows, err := q.QueryContext(ctx, `SELECT o.payload FROM bindings b JOIN objects o ON o.hash=b.hash WHERE b.workspace_id=?`, w.ID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	var bs []schema.WorkspaceBinding
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return "", "", err
		}
		var b schema.WorkspaceBinding
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			return "", "", err
		}
		t, err := schema.ParseTimestamp(b.At)
		if err != nil {
			return "", "", err
		}
		if !t.After(at) {
			bs = append(bs, b)
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	// Follow predecessors rather than choosing a timestamp when branches fork.
	var head string
	if err := q.QueryRowContext(ctx, `SELECT hash FROM creations WHERE entity_id=?`, w.ID).Scan(&head); err != nil {
		return "", "", err
	}
	owner, cid := w.SessionID, w.CapabilityID
	for range len(bs) + 1 {
		found := -1
		for i, b := range bs {
			if b.Previous == head {
				if found >= 0 {
					return "", "", apierr.New("binding_fork", "historical binding conflict")
				}
				found = i
			}
		}
		if found < 0 {
			return owner, cid, nil
		}
		b := bs[found]
		owner, cid = b.SessionID, b.CapabilityID
		// The hash is available through the immutable payload's object row.
		if err := q.QueryRowContext(ctx, `SELECT hash FROM objects WHERE kind='workspace_binding' AND json_extract(payload,'$.workspace_id')=? AND json_extract(payload,'$.previous')=? AND json_extract(payload,'$.session_id')=?`, w.ID, b.Previous, b.SessionID).Scan(&head); err != nil {
			return "", "", err
		}
	}
	return "", "", apierr.New("binding_fork", "binding cycle")
}

// eventOwner derives authorization without relying on today's workspace owner.
func eventOwner(ctx context.Context, q querier, e schema.LifecycleEvent) (string, error) {
	var kind schema.Kind
	var raw string
	var version int
	if err := q.QueryRowContext(ctx, `SELECT kind,payload,version FROM objects WHERE hash=?`, e.Creation).Scan(&kind, &raw, &version); err != nil {
		return "", err
	}
	switch kind {
	case schema.KindWorkspace:
		var w schema.Workspace
		if err := schema.Decode(version, kind, []byte(raw), &w); err != nil {
			return "", err
		}
		at, err := schema.ParseTimestamp(e.At)
		if err != nil {
			return "", err
		}
		owner, _, err := historicalBinding(ctx, q, w, at)
		if apierr.HasCode(err, "binding_fork") {
			return "", nil
		}
		return owner, err
	case schema.KindSession:
		return e.SubjectID, nil
	case schema.KindIntent:
		var p schema.Intent
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return "", err
		}
		return p.Owner, nil
	default:
		var signer string
		err := q.QueryRowContext(ctx, `SELECT signer FROM objects WHERE hash=?`, e.Creation).Scan(&signer)
		return signer, err
	}
}
