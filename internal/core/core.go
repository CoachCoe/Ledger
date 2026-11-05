// Package core implements the versioned API independently of transports.
package core

import (
	"context"
	"encoding/json/v2"
	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/workspace"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type Service struct {
	mu              sync.Mutex
	Store           *record.Store
	Keys            identity.IdentityProvider
	Provider        workspace.WorkspaceProvider
	signers         map[string]identity.Signer
	pendingSigners  map[string]identity.Signer
	recoveryPending bool
	rootID          string
}

func New(ctx context.Context, store *record.Store, keys identity.IdentityProvider) (*Service, error) {
	p := workspace.GitProvider{Repo: store.Repo}
	store.SetEffectHandler(p.Apply)
	s := &Service{Store: store, Keys: keys, Provider: p, signers: map[string]identity.Signer{}}
	var config struct {
		Root string `json:"root_fingerprint"`
	}
	if b, err := os.ReadFile(filepath.Join(store.Dir, "config.json")); err == nil {
		if err := json.Unmarshal(b, &config); err != nil {
			return nil, err
		}
		s.rootID = config.Root
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	stale, err := filepath.Glob(filepath.Join(store.Dir, "runtime", "observe-*.index.lock"))
	if err != nil {
		return nil, err
	}
	for _, path := range stale {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if _, err := store.Recover(ctx); err != nil {
		return nil, err
	}
	if err := s.reconcile(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Service) root() (identity.Signer, error) {
	r, err := s.Keys.Root()
	if err != nil {
		return nil, err
	}
	if s.rootID != "" && r.ID() != s.rootID {
		return nil, apierr.New("root_mismatch", "key does not match repository root")
	}
	return r, nil
}
func (s *Service) reconcile(ctx context.Context) error {
	subjects, err := s.Store.Index.Subjects(ctx)
	if err != nil {
		return err
	}
	for _, sub := range subjects {
		if sub.Kind != "session" || sub.State != "active" || sub.Conflict != "" || s.signers[sub.ID] != nil {
			continue
		}
		root, err := s.root()
		if err != nil {
			s.recoveryPending = true
			return nil
		}
		reason := "daemon_restart"
		event, err := s.event(sub, "ended", &reason, root)
		if err != nil {
			return err
		}
		hash := canon.Hash([]byte("daemon_restart:" + sub.Creation))
		_, err = s.Store.Do(ctx, record.Op{ID: "op_" + hash[:26], Name: "session.restart", Digest: hash, Signer: root}, func(*index.Index) (any, []envelope.Object, error) {
			return map[string]string{"session_id": sub.ID}, []envelope.Object{event}, nil
		})
		if err != nil {
			return err
		}
	}
	s.recoveryPending = false
	return nil
}
func (s *Service) event(sub index.Subject, to string, reason *string, signer identity.Signer) (envelope.Object, error) {
	var prev *string
	if sub.Head != "" {
		p := sub.Head
		prev = &p
	}
	return envelope.Seal(schema.KindLifecycleEvent, schema.LifecycleEvent{SubjectKind: sub.Kind, SubjectID: sub.ID, Creation: sub.Creation, Previous: prev, FromState: sub.State, ToState: to, Reason: reason, Actor: signer.ID(), At: schema.Timestamp(time.Now())}, signer)
}
func (s *Service) subject(ctx context.Context, id, kind string) (*index.Subject, error) {
	sub, err := s.Store.Index.Subject(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub == nil || sub.Kind != kind {
		return nil, apierr.New("unknown_entity", "no %s %s", kind, id)
	}
	if sub.Conflict != "" {
		return nil, apierr.New("subject_conflict", "%s: %s", id, sub.Conflict)
	}
	return sub, nil
}
func creation[T any](ctx context.Context, idx *index.Index, id string, kind schema.Kind) (T, error) {
	var p T
	k, raw, err := idx.Creation(ctx, id)
	if err != nil {
		return p, err
	}
	if k != kind {
		return p, apierr.New("unknown_entity", "no %s %s", kind, id)
	}
	return p, json.Unmarshal(raw, &p)
}
func (s *Service) Handle(ctx context.Context, r api.Request) (any, error) {
	raw, err := canon.Marshal(r)
	if err != nil {
		return nil, err
	}
	if _, err := api.Parse(raw); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryPending {
		if err := s.reconcile(ctx); err != nil {
			return nil, err
		}
	}
	// Every API call by a session refreshes its bound active workspaces.
	if r.SessionID != "" {
		if err := s.observeSession(ctx, r.SessionID); err != nil {
			return nil, err
		}
	}
	if api.ReadOnly(r.Operation) {
		return s.read(ctx, r)
	}
	digest, err := r.Digest()
	if err != nil {
		return nil, err
	}
	var signer identity.Signer
	var keyErr error
	if r.SessionID == "" {
		signer, keyErr = s.root()
	} else {
		signer = s.signers[r.SessionID]
		if signer == nil {
			keyErr = apierr.New("session_key_unavailable", "session key was lost or ended; use explicit recovery")
		}
	}
	s.pendingSigners = map[string]identity.Signer{}
	result, opErr := s.Store.DoEffects(ctx, record.Op{ID: r.OperationID, Name: r.Operation, Digest: digest, Signer: signer}, func(idx *index.Index) (any, []envelope.Object, []record.Effect, error) {
		if keyErr != nil {
			return nil, nil, nil, keyErr
		}
		if r.SessionID != "" {
			session, err := creation[schema.Session](ctx, idx, r.SessionID, schema.KindSession)
			if err != nil {
				return nil, nil, nil, err
			}
			if err := s.authority(ctx, r.SessionID, session.CapabilityID); err != nil {
				return nil, nil, nil, err
			}
		}
		return s.mutate(ctx, r, signer)
	})
	if opErr == nil {
		for id, key := range s.pendingSigners {
			s.signers[id] = key
		}
	}
	s.pendingSigners = nil
	if opErr != nil {
		s.recoveryPending = true
	}
	for id := range s.signers {
		sub, err := s.Store.Index.Subject(ctx, id)
		if err == nil && sub != nil && sub.State == "ended" {
			delete(s.signers, id)
		}
	}
	return result, opErr
}
func (s *Service) read(ctx context.Context, r api.Request) (any, error) {
	var in struct {
		IntentID    string `json:"intent_id"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(r.Input, &in); err != nil {
		return nil, err
	}
	switch r.Operation {
	case "system.status":
		subjects, err := s.Store.Index.Subjects(ctx)
		return map[string]any{"subjects": subjects, "recovery_pending": s.recoveryPending, "process_id": os.Getpid()}, err
	case "intent.status":
		sub, err := s.subject(ctx, in.IntentID, "intent")
		if err != nil {
			return nil, err
		}
		p, err := creation[schema.Intent](ctx, s.Store.Index, in.IntentID, schema.KindIntent)
		return map[string]any{"subject": sub, "intent": p}, err
	case "workspace.status":
		return s.workspaceStatus(ctx, in.WorkspaceID)
	}
	return nil, apierr.New("unknown_operation", "%s", r.Operation)
}
func (s *Service) sessionRecords(agent, model, kind, expiry string, root identity.Signer) (map[string]any, []envelope.Object, identity.Signer, error) {
	sid, cid := schema.NewID(schema.PrefixSession), schema.NewID(schema.PrefixCapability)
	key, err := identity.NewSession(sid)
	if err != nil {
		return nil, nil, nil, err
	}
	if kind == "" {
		kind = "agent"
	}
	now := time.Now()
	if expiry == "" {
		expiry = schema.Timestamp(now.Add(24 * time.Hour))
	}
	end, err := schema.ParseTimestamp(expiry)
	if err != nil || !end.After(now) {
		return nil, nil, nil, apierr.New("invalid_expiry", "session capability must expire in the future")
	}
	cap, err := envelope.Seal(schema.KindCapability, schema.Capability{ID: cid, SessionID: sid, Issuer: schema.Issuer{Kind: "root", Fingerprint: root.ID()}, Allow: []string{"**"}, Deny: []string{}, MayAddDependencies: true, ExpiresAt: expiry, CreatedAt: schema.Timestamp(now)}, root)
	if err != nil {
		return nil, nil, nil, err
	}
	var agentPtr, modelPtr *string
	if agent != "" {
		agentPtr = &agent
	}
	if model != "" {
		modelPtr = &model
	}
	session, err := envelope.Seal(schema.KindSession, schema.Session{ID: sid, CapabilityID: cid, PublicKey: identity.EncodePublicKey(key.PublicKey()), ParticipantKind: kind, AgentName: agentPtr, Model: modelPtr, StartedAt: schema.Timestamp(now)}, root)
	return map[string]any{"session_id": sid, "capability_id": cid, "session_hash": session.Hash, "capability_hash": cap.Hash}, []envelope.Object{cap, session}, key, err
}
func (s *Service) mutate(ctx context.Context, r api.Request, signer identity.Signer) (any, []envelope.Object, []record.Effect, error) {
	var in struct {
		AgentName       string             `json:"agent_name"`
		Model           string             `json:"model"`
		ParticipantKind string             `json:"participant_kind"`
		ExpiresAt       string             `json:"expires_at"`
		SessionID       string             `json:"session_id"`
		IntentID        string             `json:"intent_id"`
		WorkspaceID     string             `json:"workspace_id"`
		CapabilityID    string             `json:"capability_id"`
		BaseCommit      string             `json:"base_commit"`
		Description     string             `json:"description"`
		Criteria        []schema.Criterion `json:"criteria"`
		ParentID        *string            `json:"parent_id"`
		Source          *schema.Source     `json:"source"`
		Reason          string             `json:"reason"`
		ReplacementID   *string            `json:"replacement_id"`
		Plan            *string            `json:"plan"`
		ContextRefs     []string           `json:"context_refs"`
	}
	if err := json.Unmarshal(r.Input, &in, json.RejectUnknownMembers(true)); err != nil {
		return nil, nil, nil, err
	}
	switch r.Operation {
	case "daemon.stop":
		if r.SessionID != "" {
			return nil, nil, nil, apierr.New("unauthorized", "daemon stop requires root authorization")
		}
		return map[string]bool{"stopping": true}, nil, nil, nil
	case "session.create", "session.recover":
		if r.SessionID != "" {
			return nil, nil, nil, apierr.New("unauthorized", "root authorization required")
		}
		var sub *index.Subject
		if r.Operation == "session.recover" {
			var err error
			sub, err = s.subject(ctx, in.WorkspaceID, "workspace")
			if err != nil {
				return nil, nil, nil, err
			}
			if sub.State != "open" && sub.State != "proposed" {
				return nil, nil, nil, apierr.New("invalid_state", "terminal workspace cannot rebind")
			}
		}
		result, objects, key, err := s.sessionRecords(in.AgentName, in.Model, in.ParticipantKind, in.ExpiresAt, signer)
		if err != nil {
			return nil, nil, nil, err
		}
		if sub != nil {
			b, err := envelope.Seal(schema.KindWorkspaceBinding, schema.WorkspaceBinding{WorkspaceID: sub.ID, Previous: sub.BindingHead, SessionID: key.ID(), CapabilityID: result["capability_id"].(string), Actor: signer.ID(), Reason: "explicit_recovery", At: schema.Timestamp(time.Now())}, signer)
			if err != nil {
				return nil, nil, nil, err
			}
			objects = append(objects, b)
			result["workspace_id"] = sub.ID
		}
		// Stage key custody until the signed operation has committed.
		s.pendingSigners[key.ID()] = key
		return result, objects, nil, nil
	case "session.end":
		sub, err := s.subject(ctx, in.SessionID, "session")
		if err != nil {
			return nil, nil, nil, err
		}
		if r.SessionID != "" && r.SessionID != in.SessionID {
			return nil, nil, nil, apierr.New("unauthorized", "only session holder/root may end it")
		}
		reason := "session_end"
		event, err := s.event(*sub, "ended", &reason, signer)

		return map[string]string{"session_id": in.SessionID}, []envelope.Object{event}, nil, err
	case "intent.create":
		if in.ParentID != nil {
			if err := s.unusedLeaf(ctx, *in.ParentID); err != nil {
				return nil, nil, nil, err
			}
		}
		criteria, definitions, err := s.pinChecks(in.Criteria, signer)
		if err != nil {
			return nil, nil, nil, err
		}
		id := schema.NewID(schema.PrefixIntent)
		source := schema.Source{Kind: "human"}
		if in.Source != nil {
			source = *in.Source
		}
		if in.ParentID != nil && in.Source == nil {
			source = schema.Source{Kind: "intent", Reference: in.ParentID}
		}
		o, err := envelope.Seal(schema.KindIntent, schema.Intent{ID: id, ParentID: in.ParentID, Description: in.Description, Criteria: criteria, Source: source, Owner: signer.ID(), CreatedAt: schema.Timestamp(time.Now())}, signer)
		return map[string]string{"intent_id": id, "object_hash": o.Hash}, append(definitions, o), nil, err
	case "intent.close":
		sub, err := s.subject(ctx, in.IntentID, "intent")
		if err != nil {
			return nil, nil, nil, err
		}
		var prev *string
		if sub.Head != "" {
			prev = &sub.Head
		}
		event, err := envelope.Seal(schema.KindLifecycleEvent, schema.LifecycleEvent{SubjectKind: "intent", SubjectID: sub.ID, Creation: sub.Creation, Previous: prev, FromState: sub.State, ToState: "closed", Reason: &in.Reason, ReplacementID: in.ReplacementID, Actor: signer.ID(), At: schema.Timestamp(time.Now())}, signer)
		return map[string]string{"intent_id": sub.ID}, []envelope.Object{event}, nil, err
	case "workspace.fork":
		if r.SessionID == "" {
			return nil, nil, nil, apierr.New("unauthorized", "fork requires an active session")
		}
		if err := s.leaf(ctx, in.IntentID); err != nil {
			return nil, nil, nil, err
		}
		if err := s.authority(ctx, r.SessionID, in.CapabilityID); err != nil {
			return nil, nil, nil, err
		}
		base := in.BaseCommit
		if base == "" {
			base = "HEAD"
		}
		base, err := s.Store.Repo.ResolveRef(ctx, base)
		if err != nil {
			return nil, nil, nil, err
		}
		if base == "" {
			return nil, nil, nil, apierr.New("base_unavailable", "workspace needs an existing base commit")
		}
		id := schema.NewID(schema.PrefixWorkspace)
		path := s.Provider.Path(id)
		o, err := envelope.Seal(schema.KindWorkspace, schema.Workspace{ID: id, IntentID: in.IntentID, SessionID: r.SessionID, CapabilityID: in.CapabilityID, ChangeID: schema.NewID(schema.PrefixChange), BaseCommit: base, CreatedAt: schema.Timestamp(time.Now())}, signer)
		return map[string]string{"workspace_id": id, "path": path, "base_commit": base, "object_hash": o.Hash}, []envelope.Object{o}, []record.Effect{{Kind: "worktree_add", WorkspaceID: id, Path: path, Base: base}}, err
	case "workspace.propose", "workspace.rework", "workspace.close":
		sub, w, cap, err := s.boundWorkspace(ctx, in.WorkspaceID, r.SessionID)
		if err != nil {
			return nil, nil, nil, err
		}
		if r.Operation == "workspace.propose" {
			if sub.State != "open" {
				return nil, nil, nil, apierr.New("invalid_state", "workspace must be open before proposing")
			}
			if err := s.authority(ctx, r.SessionID, cap); err != nil {
				return nil, nil, nil, err
			}
			if err := s.leaf(ctx, w.IntentID); err != nil {
				return nil, nil, nil, err
			}
			snap, err := s.Provider.Snapshot(ctx, w, workspace.IndexPath(filepath.Join(s.Store.Dir, "runtime"), w.ID), true)
			if err != nil {
				return nil, nil, nil, err
			}
			if in.ContextRefs == nil {
				in.ContextRefs = []string{}
			}
			if err := s.authority(ctx, r.SessionID, cap); err != nil {
				return nil, nil, nil, err
			}
			manifest, err := envelope.Seal(schema.KindChangeManifest, schema.ChangeManifest{ChangeID: w.ChangeID, IntentID: w.IntentID, WorkspaceID: w.ID, AuthorSession: r.SessionID, ContextRefs: in.ContextRefs, Plan: in.Plan, BaseCommit: w.BaseCommit, Tree: snap.Tree, Files: snap.Files, SymbolsRead: []schema.Symbol{}, SymbolsWritten: []schema.Symbol{}}, signer)
			if err != nil {
				return nil, nil, nil, err
			}
			branch := "refs/heads/ledger/" + w.IntentID + "/" + w.ID
			old, err := s.Store.Index.ProposalCommit(ctx, w.ID)
			if err != nil {
				return nil, nil, nil, err
			}
			current, err := s.Store.Repo.ResolveRef(ctx, branch)
			if err != nil {
				return nil, nil, nil, err
			}
			if current != old {
				return nil, nil, nil, apierr.New("ref_moved", "proposal branch advanced outside Ledger").With("expected", old).With("current", current)
			}
			message, err := s.proposalMessage(ctx, w, manifest.Hash, r.SessionID)
			if err != nil {
				return nil, nil, nil, err
			}
			commit, err := s.Store.Index.ManifestCommit(ctx, manifest.Hash)
			if err != nil {
				return nil, nil, nil, err
			}
			if commit == "" {
				commit, err = s.Store.Repo.CommitTree(ctx, snap.Tree, []string{w.BaseCommit}, message)
				if err != nil {
					return nil, nil, nil, err
				}
			}
			ev, err := s.event(*sub, "proposed", nil, signer)
			return map[string]string{"workspace_id": w.ID, "change_id": w.ChangeID, "manifest": manifest.Hash, "commit": commit, "tree": snap.Tree, "branch": strings.TrimPrefix(branch, "refs/heads/")}, []envelope.Object{manifest, ev}, []record.Effect{{Kind: "ref", Ref: "refs/ledger/proposals/" + manifest.Hash, New: commit}, {Kind: "ref", Ref: branch, Old: old, New: commit}}, err
		}
		to := "open"
		var reason *string
		var effects []record.Effect
		if r.Operation == "workspace.close" {
			to = "closed"
			reason = &in.Reason
			effects = []record.Effect{{Kind: "worktree_remove", WorkspaceID: w.ID, Path: s.Provider.Path(w.ID), After: true}}
		}
		ev, err := s.event(*sub, to, reason, signer)
		return map[string]string{"workspace_id": w.ID, "state": to}, []envelope.Object{ev}, effects, err
	}
	return nil, nil, nil, apierr.New("unknown_operation", "%s", r.Operation)
}
func (s *Service) authority(ctx context.Context, sid, cid string) error {
	sub, err := s.subject(ctx, sid, "session")
	if err != nil {
		return err
	}
	if sub.State != "active" {
		return apierr.New("session_inactive", "session ended")
	}
	cap, err := creation[schema.Capability](ctx, s.Store.Index, cid, schema.KindCapability)
	if err != nil {
		return err
	}
	expires, err := schema.ParseTimestamp(cap.ExpiresAt)
	if err != nil {
		return err
	}
	if !expires.After(time.Now()) {
		return apierr.New("capability_expired", "capability expired")
	}
	if cap.SessionID != sid || cap.Issuer.Kind != "root" || !slices.Equal(cap.Allow, []string{"**"}) || len(cap.Deny) != 0 || !cap.MayAddDependencies {
		return apierr.New("unsupported_capability", "M1 supports root-issued whole-repository capabilities only")
	}
	session, err := creation[schema.Session](ctx, s.Store.Index, sid, schema.KindSession)
	if err != nil {
		return err
	}
	if session.CapabilityID != cid {
		return apierr.New("unauthorized", "session capability binding differs")
	}
	return nil
}
func (s *Service) boundWorkspace(ctx context.Context, id, sid string) (*index.Subject, schema.Workspace, string, error) {
	sub, err := s.subject(ctx, id, "workspace")
	if err != nil {
		return nil, schema.Workspace{}, "", err
	}
	w, err := creation[schema.Workspace](ctx, s.Store.Index, id, schema.KindWorkspace)
	if err != nil {
		return nil, w, "", err
	}
	owner, cap, err := s.Store.Index.Binding(ctx, sub, w)
	if err != nil {
		return nil, w, "", err
	}
	if sid != "" && owner != sid {
		return nil, w, "", apierr.New("unauthorized", "workspace belongs to another session")
	}
	return sub, w, cap, nil
}
func (s *Service) leaf(ctx context.Context, id string) error {
	sub, err := s.subject(ctx, id, "intent")
	if err != nil {
		return err
	}
	if sub.State != "open" {
		return apierr.New("invalid_state", "intent is closed")
	}
	intents, err := s.Store.Index.Objects(ctx, schema.KindIntent)
	if err != nil {
		return err
	}
	for _, raw := range intents {
		var p schema.Intent
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if p.ParentID != nil && *p.ParentID == id {
			return apierr.New("intent_not_leaf", "changes attach to leaf intents only")
		}
	}
	return nil
}
func (s *Service) unusedLeaf(ctx context.Context, id string) error {
	objects, err := s.Store.Index.Objects(ctx, schema.KindChangeManifest)
	if err != nil {
		return err
	}
	for _, raw := range objects {
		var p schema.ChangeManifest
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if p.IntentID == id {
			return apierr.New("intent_already_used", "an intent used by a change cannot acquire children")
		}
	}
	return nil
}
func (s *Service) pinChecks(criteria []schema.Criterion, signer identity.Signer) ([]schema.Criterion, []envelope.Object, error) {
	var definitions []schema.CheckDefinition
	needs := false
	for _, c := range criteria {
		needs = needs || c.Verification.Kind == "check"
	}
	if needs {
		raw, err := os.ReadFile(filepath.Join(s.Store.Dir, "checks.json"))
		if err != nil {
			return nil, nil, apierr.New("unknown_check", "checks.json unavailable: %v", err)
		}
		if err := canon.Validate(raw); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(raw, &definitions, json.RejectUnknownMembers(true)); err != nil {
			return nil, nil, apierr.New("invalid_checks", "%v", err)
		}
	}
	pinned := map[string]envelope.Object{}
	names := map[string]bool{}
	for _, def := range definitions {
		if names[def.Name] {
			return nil, nil, apierr.New("invalid_checks", "duplicate check %s", def.Name)
		}
		names[def.Name] = true
		o, err := envelope.Seal(schema.KindCheckDefinition, def, signer)
		if err != nil {
			return nil, nil, err
		}
		pinned[def.Name] = o
	}
	members := []envelope.Object{}
	included := map[string]bool{}
	for i, c := range criteria {
		if c.Verification.Kind != "check" {
			continue
		}
		o, ok := pinned[c.Verification.Check]
		if !ok {
			return nil, nil, apierr.New("unknown_check", "no check %s", c.Verification.Check)
		}
		criteria[i].Verification.DefinitionHash = o.Hash
		if !included[o.Hash] {
			members = append(members, o)
			included[o.Hash] = true
		}
	}
	return criteria, members, nil
}
