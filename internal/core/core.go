// Package core implements the versioned API independently of transports.
package core

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/evidence"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/workspace"
)

// Service executes API requests. Mutations are serialized by mu; reads and
// observations run concurrently, with each workspace's private observation
// index guarded by its own lock.
type Service struct {
	Store    *record.Store
	Keys     identity.IdentityProvider
	Provider workspace.WorkspaceProvider

	mu              sync.Mutex
	signers         map[string]identity.Signer
	recoveryPending bool
	rootID          string
	caseSensitive   bool

	observing sync.Map // workspace id -> *sync.Mutex

	// evidenceHooks are test seams for evidence runs; nil in production.
	evidenceHooks *evidence.Hooks
}

// New opens the service over store, completing interrupted operations and
// ending sessions whose keys did not survive a restart.
func New(ctx context.Context, store *record.Store, keys identity.IdentityProvider) (*Service, error) {
	p := workspace.GitProvider{Repo: store.Repo}
	store.SetEffectHandler(p.Apply)
	s := &Service{Store: store, Keys: keys, Provider: p, signers: map[string]identity.Signer{}, caseSensitive: true}
	var config struct {
		Root          string `json:"root_fingerprint"`
		CaseSensitive bool   `json:"case_sensitive"`
	}
	if b, err := os.ReadFile(filepath.Join(store.Dir, "config.json")); err == nil {
		if err := json.Unmarshal(b, &config); err != nil {
			return nil, err
		}
		s.rootID = config.Root
		s.caseSensitive = config.CaseSensitive
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	stale, err := filepath.Glob(filepath.Join(s.runtimeDir(), "observe-*.index.lock"))
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
	if _, err := evidence.Reconcile(ctx, store.Repo, s.runtimeDir()); err != nil {
		return nil, err
	}
	if err := s.settleRuns(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reconcile(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) runtimeDir() string { return filepath.Join(s.Store.Dir, "runtime") }

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

// reconcile ends active sessions whose keys this process does not hold with
// root-signed events. Without the root key it leaves recovery pending, so
// no lost session can sign and the next request retries. Callers hold mu.
func (s *Service) reconcile(ctx context.Context) error {
	sessions, err := s.Store.Index.SubjectsIn(ctx, "session", "active")
	if err != nil {
		return err
	}
	s.recoveryPending = false
	for _, sub := range sessions {
		if sub.Conflict != "" || s.signers[sub.ID] != nil {
			continue
		}
		root, err := s.root()
		if err != nil {
			s.recoveryPending = true
			return nil //nolint:nilerr // a missing root key is reported as recovery_pending, not as a failure
		}
		reason := "daemon_restart"
		event, err := envelope.Seal(schema.KindLifecycleEvent, transition(sub, "ended", &reason, root.ID()), root)
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
	return nil
}

// transition is the next lifecycle event for sub.
func transition(sub index.Subject, to string, reason *string, actor string) schema.LifecycleEvent {
	var prev *string
	if sub.Head != "" {
		prev = &sub.Head
	}
	return schema.LifecycleEvent{SubjectKind: sub.Kind, SubjectID: sub.ID, Creation: sub.Creation, Previous: prev, FromState: sub.State, ToState: to, Reason: reason, Actor: actor, At: now()}
}

func now() string { return schema.Timestamp(time.Now()) }

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

func decode[T any](raw []byte) (T, error) {
	var in T
	err := json.Unmarshal(raw, &in, json.RejectUnknownMembers(true))
	return in, err
}

func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// Handle validates and executes one request.
func (s *Service) Handle(ctx context.Context, r api.Request) (any, error) {
	raw, err := canon.Marshal(r)
	if err != nil {
		return nil, err
	}
	if _, err := api.Parse(raw); err != nil {
		return nil, err
	}
	if err := s.retryRecovery(ctx); err != nil {
		return nil, err
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
	if r.Operation == "evidence.run" {
		return s.evidenceRun(ctx, r)
	}
	return s.mutate(ctx, r)
}

func (s *Service) retryRecovery(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.recoveryPending {
		return nil
	}
	return s.reconcile(ctx)
}

func (s *Service) read(ctx context.Context, r api.Request) (any, error) {
	switch r.Operation {
	case "coordination.notifications", "coordination.ack":
		return s.notifications(ctx, r)
	case "system.status":
		subjects, err := s.Store.Index.Subjects(ctx)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		pending := s.recoveryPending
		s.mu.Unlock()
		runs, err := evidence.Interrupted(s.runtimeDir())
		if err != nil {
			return nil, err
		}
		interrupted := []map[string]any{}
		for _, j := range runs {
			if j.OperationID != "" {
				// Recovery may have committed the attestation after the crash.
				ops, err := s.Store.Index.Operations(ctx, j.OperationID)
				if err != nil {
					return nil, err
				}
				if len(ops) > 0 {
					continue
				}
			}
			interrupted = append(interrupted, map[string]any{"run_id": j.RunID, "manifest": j.Manifest, "commit": j.Commit, "reason": j.Reason})
		}
		return map[string]any{"subjects": subjects, "recovery_pending": pending, "process_id": os.Getpid(), "interrupted_runs": interrupted}, nil
	case "intent.status":
		in, err := decode[struct {
			IntentID string `json:"intent_id"`
		}](r.Input)
		if err != nil {
			return nil, err
		}
		sub, err := s.subject(ctx, in.IntentID, "intent")
		if err != nil {
			return nil, err
		}
		p, err := creation[schema.Intent](ctx, s.Store.Index, in.IntentID, schema.KindIntent)
		return map[string]any{"subject": sub, "intent": p}, err
	case "evidence.resolve":
		in, err := decode[struct {
			ChangeID string `json:"change_id"`
			Manifest string `json:"manifest"`
			Commit   string `json:"commit"`
		}](r.Input)
		if err != nil {
			return nil, err
		}
		return evidence.Resolve(ctx, s.Store.Index, evidence.ResolveInput{ChangeID: in.ChangeID, Manifest: in.Manifest, Commit: in.Commit})
	case "workspace.status":
		in, err := decode[workspaceInput](r.Input)
		if err != nil {
			return nil, err
		}
		return s.workspaceStatus(ctx, in.WorkspaceID)
	}
	return nil, apierr.New("unknown_operation", "%s", r.Operation)
}

// invocation is one mutation in progress under the RecordStore lock.
type invocation struct {
	api.Request
	signer identity.Signer
	// key is a new session key, held only once the operation commits.
	key identity.Signer
}

// prepared is what a mutation publishes as one operation group.
type prepared struct {
	result  any
	records []envelope.Object
	effects []record.Effect
}

type mutation func(*Service, context.Context, *invocation) (prepared, error)

var mutations = map[string]mutation{
	"capability.issue":    (*Service).issueCapability,
	"capability.delegate": (*Service).issueCapability,
	"daemon.stop":         (*Service).stopDaemon,
	"session.create":      (*Service).createSession,
	"session.recover":     (*Service).recoverSession,
	"session.end":         (*Service).endSession,
	"intent.create":       (*Service).createIntent,
	"intent.close":        (*Service).closeIntent,
	"workspace.fork":      (*Service).fork,
	"workspace.propose":   (*Service).propose,
	"workspace.rework":    (*Service).rework,
	"workspace.close":     (*Service).closeWorkspace,
}

func (s *Service) mutate(ctx context.Context, r api.Request) (any, error) {
	run, ok := mutations[r.Operation]
	if !ok {
		return nil, apierr.New("unknown_operation", "%s", r.Operation)
	}
	digest, err := r.Digest()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &invocation{Request: r}
	var keyErr error
	if r.SessionID == "" {
		c.signer, keyErr = s.root()
	} else if c.signer = s.signers[r.SessionID]; c.signer == nil {
		keyErr = apierr.New("session_key_unavailable", "session key was lost or ended; use explicit recovery")
	}
	result, err := s.publish(ctx, record.Op{ID: r.OperationID, Name: r.Operation, Digest: digest, Signer: c.signer}, func(idx *index.Index) (any, []envelope.Object, []record.Effect, error) {
		// A replayed operation returns before this runs, so a retry
		// succeeds even after its signing key is gone.
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
		p, err := run(s, ctx, c)
		return p.result, p.records, p.effects, err
	})
	if c.key != nil {
		if err == nil {
			s.signers[c.key.ID()] = c.key
		} else {
			// The session may have committed before the failure; without
			// its key it must be ended like any other lost session.
			s.recoveryPending = true
		}
	}
	return result, err
}

// publish commits one operation group under the RecordStore lock and then
// drops the keys of sessions it ended. Callers hold mu.
func (s *Service) publish(ctx context.Context, op record.Op, build record.EffectsFunc) (*record.Result, error) {
	result, err := s.Store.DoEffects(ctx, op, build)
	for id := range s.signers {
		if sub, e := s.Store.Index.Subject(ctx, id); e == nil && sub != nil && sub.State == "ended" {
			delete(s.signers, id)
		}
	}
	return result, err
}

func requireRoot(c *invocation) error {
	if c.SessionID != "" {
		return apierr.New("unauthorized", "%s requires root authorization", c.Operation)
	}
	return nil
}

func (s *Service) stopDaemon(_ context.Context, c *invocation) (prepared, error) {
	if err := requireRoot(c); err != nil {
		return prepared{}, err
	}
	return prepared{result: map[string]bool{"stopping": true}}, nil
}

type sessionInput struct {
	WorkspaceID        string   `json:"workspace_id"`
	AgentName          string   `json:"agent_name"`
	Model              string   `json:"model"`
	ParticipantKind    string   `json:"participant_kind"`
	ExpiresAt          string   `json:"expires_at"`
	Allow              []string `json:"allow"`
	Deny               []string `json:"deny"`
	MayAddDependencies *bool    `json:"may_add_dependencies"`
	PreviousBinding    string   `json:"previous_binding"`
}

func (s *Service) createSession(_ context.Context, c *invocation) (prepared, error) {
	if err := requireRoot(c); err != nil {
		return prepared{}, err
	}
	in, err := decode[sessionInput](c.Input)
	if err != nil {
		return prepared{}, err
	}
	result, records, err := s.newSession(c, in)
	return prepared{result: result, records: records}, err
}

// recoverSession issues replacement authority for an open or proposed
// workspace and rebinds it, preserving earlier authorship.
func (s *Service) recoverSession(ctx context.Context, c *invocation) (prepared, error) {
	if err := requireRoot(c); err != nil {
		return prepared{}, err
	}
	in, err := decode[sessionInput](c.Input)
	if err != nil {
		return prepared{}, err
	}
	sub, err := s.subject(ctx, in.WorkspaceID, "workspace")
	if err != nil {
		return prepared{}, err
	}
	if sub.State != "open" && sub.State != "proposed" {
		return prepared{}, apierr.New("invalid_state", "terminal workspace cannot rebind")
	}
	if in.PreviousBinding != "" && in.PreviousBinding != sub.BindingHead {
		return prepared{}, apierr.New("stale_predecessor", "workspace binding changed")
	}
	result, records, err := s.newSession(c, in)
	if err != nil {
		return prepared{}, err
	}
	binding, err := envelope.Seal(schema.KindWorkspaceBinding, schema.WorkspaceBinding{WorkspaceID: sub.ID, Previous: sub.BindingHead, SessionID: result["session_id"], CapabilityID: result["capability_id"], Actor: c.signer.ID(), Reason: "explicit_recovery", At: now()}, c.signer)
	if err != nil {
		return prepared{}, err
	}
	result["workspace_id"] = sub.ID
	return prepared{result: result, records: append(records, binding)}, nil
}

// newSession signs a session and its whole-repository root capability, and
// stages the session's ephemeral key on c.
func (s *Service) newSession(c *invocation, in sessionInput) (map[string]string, []envelope.Object, error) {
	sid, cid := schema.NewID(schema.PrefixSession), schema.NewID(schema.PrefixCapability)
	key, err := identity.NewSession(sid)
	if err != nil {
		return nil, nil, err
	}
	start := time.Now()
	expiry := cmp.Or(in.ExpiresAt, schema.Timestamp(start.Add(24*time.Hour)))
	if end, err := schema.ParseTimestamp(expiry); err != nil || !end.After(start) {
		return nil, nil, apierr.New("invalid_expiry", "session capability must expire in the future")
	}
	allow, deny := in.Allow, in.Deny
	if allow == nil {
		allow = []string{"**"}
	}
	if deny == nil {
		deny = []string{}
	}
	deps := true
	if in.MayAddDependencies != nil {
		deps = *in.MayAddDependencies
	}
	for i := range allow {
		allow[i] = norm.NFC.String(allow[i])
	}
	for i := range deny {
		deny[i] = norm.NFC.String(deny[i])
	}
	grant := schema.Capability{ID: cid, SessionID: sid, Issuer: schema.Issuer{Kind: "root", Fingerprint: c.signer.ID()}, Allow: allow, Deny: deny, MayAddDependencies: deps, ExpiresAt: expiry, CreatedAt: schema.Timestamp(start)}
	if err := capability.Validate(grant, s.caseSensitive); err != nil {
		return nil, nil, err
	}
	capability, err := envelope.Seal(schema.KindCapability, grant, c.signer)
	if err != nil {
		return nil, nil, err
	}
	session, err := envelope.Seal(schema.KindSession, schema.Session{ID: sid, CapabilityID: cid, PublicKey: identity.EncodePublicKey(key.PublicKey()), ParticipantKind: cmp.Or(in.ParticipantKind, "agent"), AgentName: optional(in.AgentName), Model: optional(in.Model), StartedAt: schema.Timestamp(start)}, c.signer)
	if err != nil {
		return nil, nil, err
	}
	c.key = key
	result := map[string]string{"session_id": sid, "capability_id": cid, "session_hash": session.Hash, "capability_hash": capability.Hash}
	return result, []envelope.Object{capability, session}, nil
}

func (s *Service) endSession(ctx context.Context, c *invocation) (prepared, error) {
	in, err := decode[struct {
		SessionID string `json:"session_id"`
	}](c.Input)
	if err != nil {
		return prepared{}, err
	}
	if c.SessionID != "" && c.SessionID != in.SessionID {
		return prepared{}, apierr.New("unauthorized", "only the session itself or root may end it")
	}
	sub, err := s.subject(ctx, in.SessionID, "session")
	if err != nil {
		return prepared{}, err
	}
	reason := "session_end"
	event, err := envelope.Seal(schema.KindLifecycleEvent, transition(*sub, "ended", &reason, c.signer.ID()), c.signer)
	return prepared{result: map[string]string{"session_id": in.SessionID}, records: []envelope.Object{event}}, err
}

func (s *Service) createIntent(ctx context.Context, c *invocation) (prepared, error) {
	in, err := decode[struct {
		Description string             `json:"description"`
		Criteria    []schema.Criterion `json:"criteria"`
		ParentID    *string            `json:"parent_id"`
		Source      *schema.Source     `json:"source"`
	}](c.Input)
	if err != nil {
		return prepared{}, err
	}
	source := schema.Source{Kind: "human"}
	if in.ParentID != nil {
		used, err := s.Store.Index.IntentHasChange(ctx, *in.ParentID)
		if err != nil {
			return prepared{}, err
		}
		if used {
			return prepared{}, apierr.New("intent_already_used", "an intent used by a change cannot acquire children")
		}
		source = schema.Source{Kind: "intent", Reference: in.ParentID}
	}
	if in.Source != nil {
		source = *in.Source
	}
	criteria, definitions, err := s.pinChecks(in.Criteria, c.signer)
	if err != nil {
		return prepared{}, err
	}
	id := schema.NewID(schema.PrefixIntent)
	o, err := envelope.Seal(schema.KindIntent, schema.Intent{ID: id, ParentID: in.ParentID, Description: in.Description, Criteria: criteria, Source: source, Owner: c.signer.ID(), CreatedAt: now()}, c.signer)
	return prepared{result: map[string]string{"intent_id": id, "object_hash": o.Hash}, records: append(definitions, o)}, err
}

func (s *Service) closeIntent(ctx context.Context, c *invocation) (prepared, error) {
	in, err := decode[struct {
		IntentID      string  `json:"intent_id"`
		Reason        string  `json:"reason"`
		ReplacementID *string `json:"replacement_id"`
	}](c.Input)
	if err != nil {
		return prepared{}, err
	}
	sub, err := s.subject(ctx, in.IntentID, "intent")
	if err != nil {
		return prepared{}, err
	}
	event := transition(*sub, "closed", &in.Reason, c.signer.ID())
	event.ReplacementID = in.ReplacementID
	o, err := envelope.Seal(schema.KindLifecycleEvent, event, c.signer)
	return prepared{result: map[string]string{"intent_id": sub.ID}, records: []envelope.Object{o}}, err
}

func (s *Service) fork(ctx context.Context, c *invocation) (prepared, error) {
	in, err := decode[struct {
		IntentID     string `json:"intent_id"`
		CapabilityID string `json:"capability_id"`
		BaseCommit   string `json:"base_commit"`
	}](c.Input)
	if err != nil {
		return prepared{}, err
	}
	if c.SessionID == "" {
		return prepared{}, apierr.New("unauthorized", "fork requires an active session")
	}
	if err := s.leaf(ctx, in.IntentID); err != nil {
		return prepared{}, err
	}
	if err := s.authority(ctx, c.SessionID, in.CapabilityID); err != nil {
		return prepared{}, err
	}
	base, err := s.Store.Repo.ResolveRef(ctx, cmp.Or(in.BaseCommit, "HEAD"))
	if err != nil {
		return prepared{}, err
	}
	if base == "" {
		return prepared{}, apierr.New("base_unavailable", "workspace needs an existing base commit")
	}
	id := schema.NewID(schema.PrefixWorkspace)
	path := s.Provider.Path(id)
	o, err := envelope.Seal(schema.KindWorkspace, schema.Workspace{ID: id, IntentID: in.IntentID, SessionID: c.SessionID, CapabilityID: in.CapabilityID, ChangeID: schema.NewID(schema.PrefixChange), BaseCommit: base, CreatedAt: now()}, c.signer)
	return prepared{
		result:  map[string]string{"workspace_id": id, "path": path, "base_commit": base, "object_hash": o.Hash},
		records: []envelope.Object{o},
		effects: []record.Effect{{Kind: record.EffectWorktreeAdd, WorkspaceID: id, Path: path, Base: base}},
	}, err
}

type workspaceInput struct {
	WorkspaceID string `json:"workspace_id"`
}

// propose captures the workspace against its fixed base, signs a manifest
// for the resulting tree, and publishes a sole-parent commit carrying the
// manifest trailers. Authority is checked again after the capture so an
// expiry during a slow snapshot cannot sign it.
func (s *Service) propose(ctx context.Context, c *invocation) (prepared, error) {
	in, err := decode[struct {
		WorkspaceID string   `json:"workspace_id"`
		Plan        *string  `json:"plan"`
		ContextRefs []string `json:"context_refs"`
	}](c.Input)
	if err != nil {
		return prepared{}, err
	}
	sub, w, capability, err := s.boundWorkspace(ctx, in.WorkspaceID, c.SessionID)
	if err != nil {
		return prepared{}, err
	}
	if sub.State != "open" {
		return prepared{}, apierr.New("invalid_state", "workspace must be open before proposing")
	}
	if err := s.authority(ctx, c.SessionID, capability); err != nil {
		return prepared{}, err
	}
	if err := s.leaf(ctx, w.IntentID); err != nil {
		return prepared{}, err
	}
	snap, err := s.Provider.StableSnapshot(ctx, w, s.runtimeDir())
	if err != nil {
		return prepared{}, err
	}
	if err := s.authority(ctx, c.SessionID, capability); err != nil {
		return prepared{}, err
	}
	chain, err := s.chain(ctx, capability, time.Now())
	if err != nil {
		return prepared{}, err
	}
	bad, err := workspace.Violations(ctx, s.Provider.Path(w.ID), w.BaseCommit, snap, chain, s.caseSensitive, time.Now())
	if err != nil {
		return prepared{}, err
	}
	if len(bad) > 0 {
		paths := make([]string, 0, len(bad))
		for p := range bad {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		return prepared{}, apierr.New("scope_violation", "proposal contains unauthorized paths").With("paths", paths)
	}
	if in.ContextRefs == nil {
		in.ContextRefs = []string{}
	}
	recordFiles, err := snap.RecordFiles()
	if err != nil {
		return prepared{}, err
	}
	manifest, err := envelope.Seal(schema.KindChangeManifest, schema.ChangeManifest{ChangeID: w.ChangeID, IntentID: w.IntentID, WorkspaceID: w.ID, AuthorSession: c.SessionID, ContextRefs: in.ContextRefs, Plan: in.Plan, BaseCommit: w.BaseCommit, Tree: snap.Tree, Files: recordFiles, SymbolsRead: []schema.Symbol{}, SymbolsWritten: []schema.Symbol{}}, c.signer)
	if err != nil {
		return prepared{}, err
	}
	last, err := s.Store.Index.LastProposal(ctx, w.ID)
	if err != nil {
		return prepared{}, err
	}
	// A workspace keeps the branch its first proposal published.
	branch := "refs/heads/" + cmp.Or(last.Branch, "ledger/"+schema.ShortID(w.IntentID)+"/"+schema.ShortID(w.ID))
	old := last.Commit
	current, err := s.Store.Repo.ResolveRef(ctx, branch)
	if err != nil {
		return prepared{}, err
	}
	if current != old {
		return prepared{}, apierr.New("ref_moved", "proposal branch advanced outside Ledger").With("expected", old).With("current", current)
	}
	commit, err := s.Store.Index.ManifestCommit(ctx, manifest.Hash)
	if err != nil {
		return prepared{}, err
	}
	if commit == "" {
		message, err := s.proposalMessage(ctx, w, manifest.Hash, c.SessionID)
		if err != nil {
			return prepared{}, err
		}
		if commit, err = s.Store.Repo.CommitTree(ctx, snap.Tree, []string{w.BaseCommit}, message); err != nil {
			return prepared{}, err
		}
	}
	event, err := envelope.Seal(schema.KindLifecycleEvent, transition(*sub, "proposed", nil, c.signer.ID()), c.signer)
	return prepared{
		result:  map[string]string{"workspace_id": w.ID, "change_id": w.ChangeID, "manifest": manifest.Hash, "commit": commit, "tree": snap.Tree, "branch": strings.TrimPrefix(branch, "refs/heads/")},
		records: []envelope.Object{manifest, event},
		effects: []record.Effect{
			{Kind: record.EffectRef, Ref: "refs/ledger/proposals/" + manifest.Hash, New: commit},
			{Kind: record.EffectRef, Ref: branch, Old: old, New: commit},
		},
	}, err
}

// proposalMessage builds the commit message. Trailers are appended
// literally rather than through git interpret-trailers, so repository
// trailer configuration cannot rewrite or retarget them (ADR 0018).
func (s *Service) proposalMessage(ctx context.Context, w schema.Workspace, manifest, sid string) (string, error) {
	intent, err := creation[schema.Intent](ctx, s.Store.Index, w.IntentID, schema.KindIntent)
	if err != nil {
		return "", err
	}
	summary, _, _ := strings.Cut(strings.ReplaceAll(intent.Description, "\r", "\n"), "\n")
	summary = strings.ReplaceAll(summary, "\x00", " ")
	return "Ledger: " + summary + "\n\nLedger-Change: " + w.ChangeID + "\nLedger-Intent: " + w.IntentID + "\nLedger-Session: " + sid + "\nLedger-Manifest: " + manifest + "\n", nil
}

func (s *Service) rework(ctx context.Context, c *invocation) (prepared, error) {
	in, err := decode[workspaceInput](c.Input)
	if err != nil {
		return prepared{}, err
	}
	return s.transitionWorkspace(ctx, c, in.WorkspaceID, "open", nil, nil)
}

// closeWorkspace publishes the terminal state before removing the
// worktree, so a cleanup failure never hides the committed close.
func (s *Service) closeWorkspace(ctx context.Context, c *invocation) (prepared, error) {
	in, err := decode[struct {
		WorkspaceID string `json:"workspace_id"`
		Reason      string `json:"reason"`
	}](c.Input)
	if err != nil {
		return prepared{}, err
	}
	cleanup := []record.Effect{{Kind: record.EffectWorktreeRemove, WorkspaceID: in.WorkspaceID, Path: s.Provider.Path(in.WorkspaceID), After: true}}
	return s.transitionWorkspace(ctx, c, in.WorkspaceID, "closed", &in.Reason, cleanup)
}

func (s *Service) transitionWorkspace(ctx context.Context, c *invocation, id, to string, reason *string, effects []record.Effect) (prepared, error) {
	sub, _, _, err := s.boundWorkspace(ctx, id, c.SessionID)
	if err != nil {
		return prepared{}, err
	}
	event, err := envelope.Seal(schema.KindLifecycleEvent, transition(*sub, to, reason, c.signer.ID()), c.signer)
	return prepared{result: map[string]string{"workspace_id": id, "state": to}, records: []envelope.Object{event}, effects: effects}, err
}

// authority checks the authenticated holder and every link at the current time.
func (s *Service) authority(ctx context.Context, sid, cid string) error {
	sub, err := s.subject(ctx, sid, "session")
	if err != nil {
		return err
	}
	if sub.State != "active" {
		return apierr.New("session_inactive", "session ended")
	}
	chain, err := s.chain(ctx, cid, time.Now())
	if err != nil {
		return err
	}
	if chain[0].SessionID != sid {
		return apierr.New("unauthorized", "capability belongs to another session")
	}
	return nil
}
func (s *Service) chain(ctx context.Context, cid string, at time.Time) ([]schema.Capability, error) {
	return capability.Chain(cid, func(id string) (schema.Capability, error) {
		return creation[schema.Capability](ctx, s.Store.Index, id, schema.KindCapability)
	}, s.caseSensitive, at)
}

// boundWorkspace returns workspace id and the capability it is currently
// bound to. A non-empty sid must be the bound session.
func (s *Service) boundWorkspace(ctx context.Context, id, sid string) (*index.Subject, schema.Workspace, string, error) {
	sub, err := s.subject(ctx, id, "workspace")
	if err != nil {
		return nil, schema.Workspace{}, "", err
	}
	w, err := creation[schema.Workspace](ctx, s.Store.Index, id, schema.KindWorkspace)
	if err != nil {
		return nil, w, "", err
	}
	owner, capability, err := s.Store.Index.Binding(ctx, sub, w)
	if err != nil {
		return nil, w, "", err
	}
	if sid != "" && owner != sid {
		return nil, w, "", apierr.New("unauthorized", "workspace belongs to another session")
	}
	return sub, w, capability, nil
}

// leaf requires intent id to be open and childless: a change attaches to
// exactly one leaf intent.
func (s *Service) leaf(ctx context.Context, id string) error {
	sub, err := s.subject(ctx, id, "intent")
	if err != nil {
		return err
	}
	if sub.State != "open" {
		return apierr.New("invalid_state", "intent is closed")
	}
	parent, err := s.Store.Index.HasChildIntent(ctx, id)
	if err != nil {
		return err
	}
	if parent {
		return apierr.New("intent_not_leaf", "changes attach to leaf intents only")
	}
	return nil
}

// pinChecks resolves check criteria against .ledger/checks.json, storing
// each referenced definition as a signed record and its hash in the
// criterion, so later edits to the file cannot retarget the intent.
func (s *Service) pinChecks(criteria []schema.Criterion, signer identity.Signer) ([]schema.Criterion, []envelope.Object, error) {
	if !slices.ContainsFunc(criteria, func(c schema.Criterion) bool { return c.Verification.Kind == "check" }) {
		return criteria, nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(s.Store.Dir, "checks.json"))
	if err != nil {
		return nil, nil, apierr.New("unknown_check", "checks.json unavailable: %v", err)
	}
	if err := canon.Validate(raw); err != nil {
		return nil, nil, apierr.New("invalid_checks", "%v", err)
	}
	definitions, err := decode[[]schema.CheckDefinition](raw)
	if err != nil {
		return nil, nil, apierr.New("invalid_checks", "%v", err)
	}
	byName := map[string]schema.CheckDefinition{}
	for _, def := range definitions {
		if _, dup := byName[def.Name]; dup {
			return nil, nil, apierr.New("invalid_checks", "duplicate check %s", def.Name)
		}
		byName[def.Name] = def
	}
	var records []envelope.Object
	pinned := map[string]string{}
	for i, c := range criteria {
		if c.Verification.Kind != "check" {
			continue
		}
		name := c.Verification.Check
		if _, done := pinned[name]; !done {
			def, ok := byName[name]
			if !ok {
				return nil, nil, apierr.New("unknown_check", "no check %s", name)
			}
			if err := evidence.ValidateDefinition(def); err != nil {
				return nil, nil, apierr.New("invalid_checks", "check %s: %v", name, err)
			}
			o, err := envelope.Seal(schema.KindCheckDefinition, def, signer)
			if err != nil {
				return nil, nil, err
			}
			pinned[name] = o.Hash
			records = append(records, o)
		}
		criteria[i].Verification.DefinitionHash = pinned[name]
	}
	return criteria, records, nil
}
