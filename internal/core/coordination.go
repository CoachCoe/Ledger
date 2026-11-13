package core

import (
	"context"
	"encoding/json/v2"
	"slices"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/coord"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/symbols"
	"github.com/CoachCoe/Ledger/internal/workspace"
)

type coordinatedWorkspace struct {
	Workspace schema.Workspace
	Subject   *index.Subject
	Analysis  symbols.Summary
	Tree      string
}

func (s *Service) activeCoordination(ctx context.Context) ([]coordinatedWorkspace, error) {
	active, err := s.Store.Index.ActiveWorkspaces(ctx, "")
	if err != nil {
		return nil, err
	}
	out := []coordinatedWorkspace{}
	for _, w := range active {
		sub, _, cid, err := s.boundWorkspace(ctx, w.ID, "")
		if err != nil {
			return nil, err
		}
		if err := s.authority(ctx, sub.Owner, cid); err != nil {
			continue
		}
		a, tree, err := s.Store.Index.Symbols(ctx, w.ID)
		if err != nil {
			return nil, err
		}
		if tree == "" {
			continue
		}
		out = append(out, coordinatedWorkspace{w, sub, a, tree})
	}
	return out, nil
}

func (s *Service) observeSymbols(ctx context.Context, w schema.Workspace, snap workspace.Snapshot, at string) error {
	// Serialize publication/comparison, not Git capture. The last observer
	// always compares against every previously published observation.
	s.coordMu.Lock()
	defer s.coordMu.Unlock()
	sub, _, _, err := s.boundWorkspace(ctx, w.ID, "")
	if err != nil {
		return err
	}
	if s.coordPublished == nil {
		s.coordPublished = map[string]string{}
	}
	_, tree, err := s.Store.Index.Symbols(ctx, w.ID)
	if err != nil {
		return err
	}
	proposal, err := s.Store.Index.LastProposal(ctx, w.ID)
	if err != nil {
		return err
	}
	stamp := sub.BindingHead + ":" + snap.Tree + ":" + proposal.Manifest
	if tree == snap.Tree && s.coordPublished[w.ID] == stamp {
		return nil
	}
	if tree != snap.Tree || s.coordPublished[w.ID] != stamp {
		contexts := []string{}
		if proposal.Manifest != "" {
			m, err := s.Store.Index.Manifest(ctx, proposal.Manifest)
			if err != nil {
				return err
			}
			contexts = m.ContextRefs
		}
		a, err := s.symbolEngine.Compare(ctx, s.symbolSource(), w.BaseTree, snap.Tree, snap.Files, contexts)
		if err != nil {
			return err
		}
		if err := s.Store.Index.SaveSymbols(ctx, w.ID, snap.Tree, at, a); err != nil {
			return err
		}
	}
	if err := s.expireLeases(ctx); err != nil {
		return err
	}
	views, err := s.activeCoordination(ctx)
	if err != nil {
		return err
	}
	for i, a := range views {
		for _, b := range views[i+1:] {
			if a.Subject.Owner == b.Subject.Owner {
				continue
			}
			for _, o := range coord.Overlaps(a.Analysis, b.Analysis) {
				key := a.Subject.BindingHead + b.Subject.BindingHead + a.Tree + b.Tree
				if err := s.overlapNotice(ctx, a.Subject.Owner, b, o, "overlap", key, at); err != nil {
					return err
				}
				if err := s.overlapNotice(ctx, b.Subject.Owner, a, o, "overlap", key, at); err != nil {
					return err
				}
			}
		}
	}
	if err := s.warnLeases(ctx, views, at); err != nil {
		return err
	}
	s.coordPublished[w.ID] = stamp
	return nil
}

func (s *Service) overlapNotice(ctx context.Context, recipient string, other coordinatedWorkspace, o coord.Overlap, kind, key, at string) error {
	raw, err := canon.Marshal(o)
	if err != nil {
		return err
	}
	id := "ntf_" + canon.Hash([]byte(kind + ":" + recipient + ":" + key + ":" + string(raw)))[:26]
	n := index.Notification{ID: id, Recipient: recipient, Kind: kind, Subject: index.NotificationSubject{Path: o.Path, Symbol: o.Symbol},
		Counterpart: &index.NotificationCounterpart{WorkspaceID: other.Workspace.ID, SessionID: other.Subject.Owner, IntentID: other.Workspace.IntentID},
		Confidence:  o.Confidence, OverlapKind: o.Kind, Reason: o.Reason, CreatedAt: at}
	return s.Store.Index.Enqueue(ctx, n)
}

