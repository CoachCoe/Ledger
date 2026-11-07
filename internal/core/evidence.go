package core

import (
	"context"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/evidence"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// verifierGrace is how long past a run's own timeouts its verifier session
// stays valid, covering checkout and publication.
const verifierGrace = 10 * time.Minute

var errNotCommitted = errors.New("operation not committed")

type runInput struct {
	Manifest        string `json:"manifest"`
	Commit          string `json:"commit"`
	CheckDefinition string `json:"check_definition"`
	KeepWorktree    string `json:"keep_worktree"`
}

// evidenceRun executes one check against an exact revision and publishes a
// signed attestation. It holds the service lock only to resolve, provision a
// verifier session and journal the run (phase 1) and to publish (phase 3);
// the child processes run unlocked (phase 2), so other requests proceed
// while a long check executes. Publication uses the caller's operation id,
// so a retry replays the frozen result.
func (s *Service) evidenceRun(ctx context.Context, r api.Request) (any, error) {
	in, err := decode[runInput](r.Input)
	if err != nil {
		return nil, err
	}
	digest, err := r.Digest()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	replayed, plan, err := s.beginRun(ctx, r, in, digest)
	s.mu.Unlock()
	if err != nil || replayed != nil {
		if replayed != nil {
			return replayed, nil
		}
		return nil, err
	}
	out, execErr := evidence.Execute(ctx, s.Store.Repo, s.runtimeDir(), plan, s.evidenceHooks)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishRun(ctx, r, digest, plan, out, execErr)
}

// beginRun is phase 1. Callers hold mu. It returns a replayed result when
// the operation already committed.
func (s *Service) beginRun(ctx context.Context, r api.Request, in runInput, digest string) (*record.Result, evidence.Plan, error) {
	var none evidence.Plan
	// A committed operation replays its frozen result, even if keys are gone.
	res, err := s.Store.DoEffects(ctx, record.Op{ID: r.OperationID, Name: r.Operation, Digest: digest}, func(*index.Index) (any, []envelope.Object, []record.Effect, error) {
		return nil, nil, nil, errNotCommitted
	})
	if err == nil {
		return res, none, nil
	}
	if !errors.Is(err, errNotCommitted) {
		return nil, none, err
	}
	journals, err := evidence.ListJournals(s.runtimeDir())
	if err != nil {
		return nil, none, err
	}
	for _, j := range journals {
		if j.OperationID != r.OperationID {
			continue
		}
		switch j.State {
		case evidence.StateInterrupted:
			return nil, none, apierr.New("run_interrupted", "evidence run %s was interrupted before a result was recorded; retry with a new operation id", j.RunID).
				With("run_id", j.RunID).With("manifest", j.Manifest).With("commit", j.Commit).With("check_definition", j.CheckDefinition).With("reason", j.Reason)
		case evidence.StateAborted:
		default:
			return nil, none, apierr.New("run_in_progress", "evidence run %s for this operation has not finished", j.RunID).With("run_id", j.RunID)
		}
	}

	root, err := s.root()
	if err != nil {
		return nil, none, apierr.New("root_unavailable", "evidence needs the root key to provision a verifier session: %v", err)
	}
	if r.SessionID != "" {
		if s.signers[r.SessionID] == nil {
			return nil, none, apierr.New("session_key_unavailable", "session key was lost or ended; use explicit recovery")
		}
		session, err := creation[schema.Session](ctx, s.Store.Index, r.SessionID, schema.KindSession)
		if err != nil {
			return nil, none, err
		}
		if err := s.authority(ctx, r.SessionID, session.CapabilityID); err != nil {
			return nil, none, err
		}
	}
	b, err := evidence.Resolve(ctx, s.Store.Index, evidence.ResolveInput{Manifest: in.Manifest, Commit: in.Commit})
	if err != nil {
		return nil, none, err
	}
	i := slices.IndexFunc(b.Checks, func(c evidence.CheckRef) bool { return c.DefinitionHash == in.CheckDefinition })
	if i < 0 {
		return nil, none, apierr.New("check_not_pinned", "check definition %s is not pinned by the intent of manifest %s", in.CheckDefinition, in.Manifest)
	}
	check := b.Checks[i]
	if err := evidence.Runnable(check.Definition); err != nil {
		return nil, none, apierr.New("not_runnable", "%v", err)
	}
	if r.SessionID != "" {
		holders, err := s.Store.Index.WorkspaceHolders(ctx, b.WorkspaceID)
		if err != nil {
			return nil, none, err
		}
		if r.SessionID == b.AuthorSession || slices.Contains(holders, r.SessionID) {
			return nil, none, apierr.New("unauthorized", "the author or a holder of the workspace cannot request its verification")
		}
	}

	runID := schema.NewID(schema.PrefixRun)
	verifier, err := s.provisionVerifier(ctx, r, root, runID, check.Definition)
	if err != nil {
		return nil, none, err
	}
	return nil, evidence.Plan{
		RunID: runID, OperationID: r.OperationID, VerifierSession: verifier, Binding: b, Check: check,
		Keep: evidence.KeepPolicy(in.KeepWorktree), SourceEnv: os.Environ(),
	}, nil
}

// provisionVerifier creates the run's ephemeral root-signed service session.
// It has no path authority (the capability schema requires a non-empty allow
// list, so "**" is allowed and then denied; deny wins); it exists only to
// sign this run's attestation, so the signer can never be the author.
// Callers hold mu.
func (s *Service) provisionVerifier(ctx context.Context, r api.Request, root identity.Signer, runID string, def schema.CheckDefinition) (string, error) {
	budget := time.Duration(2*def.TimeoutSeconds)*time.Second + verifierGrace
	in := sessionInput{AgentName: "evidence-verifier", ParticipantKind: "service", ExpiresAt: schema.Timestamp(time.Now().Add(budget)), Allow: []string{"**"}, Deny: []string{"**"}}
	c := &invocation{Request: r, signer: root}
	var created map[string]string
	op := record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "evidence.verifier", Digest: canon.Hash([]byte("evidence.verifier:" + runID)), Signer: root}
	_, err := s.Store.Do(ctx, op, func(*index.Index) (any, []envelope.Object, error) {
		result, records, err := s.newSession(c, in)
		created = result
		return result, records, err
	})
	if c.key != nil {
		if err == nil {
			s.signers[c.key.ID()] = c.key
		} else {
			s.recoveryPending = true
		}
	}
	if err != nil {
		return "", err
	}
	return created["session_id"], nil
}

