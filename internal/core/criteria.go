package core

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/evidence"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/selection"
)

// independenceNote is returned with every independence report: independence
// is a property of who signed, never a claim that the change is correct.
const independenceNote = "independence describes who signed the evidence; it does not establish that the change is correct"

// judgeGrace bounds the human verifier session a root judgment provisions.
const judgeGrace = 10 * time.Minute

// withCommit adds the commit a v1 binding names to result; a v2 binding
// names none.
func withCommit(result map[string]any, commit string) map[string]any {
	if commit != "" {
		result["commit"] = commit
	}
	return result
}

type bindingInput struct {
	Manifest string `json:"manifest"`
	Commit   string `json:"commit"`
}

// criteriaBinding resolves the exact revision named by manifest and commit
// (binding_mismatch if the commit is not the one bound to the manifest) and
// the intent's criteria.
func (s *Service) criteriaBinding(ctx context.Context, manifest, commit string) (evidence.Binding, selection.Binding, error) {
	b, err := evidence.Resolve(ctx, s.Store.Index, evidence.ResolveInput{Manifest: manifest, Commit: commit})
	if err != nil {
		return b, selection.Binding{}, err
	}
	intent, err := creation[schema.Intent](ctx, s.Store.Index, b.IntentID, schema.KindIntent)
	if err != nil {
		return b, selection.Binding{}, err
	}
	return b, selection.Binding{Manifest: b.Manifest, Commit: b.Commit, Tree: b.Tree, Criteria: intent.Criteria}, nil
}

// evidenceIsIndependent lists the attestations recorded for a revision with
// the independence of each signer, and the independent passing ones.
func (s *Service) evidenceIsIndependent(ctx context.Context, r api.Request) (any, error) {
	in, err := decode[bindingInput](r.Input)
	if err != nil {
		return nil, err
	}
	_, sb, err := s.criteriaBinding(ctx, in.Manifest, in.Commit)
	if err != nil {
		return nil, err
	}
	known, err := s.Store.Index.EvidenceFor(ctx, in.Manifest)
	if err != nil {
		return nil, err
	}
	attestations, passing := []map[string]any{}, []string{}
	for _, e := range known {
		var reason any
		if !e.Independent {
			reason = e.IndependenceReason
		}
		var kind any
		if e.SignerKind != "" {
			kind = e.SignerKind
		}
		attestations = append(attestations, map[string]any{
			"hash": e.Hash, "signer": e.Signer, "signer_kind": kind, "independent": e.Independent,
			"reason": reason, "result": e.Att.Result, "category": e.Att.Category, "criteria": e.Att.Criteria,
		})
		if e.Independent && e.Att.Result == "pass" && e.Att.Commit == sb.Commit && e.Att.ExecutedOn() == sb.Tree && selection.Consistent(e.Att) == "" {
			passing = append(passing, e.Hash)
		}
	}
	slices.Sort(passing)
	return withCommit(map[string]any{
		"manifest": in.Manifest, "attestations": attestations,
		"independent_passing": passing, "note": independenceNote,
	}, in.Commit), nil
}

// criteriaStatus evaluates every criterion of the intent for one revision
// over an explicit evidence set, or over all known evidence when none is
// given, and returns the contrary evidence known for the binding.
func (s *Service) criteriaStatus(ctx context.Context, r api.Request) (any, error) {
	in, err := decode[struct {
		bindingInput
		Evidence *[]string `json:"evidence"`
	}](r.Input)
	if err != nil {
		return nil, err
	}
	b, sb, err := s.criteriaBinding(ctx, in.Manifest, in.Commit)
	if err != nil {
		return nil, err
	}
	known, err := s.Store.Index.EvidenceFor(ctx, in.Manifest)
	if err != nil {
		return nil, err
	}
	set := known
	if in.Evidence != nil {
		if set, err = s.explicitSet(ctx, sb, *in.Evidence); err != nil {
			return nil, err
		}
	}
	res := selection.Evaluate(sb, set, known)
	hashes := make([]string, 0, len(set))
	for _, e := range set {
		hashes = append(hashes, e.Hash)
	}
	slices.Sort(hashes)
	return withCommit(map[string]any{
		"intent_id": b.IntentID, "manifest": b.Manifest, "evidence_set": slices.Compact(hashes),
		"criteria": res.Criteria, "contrary_evidence": res.Contrary, "rejected": res.Rejected,
	}, b.Commit), nil
}

// explicitSet loads the named attestations. An unknown hash is
// unknown_evidence; one recorded for another manifest, commit or tree is
// cross_revision_evidence, never silently ignored.
func (s *Service) explicitSet(ctx context.Context, sb selection.Binding, hashes []string) ([]selection.Evidence, error) {
	found, err := s.Store.Index.EvidenceByHash(ctx, hashes)
	if err != nil {
		return nil, err
	}
	byHash := map[string]selection.Evidence{}
	for _, e := range found {
		byHash[e.Hash] = e
	}
	for _, h := range hashes {
		e, ok := byHash[h]
		switch {
		case !ok:
			return nil, apierr.New("unknown_evidence", "no committed evidence attestation %s", h).With("evidence", h)
		case e.Att.Manifest != sb.Manifest || e.Att.Commit != sb.Commit || e.Att.ExecutedOn() != sb.Tree:
			return nil, apierr.New("cross_revision_evidence", "evidence %s was recorded for manifest %s, not %s", h, e.Att.Manifest, sb.Manifest).
				With("evidence", h).With("evidence_manifest", e.Att.Manifest)
		}
	}
	return found, nil
}

type judgeInput struct {
	bindingInput
	CriterionID string  `json:"criterion_id"`
	Result      string  `json:"result"`
	Reason      *string `json:"reason"`
}