func (s *Service) expireLeases(ctx context.Context) error {
	leases, err := s.Store.Index.Leases(ctx)
	if err != nil {
		return err
	}
	for _, l := range leases {
		exp, err := schema.ParseTimestamp(l.ExpiresAt)
		if err != nil {
			return err
		}
		session, sessionErr := creation[schema.Session](ctx, s.Store.Index, l.Holder, schema.KindSession)
		active := sessionErr == nil && s.authority(ctx, l.Holder, session.CapabilityID) == nil
		if !active || !time.Now().Before(exp) {
			if err := s.Store.Index.DeleteLease(ctx, l.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) warnLeases(ctx context.Context, views []coordinatedWorkspace, at string) error {
	leases, err := s.Store.Index.Leases(ctx)
	if err != nil {
		return err
	}
	for _, l := range leases {
		for _, v := range views {
			if l.Holder == v.Subject.Owner {
				continue
			}
			for _, o := range coord.LeaseConflicts(v.Analysis, l, s.caseSensitive) {
				key := l.ID + v.Subject.BindingHead + v.Tree
				if err := s.overlapNotice(ctx, l.Holder, v, o, "lease_conflict", key, at); err != nil {
					return err
				}
				// A lease may exist without a holder workspace. The subject still
				// identifies the leased code; counterpart is deliberately nullable.
				raw, _ := canon.Marshal(o)
				n := index.Notification{ID: "ntf_" + canon.Hash([]byte("lease_conflict:" + v.Subject.Owner + key + string(raw)))[:26], Recipient: v.Subject.Owner, Kind: "lease_conflict", Subject: index.NotificationSubject{Path: o.Path, Symbol: o.Symbol}, Confidence: o.Confidence, OverlapKind: o.Kind, Reason: "advisory_lease", CreatedAt: at}
				for _, holder := range views {
					if holder.Subject.Owner == l.Holder {
						n.Counterpart = &index.NotificationCounterpart{WorkspaceID: holder.Workspace.ID, SessionID: l.Holder, IntentID: holder.Workspace.IntentID}
						break
					}
				}
				if err := s.Store.Index.Enqueue(ctx, n); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func runtimeResult(fields map[string]any) (any, error) {
	fields["runtime"], fields["observed_at"] = true, now()
	raw, err := canon.Marshal(fields)
	if err != nil {
		return nil, err
	}
	fields["snapshot_digest"] = canon.Hash(raw)
	return fields, nil
}

func (s *Service) coordination(ctx context.Context, r api.Request) (any, error) {
	if r.Operation == "coordination.status" {
		in, err := decode[workspaceInput](r.Input)
		if err != nil {
			return nil, err
		}
		_, w, _, err := s.boundWorkspace(ctx, in.WorkspaceID, "")
		if err != nil {
			return nil, err
		}
		if _, _, err := s.observe(ctx, w); err != nil {
			return nil, err
		}
		a, _, err := s.Store.Index.Symbols(ctx, w.ID)
		if err != nil {
			return nil, err
		}
		return runtimeResult(map[string]any{"workspace_id": w.ID, "analysis": a})
	}
	var in struct {
		SessionID string          `json:"session_id"`
		LeaseID   string          `json:"lease_id"`
		Paths     []string        `json:"paths"`
		Symbols   []schema.Symbol `json:"symbols"`
		Reason    string          `json:"reason"`
		ExpiresAt string          `json:"expires_at"`
	}
	if err := json.Unmarshal(r.Input, &in, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	// Lease mutations share the authority lock with session lifecycle changes.
	// An end cannot pass the key/authority check and race lease publication.
	if r.Operation != "coordination.leases" {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if r.Operation != "coordination.leases" {
		if r.SessionID != "" && r.SessionID != in.SessionID {
			return nil, apierr.New("unauthorized", "lease belongs to another session")
		}
		available := s.signers[in.SessionID] != nil
		if !available {
			return nil, apierr.New("session_key_unavailable", "lease holder must have an active local session")
		}
		session, err := creation[schema.Session](ctx, s.Store.Index, in.SessionID, schema.KindSession)
		if err != nil {
			return nil, err
		}
		if err := s.authority(ctx, in.SessionID, session.CapabilityID); err != nil {
			return nil, err
		}
		if r.SessionID == "" {
			if _, err := s.root(); err != nil {
				return nil, err
			}
		}
	}
	s.coordMu.Lock()
	defer s.coordMu.Unlock()
	if err := s.expireLeases(ctx); err != nil {
		return nil, err
	}
	if r.Operation == "coordination.leases" {
		leases, err := s.Store.Index.Leases(ctx)
		if err != nil {
			return nil, err
		}
		return runtimeResult(map[string]any{"leases": leases})
	}
	if r.Operation == "coordination.release" {
		leases, err := s.Store.Index.Leases(ctx)
		if err != nil {
			return nil, err
		}
		for _, l := range leases {
			if l.ID == in.LeaseID && l.Holder != in.SessionID {
				return nil, apierr.New("unauthorized", "lease belongs to another session")
			}
		}
		if err := s.Store.Index.DeleteLease(ctx, in.LeaseID); err != nil {
			return nil, err
		}
		return runtimeResult(map[string]any{"released": true})
	}
	expires, err := schema.ParseTimestamp(in.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if !time.Now().Before(expires) || expires.After(time.Now().Add(24*time.Hour)) {
		return nil, apierr.New("invalid_expiry", "lease expiry must be in the next 24 hours")
	}
	if len(in.Paths)+len(in.Symbols) == 0 {
		return nil, apierr.New("invalid_lease", "lease needs a path or symbol")
	}
	session, err := creation[schema.Session](ctx, s.Store.Index, in.SessionID, schema.KindSession)
	if err != nil {
		return nil, err
	}
	chain, err := s.chain(ctx, session.CapabilityID, time.Now())
	if err != nil {
		return nil, err
	}
	paths := slices.Clone(in.Paths)
	for _, symbol := range in.Symbols {
		paths = append(paths, symbol.Path)
	}
	for _, p := range paths {
		if _, err := capability.Normalize(p, s.caseSensitive); err != nil {
			return nil, err
		}
		if err := capability.Authorize(chain, p, s.caseSensitive, time.Now()); err != nil {
			return nil, err
		}
	}
	id := "lse_" + canon.Hash([]byte(in.SessionID + ":" + r.OperationID))[:26]
	l := coord.Lease{ID: id, Holder: in.SessionID, Paths: in.Paths, Symbols: in.Symbols, Reason: in.Reason, ExpiresAt: in.ExpiresAt}
	if err := s.Store.Index.PutLease(ctx, l); err != nil {
		return nil, err
	}
	views, err := s.activeCoordination(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.warnLeases(ctx, views, now()); err != nil {
		return nil, err
	}
	return runtimeResult(map[string]any{"lease": l})
}