// finishRun is phase 3. Callers hold mu. Publication and cleanup ignore
// cancellation of ctx: a cancelled run still records its failed attestation.
func (s *Service) finishRun(ctx context.Context, r api.Request, digest string, plan evidence.Plan, out evidence.Outcome, execErr error) (any, error) {
	pctx := context.WithoutCancel(ctx)
	rt := s.runtimeDir()
	if execErr != nil {
		// A refused checkout is final and releases its verifier. Any other
		// error leaves the run non-terminal for restart reconciliation.
		if j, err := evidence.ReadJournal(rt, plan.RunID); err == nil && j.Terminal() {
			s.endVerifier(pctx, plan.VerifierSession)
		}
		return nil, execErr
	}
	verifier := s.signers[plan.VerifierSession]
	if verifier == nil {
		_ = evidence.SetState(rt, plan.RunID, evidence.StateInterrupted, "verifier_key_lost")
		return nil, apierr.New("run_interrupted", "the verifier key for run %s was lost; no attestation was signed", plan.RunID).
			With("run_id", plan.RunID).With("manifest", plan.Binding.Manifest).With("commit", plan.Binding.Commit).With("check_definition", plan.Check.DefinitionHash).With("reason", "verifier_key_lost")
	}
	if err := s.evidenceHooks.Stage(evidence.StageBeforeSign); err != nil {
		return nil, err
	}
	att := out.Attestation
	att.VerifierSession = plan.VerifierSession
	o, err := envelope.Seal(schema.KindEvidenceAttestation, att, verifier)
	if err != nil {
		_ = evidence.SetState(rt, plan.RunID, evidence.StateInterrupted, "signing_failed")
		return nil, err
	}
	if err := s.evidenceHooks.Stage(evidence.StageAfterSign); err != nil {
		return nil, err
	}
	var kept any
	if out.Kept != "" {
		kept = out.Kept
	}
	result := map[string]any{
		"run_id": plan.RunID, "attestation": o.Hash, "result": att.Result, "reason": att.Reason,
		"verifier_session": att.VerifierSession, "manifest": att.Manifest, "commit": att.Commit,
		"check_definition": att.CheckDefinition, "setup": att.Setup, "check": att.Check, "kept_worktree": kept,
	}
	res, err := s.publish(pctx, record.Op{ID: r.OperationID, Name: r.Operation, Digest: digest, Signer: verifier}, func(*index.Index) (any, []envelope.Object, []record.Effect, error) {
		return result, []envelope.Object{o}, nil, nil
	})
	switch {
	case err == nil || apierr.HasCode(err, "cleanup_pending"):
		_ = evidence.SetState(rt, plan.RunID, evidence.StatePublished, "")
	default:
		// The group may still be completed by recovery; restart settles the
		// journal against the committed operation.
		_ = evidence.SetState(rt, plan.RunID, evidence.StateInterrupted, "publication_failed")
	}
	if err == nil || apierr.HasCode(err, "cleanup_pending") {
		s.endVerifier(pctx, plan.VerifierSession)
	}
	return res, err
}

// endVerifier ends a run's verifier session with a root-signed event and
// forgets its key. If the root key is unavailable the session stays active
// until startup reconciliation ends it. Callers hold mu.
func (s *Service) endVerifier(ctx context.Context, sid string) {
	defer delete(s.signers, sid)
	root, err := s.root()
	if err != nil {
		s.recoveryPending = true
		return
	}
	sub, err := s.subject(ctx, sid, "session")
	if err != nil || sub.State != "active" {
		return
	}
	reason := "evidence_run_complete"
	event, err := envelope.Seal(schema.KindLifecycleEvent, transition(*sub, "ended", &reason, root.ID()), root)
	if err != nil {
		return
	}
	hash := canon.Hash([]byte("evidence.verifier_end:" + sid))
	_, err = s.Store.Do(ctx, record.Op{ID: "op_" + hash[:26], Name: "session.end", Digest: hash, Signer: root}, func(*index.Index) (any, []envelope.Object, error) {
		return map[string]string{"session_id": sid}, []envelope.Object{event}, nil
	})
	if err != nil {
		s.recoveryPending = true
	}
}

// settleRuns marks interrupted journals whose operation did commit as
// published: recovery completed the group the crash had left half written.
func (s *Service) settleRuns(ctx context.Context) error {
	journals, err := evidence.ListJournals(s.runtimeDir())
	if err != nil {
		return err
	}
	for _, j := range journals {
		if j.State != evidence.StateInterrupted || j.OperationID == "" {
			continue
		}
		ops, err := s.Store.Index.Operations(ctx, j.OperationID)
		if err != nil {
			return err
		}
		if len(ops) > 0 {
			if err := evidence.SetState(s.runtimeDir(), j.RunID, evidence.StatePublished, ""); err != nil {
				return err
			}
		}
	}
	return nil
}