// evidenceJudge publishes a judgment attestation for one judgment criterion
// of the proposal's current revision. There is no reviewer input: the
// reviewer kind is the participant kind of the signing session. A session
// caller signs as itself (its kind was fixed by root when it was created);
// a root caller gets a fresh human verifier session that signs and is then
// ended, so human judgment always rests on root authentication.
func (s *Service) evidenceJudge(ctx context.Context, r api.Request) (any, error) {
	in, err := decode[judgeInput](r.Input)
	if err != nil {
		return nil, err
	}
	digest, err := r.Digest()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A committed operation replays its frozen result, even if keys are gone.
	res, err := s.Store.DoEffects(ctx, record.Op{ID: r.OperationID, Name: r.Operation, Digest: digest}, func(*index.Index) (any, []envelope.Object, []record.Effect, error) {
		return nil, nil, nil, errNotCommitted
	})
	if err == nil {
		return res, nil
	}
	if !errors.Is(err, errNotCommitted) {
		return nil, err
	}
	b, sb, err := s.criteriaBinding(ctx, in.Manifest, in.Commit)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(sb.Criteria, func(c schema.Criterion) bool { return c.ID == in.CriterionID })
	if i < 0 {
		return nil, apierr.New("unknown_criterion", "intent %s has no criterion %q", b.IntentID, in.CriterionID)
	}
	if sb.Criteria[i].Verification.Kind != "judgment" {
		return nil, apierr.New("not_judgment", "criterion %q is verified by a check, not a judgment", in.CriterionID)
	}
	last, err := s.Store.Index.LastProposal(ctx, b.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if last.Manifest != b.Manifest {
		return nil, apierr.New("stale_revision", "manifest %s is not the current proposal; judge %s", b.Manifest, last.Manifest).With("current_manifest", last.Manifest)
	}

	reviewer, ended, err := s.judgeSigner(ctx, r, b)
	if err != nil {
		return nil, err
	}
	if ended {
		defer s.endVerifier(context.WithoutCancel(ctx), reviewer)
	}
	signer := s.signers[reviewer]
	sess, err := creation[schema.Session](ctx, s.Store.Index, reviewer, schema.KindSession)
	if err != nil {
		return nil, err
	}
	def := schema.CheckDefinition{Name: "judgment", Category: "judgment", Command: "judgment", WorkingDirectory: ".", EnvAllowlist: []string{}, TimeoutSeconds: 1}
	defRecord, err := s.seal(schema.KindCheckDefinition, def, signer)
	if err != nil {
		return nil, err
	}
	env := schema.Environment{Toolchains: map[string]string{}, Variables: map[string]string{}, Lockfiles: map[string]string{}}
	envHash, err := evidence.EnvironmentHash(env)
	if err != nil {
		return nil, err
	}
	at := now()
	att := schema.EvidenceAttestation{
		Manifest: b.Manifest, Commit: b.Commit, ExecutedTree: b.Tree, CheckDefinition: defRecord.Hash,
		Category: "judgment", Criteria: []string{in.CriterionID}, Command: def.Command,
		Result: in.Result, Reason: in.Reason, EnvironmentHash: envHash, Environment: env,
		VerifierSession: reviewer, StartedAt: at, EndedAt: at, OutputDigest: canon.Hash(nil), Isolation: "none",
	}
	o, err := s.seal(schema.KindEvidenceAttestation, att, signer)
	if err != nil {
		return nil, err
	}
	result := withCommit(map[string]any{
		"attestation": o.Hash, "check_definition": defRecord.Hash, "manifest": b.Manifest,
		"criterion_id": in.CriterionID, "result": in.Result, "reviewer_session": reviewer, "reviewer_kind": sess.ParticipantKind,
	}, b.Commit)
	return s.publish(context.WithoutCancel(ctx), record.Op{ID: r.OperationID, Name: r.Operation, Digest: digest, Signer: signer}, func(*index.Index) (any, []envelope.Object, []record.Effect, error) {
		return result, []envelope.Object{defRecord, o}, nil, nil
	})
}

// judgeSigner returns the session that signs a judgment and whether it was
// provisioned for this call (and so must be ended). Callers hold mu.
func (s *Service) judgeSigner(ctx context.Context, r api.Request, b evidence.Binding) (string, bool, error) {
	if r.SessionID == "" {
		root, err := s.root()
		if err != nil {
			return "", false, apierr.New("root_unavailable", "human judgment needs the root key to provision a verifier session: %v", err)
		}
		in := sessionInput{
			AgentName: "human-reviewer", ParticipantKind: "human",
			ExpiresAt: schema.Timestamp(time.Now().Add(judgeGrace)), Allow: []string{"**"}, Deny: []string{"**"},
		}
		sid, err := s.provisionSession(ctx, r, root, "judge:"+r.OperationID, in)
		return sid, err == nil, err
	}
	if s.signers[r.SessionID] == nil {
		return "", false, apierr.New("session_key_unavailable", "session key was lost or ended; use explicit recovery")
	}
	session, err := creation[schema.Session](ctx, s.Store.Index, r.SessionID, schema.KindSession)
	if err != nil {
		return "", false, err
	}
	if err := s.authority(ctx, r.SessionID, session.CapabilityID); err != nil {
		return "", false, err
	}
	holders, err := s.Store.Index.WorkspaceHolders(ctx, b.WorkspaceID)
	if err != nil {
		return "", false, err
	}
	if r.SessionID == b.AuthorSession || slices.Contains(holders, r.SessionID) {
		return "", false, apierr.New("unauthorized", "the author or a holder of the workspace cannot judge its change")
	}
	return r.SessionID, false, nil
}
