package core

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/evidence"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/lifecycle"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/selection"
	"github.com/CoachCoe/Ledger/internal/speculate"
)

// Ledger never runs agents (ADR 0023). A run hands the caller one scoped
// workspace per attempt, freezes what each attempt submits, runs the
// intent's pinned checks against every frozen candidate under verifier
// sessions, scores the eligible ones and records the outcome of every
// attempt in one operation group. Nothing is merged.

// maxEvidenceGenerations bounds how often one candidate's check is re-run
// after interrupted evidence runs.
const maxEvidenceGenerations = 16

// attemptGrace is how long attempt capabilities outlive the run deadline.
const attemptGrace = 10 * time.Minute

type startInput struct {
	IntentID           string            `json:"intent_id"`
	N                  int               `json:"n"`
	Strategies         []schema.Strategy `json:"strategies"`
	Scorer             scorerInput       `json:"scorer"`
	Deadline           string            `json:"deadline"`
	DeadlineSeconds    int64             `json:"deadline_seconds"`
	BaseCommit         string            `json:"base_commit"`
	BaseTree           string            `json:"base_tree"`
	Allow              []string          `json:"allow"`
	Deny               []string          `json:"deny"`
	MayAddDependencies *bool             `json:"may_add_dependencies"`
}

type scorerInput struct {
	Kind           string   `json:"kind"`
	Command        string   `json:"command"`
	TimeoutSeconds int64    `json:"timeout_seconds"`
	EnvAllowlist   []string `json:"env_allowlist"`
}

type runRef struct {
	RunID string `json:"run_id"`
}

func (in scorerInput) definition() (schema.ScorerDefinition, error) {
	d := schema.ScorerDefinition{Kind: in.Kind, EnvAllowlist: in.EnvAllowlist}
	if d.EnvAllowlist == nil {
		d.EnvAllowlist = []string{}
	}
	if in.Command != "" {
		d.Command = &in.Command
	}
	if in.TimeoutSeconds != 0 {
		d.TimeoutSeconds = &in.TimeoutSeconds
	}
	return d, speculate.ValidateScorer(d)
}

// startRun creates a run and its attempts as one group: n root-issued agent
// sessions and capabilities, n workspaces on one base, the run creation
// record binding them, and the run's move to collecting. The handoff is
// returned at once; Ledger does not wait for, or start, any agent.
func (s *Service) startRun(ctx context.Context, c *invocation) (prepared, error) {
	if err := requireRoot(c); err != nil {
		return prepared{}, err
	}
	in, err := decode[startInput](c.Input)
	if err != nil {
		return prepared{}, err
	}
	if err := s.leaf(ctx, in.IntentID); err != nil {
		return prepared{}, err
	}
	intent, err := creation[schema.Intent](ctx, s.Store.Index, in.IntentID, schema.KindIntent)
	if err != nil {
		return prepared{}, err
	}
	checks := speculate.PinnedChecks(intent)
	if len(checks) == 0 {
		return prepared{}, apierr.New("no_check_criterion", "intent %s has no check-kind criterion; speculation needs something to score mechanically", in.IntentID).With("intent_id", in.IntentID)
	}
	for _, h := range checks {
		def, err := s.Store.Index.CheckDefinition(ctx, h)
		if err != nil {
			return prepared{}, err
		}
		if err := evidence.Runnable(def); err != nil {
			return prepared{}, apierr.New("not_runnable", "check %s: %v", def.Name, err)
		}
	}
	n := in.N
	if n == 0 {
		n = len(in.Strategies)
	}
	if n < len(in.Strategies) || n > speculate.MaxAttempts {
		return prepared{}, apierr.New("invalid_input", "n must be between the number of strategies (%d) and %d", len(in.Strategies), speculate.MaxAttempts)
	}
	scorer, err := in.Scorer.definition()
	if err != nil {
		return prepared{}, err
	}
	scorerHash, err := speculate.ScorerHash(scorer)
	if err != nil {
		return prepared{}, err
	}
	start := time.Now()
	deadline, err := runDeadline(in, start)
	if err != nil {
		return prepared{}, err
	}
	base, err := s.forkBase(ctx, in.BaseCommit, in.BaseTree)
	if err != nil {
		return prepared{}, err
	}
	// Attempt authority outlives the deadline briefly, so a late submission
	// is refused as deadline_passed rather than as an expired capability.
	end, err := schema.ParseTimestamp(deadline)
	if err != nil {
		return prepared{}, err
	}
	expiry := schema.Timestamp(end.Add(attemptGrace))
	runID := schema.NewID(schema.PrefixRun)
	created := schema.Timestamp(start)
	var records []envelope.Object
	var effects []record.Effect
	var bindings []schema.CandidateBinding
	handoff := []map[string]any{}
	for i := range n {
		st := in.Strategies[i%len(in.Strategies)]
		session, recs, err := s.newSession(c, sessionInput{
			AgentName: "speculation-" + st.Name, ParticipantKind: "agent", ExpiresAt: expiry,
			Allow: slices.Clone(in.Allow), Deny: slices.Clone(in.Deny), MayAddDependencies: in.MayAddDependencies,
		})
		if err != nil {
			return prepared{}, err
		}
		wid := schema.NewID(schema.PrefixWorkspace)
		path := s.Provider.Path(wid)
		w, err := s.seal(schema.KindWorkspace, base.workspace(schema.Workspace{ID: wid, IntentID: in.IntentID, SessionID: session["session_id"], CapabilityID: session["capability_id"], ChangeID: schema.NewID(schema.PrefixChange), CreatedAt: created}), c.signer)
		if err != nil {
			return prepared{}, err
		}
		records = append(append(records, recs...), w)
		effects = append(effects, base.create(wid, path, session["session_id"]))
		bindings = append(bindings, schema.CandidateBinding{WorkspaceID: wid, Strategy: st.Name})
		handoff = append(handoff, map[string]any{
			"workspace_id": wid, "session_id": session["session_id"], "capability_id": session["capability_id"],
			"strategy": st.Name, "spec": st.Spec, "path": path,
		})
	}
	run := schema.SpeculationRun{ID: runID, IntentID: in.IntentID, Strategies: in.Strategies, Attempts: bindings, Checks: checks,
		Scorer: scorerHash, ScorerDefinition: scorer, BaseCommit: base.commit, BaseTree: base.tree, Deadline: deadline, CreatedAt: created}
	ro, err := s.seal(schema.KindSpeculationRun, run, c.signer)
	if err != nil {
		return prepared{}, err
	}
	sub := index.Subject{ID: runID, Kind: lifecycle.SpeculationRun, Creation: ro.Hash, State: "created"}
	ev, err := s.seal(schema.KindLifecycleEvent, transition(sub, "collecting", nil, c.signer.ID()), c.signer)
	if err != nil {
		return prepared{}, err
	}
	return prepared{
		result: base.result(map[string]any{
			"run_id": runID, "intent_id": in.IntentID, "state": "collecting", "deadline": deadline,
			"scorer": scorerHash, "checks": checks, "attempts": handoff,
		}),
		records: append(records, ro, ev),
		effects: effects,
	}, nil
}

// phase1 adds fields that only a Phase 1 repository reports, such as the
// commit and branch a proposal publishes; a native proposal has neither.
func (s *Service) phase1(m, fields map[string]any) map[string]any {
	return phase1(!s.native(), m, fields)
}

func phase1(git bool, m, fields map[string]any) map[string]any {
	if git {
		maps.Copy(m, fields)
	}
	return m
}

// runBase is the base a run's workspaces were forked from.
func runBase(r schema.SpeculationRun) baseRef { return baseRef{commit: r.BaseCommit, tree: r.BaseTree} }

func runDeadline(in startInput, start time.Time) (string, error) {
	var at time.Time
	if in.Deadline != "" {
		t, err := schema.ParseTimestamp(in.Deadline)
		if err != nil {
			return "", apierr.New("invalid_input", "deadline: %v", err)
		}
		at = t
	} else {
		at = start.Add(time.Duration(in.DeadlineSeconds) * time.Second)
	}
	if !at.After(start) {
		return "", apierr.New("invalid_input", "the deadline must be in the future")
	}
	return schema.Timestamp(at), nil
}

// runState is a run and its attempts as currently indexed.
type runState struct {
	sub      *index.Subject
	run      *index.Run
	attempts []*attemptState
}

type attemptState struct {
	binding  schema.CandidateBinding
	sub      *index.Subject
	proposal index.Proposal // Manifest is "" when nothing was submitted
	manifest schema.ChangeManifest
}

func (s *Service) loadRun(ctx context.Context, id string) (*runState, error) {
	sub, err := s.subject(ctx, id, lifecycle.SpeculationRun)
	if err != nil {
		return nil, err
	}
	run, err := s.Store.Index.RunCreation(ctx, id)
	if err != nil {
		return nil, err
	}
	rs := &runState{sub: sub, run: run}
	for _, b := range run.Attempts {
		a := &attemptState{binding: b}
		if a.sub, err = s.subject(ctx, b.WorkspaceID, "workspace"); err != nil {
			return nil, err
		}
		ms, err := s.Store.Index.WorkspaceManifests(ctx, b.WorkspaceID)
		if err != nil {
			return nil, err
		}
		if len(ms) > 1 {
			return nil, apierr.New("proposal_conflict", "run workspace %s has more than one manifest", b.WorkspaceID)
		}
		if len(ms) == 1 {
			if a.proposal, err = s.Store.Index.LastProposal(ctx, b.WorkspaceID); err != nil {
				return nil, err
			}
			if a.proposal.Manifest != ms[0] {
				return nil, apierr.New("proposal_conflict", "run workspace %s has no unambiguous frozen candidate", b.WorkspaceID)
			}
			if a.manifest, err = s.Store.Index.Manifest(ctx, ms[0]); err != nil {
				return nil, err
			}
		}
		rs.attempts = append(rs.attempts, a)
	}
	return rs, nil
}

func (rs *runState) strategy(name string) string {
	for _, st := range rs.run.Strategies {
		if st.Name == name {
			return st.Spec
		}
	}
	return name
}

// derivedOp is a deterministic operation identity for a step the service
// takes on a run's behalf, so a retry or restart converges on one result.
func derivedOp(parts ...string) (string, string) {
	h := canon.Hash([]byte(joinParts(parts)))
	return "op_" + h[:26], h
}

func joinParts(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\x00"
		}
		out += p
	}
	return out
}

// transitionRun publishes one root-signed run event. Callers hold mu.
func (s *Service) transitionRun(ctx context.Context, rs *runState, to, reason string) error {
	root, err := s.root()
	if err != nil {
		return apierr.New("root_unavailable", "speculation needs the root key: %v", err)
	}
	id, digest := derivedOp("speculation.transition", rs.sub.ID, rs.sub.Head, to)
	var why *string
	if reason != "" {
		why = &reason
	}
	ev, err := s.seal(schema.KindLifecycleEvent, transition(*rs.sub, to, why, root.ID()), root)
	if err != nil {
		return err
	}
	_, err = s.Store.Do(ctx, record.Op{ID: id, Name: "speculation.transition", Digest: digest, Signer: root}, func(*index.Index) (any, []envelope.Object, error) {
		return map[string]string{"run_id": rs.sub.ID, "state": to}, []envelope.Object{ev}, nil
	})
	return err
}

// interruptRun moves a collecting or evaluating run to interrupted. A failed
// interruption leaves the run evaluating; the error that caused it is
// still returned to the caller. Callers hold mu.
func (s *Service) interruptRun(ctx context.Context, id, reason string) {
	rs, err := s.loadRun(ctx, id)
	if err != nil || (rs.sub.State != "collecting" && rs.sub.State != "evaluating") {
		return
	}
	_ = s.transitionRun(context.WithoutCancel(ctx), rs, "interrupted", reason)
}

// interruptEvaluations marks runs left evaluating by a previous process as
// interrupted: their evidence runs and scorers died with it. Without the
// root key they stay evaluating and evaluate continues them directly.
func (s *Service) interruptEvaluations(ctx context.Context) error {
	if _, err := s.root(); err != nil {
		return nil //nolint:nilerr // runs are left evaluating until the root key is available
	}
	runs, err := s.Store.Index.SubjectsIn(ctx, lifecycle.SpeculationRun, "evaluating")
	if err != nil {
		return err
	}
	for _, sub := range runs {
		if sub.Conflict != "" {
			continue
		}
		rs, err := s.loadRun(ctx, sub.ID)
		if err != nil {
			return err
		}
		if err := s.transitionRun(ctx, rs, "interrupted", "daemon_restart"); err != nil {
			return err
		}
	}
	return nil
}

// candidate is one frozen submission and its evaluation.
type candidate struct {
	attempt  *attemptState
	binding  selection.Binding
	evidence []string // attestations this run produced
	set      []selection.Evidence
	known    []selection.Evidence
	eval     selection.Result
	eligible bool
	failed   []string
}

// evaluateCandidates gathers each frozen candidate's evidence: one
// attestation per pinned check, produced under a derived operation id so a
// retry replays it. With execute false it only reads what is committed and
// reports evidence_incomplete for anything missing.
func (s *Service) evaluateCandidates(ctx context.Context, rs *runState, execute bool) (map[string]*candidate, error) {
	intent, err := creation[schema.Intent](ctx, s.Store.Index, rs.run.IntentID, schema.KindIntent)
	if err != nil {
		return nil, err
	}
	var submitted []*attemptState
	for _, a := range rs.attempts {
		if a.proposal.Manifest != "" {
			submitted = append(submitted, a)
		}
	}
	found, err := s.runAllEvidence(ctx, rs.run.ID, submitted, rs.run.Checks, execute)
	if err != nil {
		return nil, err
	}
	out := map[string]*candidate{}
	for i, a := range submitted {
		c := &candidate{attempt: a, binding: selection.Binding{Manifest: a.proposal.Manifest, Commit: a.proposal.Commit, Tree: a.manifest.Tree, Criteria: intent.Criteria}}
		for _, hashes := range found[i] {
			c.evidence = append(c.evidence, hashes...)
		}
		slices.Sort(c.evidence)
		if c.known, err = s.Store.Index.EvidenceFor(ctx, a.proposal.Manifest); err != nil {
			return nil, err
		}
		for _, e := range c.known {
			if slices.Contains(c.evidence, e.Hash) || e.Att.Category == "judgment" {
				c.set = append(c.set, e)
			}
		}
		c.eval = selection.Evaluate(c.binding, c.set, c.known)
		c.eligible = true
		for _, st := range c.eval.Criteria {
			i := slices.IndexFunc(intent.Criteria, func(cr schema.Criterion) bool { return cr.ID == st.CriterionID })
			if i >= 0 && intent.Criteria[i].Verification.Kind == "check" && st.Status != selection.Satisfied {
				c.eligible = false
				c.failed = append(c.failed, st.CriterionID)
			}
		}
		slices.Sort(c.failed)
		out[a.binding.WorkspaceID] = c
	}
	return out, nil
}

// nativeEvidenceWorkers bounds the checks one evaluation runs at once in a
// native repository, whose copy-on-write checkouts take no repository-wide
// lock (ADR 0026). Phase 1 worktrees are added one at a time.
const nativeEvidenceWorkers = 4

// runAllEvidence runs runEvidence for every attempt and check, returning
// the attestations by attempt and check index.
func (s *Service) runAllEvidence(ctx context.Context, runID string, attempts []*attemptState, checks []string, execute bool) ([][][]string, error) {
	found := make([][][]string, len(attempts))
	for i := range found {
		found[i] = make([][]string, len(checks))
	}
	slots := make(chan struct{}, s.evidenceWorkers)
	errs := make([]error, len(attempts)*len(checks))
	var wg sync.WaitGroup
	for i, a := range attempts {
		for j, check := range checks {
			wg.Go(func() {
				slots <- struct{}{}
				defer func() { <-slots }()
				found[i][j], errs[i*len(checks)+j] = s.runEvidence(ctx, runID, a, check, execute)
			})
		}
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return found, nil
}

// runEvidence returns the attestations this run holds for one candidate and
// check, running the check when none is committed. An interrupted evidence
// run cannot be retried under its operation id, so the next generation's
// id is used; committed generations are all reported, never hidden.
func (s *Service) runEvidence(ctx context.Context, runID string, a *attemptState, check string, execute bool) ([]string, error) {
	journals, err := evidence.ListJournals(s.runtimeDir())
	if err != nil {
		return nil, err
	}
	var found []string
	for gen := range maxEvidenceGenerations {
		id, _ := derivedOp("speculation.evidence", runID, a.proposal.Manifest, check, strconv.Itoa(gen))
		ops, err := s.Store.Index.Operations(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(ops) > 0 {
			var res struct {
				Attestation string `json:"attestation"`
			}
			if err := json.Unmarshal([]byte(ops[0].Result), &res); err != nil {
				return nil, err
			}
			found = append(found, res.Attestation)
			continue
		}
		interrupted := slices.ContainsFunc(journals, func(j evidence.Journal) bool {
			return j.OperationID == id && j.State == evidence.StateInterrupted
		})
		if interrupted {
			continue
		}
		if len(found) > 0 {
			return found, nil
		}
		if !execute {
			return nil, apierr.New("evidence_incomplete", "candidate %s has no committed evidence for check %s; evaluate the run first", a.proposal.Manifest, check)
		}
		input, err := canon.Marshal(s.phase1(map[string]any{"manifest": a.proposal.Manifest, "check_definition": check}, map[string]any{"commit": a.proposal.Commit}))
		if err != nil {
			return nil, err
		}
		res, err := s.evidenceRun(ctx, api.Request{Version: s.apiVersion(), Operation: "evidence.run", OperationID: id, Input: input})
		if apierr.HasCode(err, "run_interrupted") {
			continue
		}
		if err != nil {
			return nil, err
		}
		var out struct {
			Attestation string `json:"attestation"`
		}
		if err := json.Unmarshal(res.(*record.Result).Result, &out); err != nil {
			return nil, err
		}
		return []string{out.Attestation}, nil
	}
	if len(found) > 0 {
		return found, nil
	}
	return nil, apierr.New("evidence_unavailable", "check %s for candidate %s was interrupted %d times", check, a.proposal.Manifest, maxEvidenceGenerations)
}

// eligibleIDs lists the eligible candidates' manifests in ascending order.
func eligibleIDs(cands map[string]*candidate) []string {
	ids := []string{}
	for _, c := range cands {
		if c.eligible {
			ids = append(ids, c.binding.Manifest)
		}
	}
	slices.Sort(ids)
	return ids
}

// scorerDocument is the frozen candidate JSON a scorer receives.
func scorerDocument(rs *runState, cands map[string]*candidate) ([]byte, error) {
	list := []map[string]any{}
	for _, id := range eligibleIDs(cands) {
		for _, c := range cands {
			if c.binding.Manifest != id {
				continue
			}
			list = append(list, runBase(rs.run.SpeculationRun).result(phase1(rs.run.BaseCommit != "", map[string]any{
				"id": id, "workspace_id": c.attempt.binding.WorkspaceID, "strategy": c.attempt.binding.Strategy,
				"manifest": id, "tree": c.binding.Tree,
				"files": c.attempt.manifest.Files, "evidence": c.evidence, "criteria": c.eval.Criteria,
			}, map[string]any{"commit": c.binding.Commit, "branch": c.attempt.proposal.Branch})))
		}
	}
	return canon.Marshal(map[string]any{"run_id": rs.run.ID, "intent_id": rs.run.IntentID, "scorer": rs.run.Scorer, "candidates": list})
}

// evaluateRun closes collection, evaluates every frozen candidate and, with
// a command scorer, resolves the run under the caller's operation id. Long
// work runs without the service lock. A scorer failure interrupts the run
// without a winner; resume and evaluate again to retry with the same frozen
// candidates and evidence.
func (s *Service) evaluateRun(ctx context.Context, r api.Request) (any, error) {
	if r.SessionID != "" {
		return nil, apierr.New("unauthorized", "speculation.evaluate requires root authorization")
	}
	in, err := decode[runRef](r.Input)
	if err != nil {
		return nil, err
	}
	digest, err := r.Digest()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if res, err := s.committed(ctx, r, digest); res != nil || err != nil {
		s.mu.Unlock()
		return res, err
	}
	if s.evaluating[in.RunID] != nil {
		s.mu.Unlock()
		return nil, apierr.New("run_in_progress", "run %s is already being evaluated", in.RunID).With("run_id", in.RunID)
	}
	// A disconnecting client does not stop evaluation: cancelled checks
	// would be committed as failed evidence. Only speculation.cancel does.
	ectx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	s.evaluating[in.RunID] = cancel
	defer func() {
		s.mu.Lock()
		delete(s.evaluating, in.RunID)
		s.mu.Unlock()
	}()
	rs, err := s.beginEvaluation(ctx, in.RunID)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	cands, err := s.evaluateCandidates(ectx, rs, true)
	if err == nil && ectx.Err() != nil {
		err = ectx.Err()
	}
	if err != nil {
		return nil, s.cancelledOr(ctx, in.RunID, err)
	}
	var scores map[string]int64
	if ids := eligibleIDs(cands); len(ids) > 0 {
		doc, err := scorerDocument(rs, cands)
		if err != nil {
			return nil, err
		}
		if rs.run.ScorerDefinition.Kind == "submission" {
			var candidates jsontext.Value = doc
			return map[string]any{"run_id": rs.run.ID, "state": "evaluating", "awaiting_scores": true, "eligible": ids, "candidates": candidates}, nil
		}
		if scores, err = s.runScorer(ectx, rs, doc, ids); err != nil {
			if err = s.cancelledOr(ctx, in.RunID, err); apierr.HasCode(err, "run_cancelled") {
				return nil, err
			}
			s.mu.Lock()
			s.interruptRun(ctx, in.RunID, apierr.As(err).Code)
			s.mu.Unlock()
			return nil, apierr.As(err).With("run_id", in.RunID).With("interrupted", true)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolve(ctx, r, digest, in.RunID, cands, scores)
}

// committed returns the frozen result of an already committed operation.
// Callers hold mu.
func (s *Service) committed(ctx context.Context, r api.Request, digest string) (*record.Result, error) {
	res, err := s.Store.DoEffects(ctx, record.Op{ID: r.OperationID, Name: r.Operation, Digest: digest}, func(*index.Index) (any, []envelope.Object, []record.Effect, error) {
		return nil, nil, nil, errNotCommitted
	})
	if errors.Is(err, errNotCommitted) {
		return nil, nil
	}
	return res, err
}

// beginEvaluation moves a collecting run to evaluating. Callers hold mu.
func (s *Service) beginEvaluation(ctx context.Context, id string) (*runState, error) {
	rs, err := s.loadRun(ctx, id)
	if err != nil {
		return nil, err
	}
	switch rs.sub.State {
	case "collecting":
		reason := "collection_closed"
		if speculate.DeadlinePassed(rs.run.SpeculationRun, time.Now()) {
			reason = "deadline_passed"
		}
		if err := s.transitionRun(ctx, rs, "evaluating", reason); err != nil {
			return nil, err
		}
		return s.loadRun(ctx, id)
	case "evaluating":
		return rs, nil
	case "interrupted":
		return nil, apierr.New("run_interrupted", "run %s is interrupted; resume it before evaluating", id).With("run_id", id).With("resume", rs.sub.Resume)
	}
	return nil, apierr.New("invalid_state", "run %s is %s", id, rs.sub.State).With("run_id", id).With("state", rs.sub.State)
}

// cancelledOr reports run_cancelled when the run was cancelled while err
// happened, and err otherwise.
func (s *Service) cancelledOr(ctx context.Context, id string, err error) error {
	if sub, e := s.Store.Index.Subject(ctx, id); e == nil && sub != nil && sub.State == "cancelled" {
		return apierr.New("run_cancelled", "run %s was cancelled during evaluation", id).With("run_id", id)
	}
	return err
}

// scoreRun accepts scores for a submission scorer and resolves the run.
// Scores that do not cover exactly the eligible candidates with safe
// integers interrupt the run without a winner.
func (s *Service) scoreRun(ctx context.Context, r api.Request) (any, error) {
	if r.SessionID != "" {
		return nil, apierr.New("unauthorized", "speculation.score requires root authorization")
	}
	in, err := decode[struct {
		RunID  string         `json:"run_id"`
		Scores jsontext.Value `json:"scores"`
	}](r.Input)
	if err != nil {
		return nil, err
	}
	digest, err := r.Digest()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if res, err := s.committed(ctx, r, digest); res != nil || err != nil {
		return res, err
	}
	rs, err := s.loadRun(ctx, in.RunID)
	if err != nil {
		return nil, err
	}
	if rs.sub.State != "evaluating" || rs.run.ScorerDefinition.Kind != "submission" {
		return nil, apierr.New("invalid_state", "run %s is %s and does not await submitted scores", in.RunID, rs.sub.State).With("run_id", in.RunID)
	}
	cands, err := s.evaluateCandidates(ctx, rs, false)
	if err != nil {
		return nil, err
	}
	ids := eligibleIDs(cands)
	if len(ids) == 0 {
		return nil, apierr.New("invalid_state", "run %s has no eligible candidate to score; evaluate resolves it", in.RunID)
	}
	raw, err := canon.Marshal(map[string]jsontext.Value{"scores": in.Scores})
	if err != nil {
		return nil, err
	}
	scores, err := speculate.ParseScores(raw, ids)
	if err != nil {
		s.interruptRun(ctx, in.RunID, "invalid_scores")
		return nil, apierr.As(err).With("run_id", in.RunID).With("interrupted", true)
	}
	return s.resolve(ctx, r, digest, in.RunID, cands, scores)
}

// resolve publishes the run's terminal group under the caller's operation.
// Callers hold mu.
func (s *Service) resolve(ctx context.Context, r api.Request, digest, id string, cands map[string]*candidate, scores map[string]int64) (any, error) {
	root, err := s.root()
	if err != nil {
		return nil, apierr.New("root_unavailable", "speculation needs the root key: %v", err)
	}
	return s.publish(context.WithoutCancel(ctx), record.Op{ID: r.OperationID, Name: r.Operation, Digest: digest, Signer: root}, func(*index.Index) (any, []envelope.Object, []record.Effect, error) {
		rs, err := s.loadRun(ctx, id)
		if err != nil {
			return nil, nil, nil, err
		}
		if rs.sub.State != "evaluating" {
			return nil, nil, nil, s.cancelledOr(ctx, id, apierr.New("invalid_state", "run %s is %s", id, rs.sub.State))
		}
		for _, a := range rs.attempts {
			c := cands[a.binding.WorkspaceID]
			if (c == nil) != (a.proposal.Manifest == "") || (c != nil && c.binding.Manifest != a.proposal.Manifest) {
				return nil, nil, nil, apierr.New("candidates_changed", "run %s's frozen candidates changed during evaluation", id)
			}
		}
		p, err := s.endRun(ctx, rs, root, "resolved", "evaluated", cands, scores)
		return p.result, p.records, p.effects, err
	})
}

// cancelRun cancels a run that has not ended: nothing is selected and every
// attempt is closed with outcome cancelled.
type cancelInput struct {
	RunID  string `json:"run_id"`
	Reason string `json:"reason"`
}

func (s *Service) cancelRun(ctx context.Context, c *invocation) (prepared, error) {
	if err := requireRoot(c); err != nil {
		return prepared{}, err
	}
	in, err := decode[cancelInput](c.Input)
	if err != nil {
		return prepared{}, err
	}
	rs, err := s.loadRun(ctx, in.RunID)
	if err != nil {
		return prepared{}, err
	}
	if rs.sub.State == "resolved" || rs.sub.State == "cancelled" {
		return prepared{}, apierr.New("invalid_state", "run %s already ended %s", in.RunID, rs.sub.State).With("run_id", in.RunID).With("state", rs.sub.State)
	}
	cands, err := s.committedCandidates(ctx, rs)
	if err != nil {
		return prepared{}, err
	}
	return s.endRun(ctx, rs, c.signer, "cancelled", cmp.Or(in.Reason, "cancelled"), cands, nil)
}

// committedCandidates evaluates each frozen candidate over the evidence the
// run committed so far, without running anything.
func (s *Service) committedCandidates(ctx context.Context, rs *runState) (map[string]*candidate, error) {
	intent, err := creation[schema.Intent](ctx, s.Store.Index, rs.run.IntentID, schema.KindIntent)
	if err != nil {
		return nil, err
	}
	out := map[string]*candidate{}
	for _, a := range rs.attempts {
		if a.proposal.Manifest == "" {
			continue
		}
		c := &candidate{attempt: a, binding: selection.Binding{Manifest: a.proposal.Manifest, Commit: a.proposal.Commit, Tree: a.manifest.Tree, Criteria: intent.Criteria}}
		for _, check := range rs.run.Checks {
			hashes, err := s.runEvidence(ctx, rs.run.ID, a, check, false)
			if apierr.HasCode(err, "evidence_incomplete") {
				continue
			}
			if err != nil {
				return nil, err
			}
			c.evidence = append(c.evidence, hashes...)
		}
		slices.Sort(c.evidence)
		out[a.binding.WorkspaceID] = c
	}
	return out, nil
}

// endRun builds the run's terminal group: the run event, the winner's
// selection and selected event, a closed event and attempt record for every
// other attempt, ended attempt sessions, and worktree removal for closed
// attempts after publication.
func (s *Service) endRun(ctx context.Context, rs *runState, root identity.Signer, to, reason string, cands map[string]*candidate, scores map[string]int64) (prepared, error) {
	seal := func(kind schema.Kind, p any) (envelope.Object, error) { return s.seal(kind, p, root) }
	ev, err := seal(schema.KindLifecycleEvent, transition(*rs.sub, to, &reason, root.ID()))
	if err != nil {
		return prepared{}, err
	}
	records := []envelope.Object{ev}
	var effects []record.Effect
	winner := ""
	if to == "resolved" {
		winner = speculate.Winner(scores)
	}
	closedAt, err := s.collectionClosed(ctx, rs)
	if err != nil {
		return prepared{}, err
	}
	timedOut := speculate.DeadlinePassed(rs.run.SpeculationRun, closedAt)
	result := map[string]any{"run_id": rs.run.ID, "state": to, "selection": nil, "winner": nil, "scores": nil, "accepted": false, "merged": false}
	if scores != nil {
		result["scores"] = scores
	}
	outcomes := []map[string]any{}
	for _, a := range rs.attempts {
		c := cands[a.binding.WorkspaceID]
		wid := a.binding.WorkspaceID
		row := s.phase1(map[string]any{"workspace_id": wid, "strategy": a.binding.Strategy, "manifest": nil, "attempt_record": nil, "score": nil, "failed_criteria": []string{}}, map[string]any{"commit": nil})
		if c != nil {
			row["manifest"] = c.binding.Manifest
			s.phase1(row, map[string]any{"commit": c.binding.Commit})
		}
		if c != nil && c.binding.Manifest == winner {
			competitors := []string{}
			for _, other := range cands {
				if other != c {
					competitors = append(competitors, other.binding.Manifest)
				}
			}
			scoring, err := canon.Marshal(speculate.Scoring{Scorer: rs.run.Scorer, Eligible: eligibleIDs(cands), Scores: scores, Rule: speculate.TieRule})
			if err != nil {
				return prepared{}, err
			}
			d, err := selection.Build(c.binding, rs.run.IntentID, c.set, c.known, schema.Selector{Kind: "speculation_run", ID: rs.run.ID}, competitors, scoring, now())
			if err != nil {
				return prepared{}, err
			}
			sel, err := seal(schema.KindSelectionDecision, d)
			if err != nil {
				return prepared{}, err
			}
			why := "speculation_selected"
			wev, err := seal(schema.KindLifecycleEvent, transition(*a.sub, "selected", &why, root.ID()))
			if err != nil {
				return prepared{}, err
			}
			records = append(records, sel, wev)
			result["selection"] = sel.Hash
			result["winner"] = s.phase1(map[string]any{"workspace_id": wid, "manifest": c.binding.Manifest}, map[string]any{"commit": c.binding.Commit, "branch": a.proposal.Branch})
			row["outcome"], row["score"] = "selected", scores[winner]
			row["selection"] = sel.Hash
			outcomes = append(outcomes, row)
			continue
		}
		rec := schema.AttemptRecord{RunID: rs.run.ID, IntentID: rs.run.IntentID, WorkspaceID: wid, Evidence: []string{}, FailedCriteria: []string{}, TargetSymbols: []schema.Symbol{}}
		spec := rs.strategy(a.binding.Strategy)
		rec.Approach = &spec
		switch {
		case to == "cancelled":
			rec.Outcome = speculate.OutcomeCancelled
		case c == nil && timedOut:
			rec.Outcome = speculate.OutcomeTimeout
		case c == nil:
			rec.Outcome = speculate.OutcomeNotProposed
		case !c.eligible:
			rec.Outcome, rec.FailedCriteria = speculate.OutcomeFailed, slices.Clone(c.failed)
		default:
			score := scores[c.binding.Manifest]
			rec.Outcome, rec.Score = speculate.OutcomeTiebreaker, &score
		}
		if c != nil {
			m := c.binding.Manifest
			rec.Manifest = &m
			rec.Evidence = setHashes(c)
			if a.manifest.SymbolsWritten != nil {
				rec.TargetSymbols = a.manifest.SymbolsWritten
			}
		}
		ar, err := seal(schema.KindAttemptRecord, rec)
		if err != nil {
			return prepared{}, err
		}
		why := "speculation_" + rec.Outcome
		wev, err := seal(schema.KindLifecycleEvent, transition(*a.sub, "closed", &why, root.ID()))
		if err != nil {
			return prepared{}, err
		}
		records = append(records, ar, wev)
		cleanup := record.Effect{Kind: record.EffectWorktreeRemove, WorkspaceID: wid, Path: s.Provider.Path(wid), After: true}
		if s.native() {
			cleanup.Kind = record.EffectWorkspaceRemove
		} else if c != nil {
			cleanup.Base, cleanup.Tree = rs.run.BaseCommit, c.binding.Tree
		}
		effects = append(effects, cleanup)
		row["outcome"], row["attempt_record"], row["failed_criteria"] = rec.Outcome, ar.Hash, rec.FailedCriteria
		if rec.Score != nil {
			row["score"] = *rec.Score
		}
		outcomes = append(outcomes, row)
	}
	ended := map[string]bool{}
	for _, a := range rs.attempts {
		if ended[a.sub.Owner] {
			continue
		}
		ended[a.sub.Owner] = true
		sub, err := s.subject(ctx, a.sub.Owner, "session")
		if err != nil {
			return prepared{}, err
		}
		if sub.State != "active" {
			continue
		}
		why := "speculation_ended"
		sev, err := seal(schema.KindLifecycleEvent, transition(*sub, "ended", &why, root.ID()))
		if err != nil {
			return prepared{}, err
		}
		records = append(records, sev)
	}
	result["attempts"] = outcomes
	return prepared{result: result, records: records, effects: effects}, nil
}

func setHashes(c *candidate) []string {
	out := slices.Clone(c.evidence)
	for _, e := range c.set {
		out = append(out, e.Hash)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// collectionClosed is when the run stopped collecting: its first move to
// evaluating, or now for a run that never did.
func (s *Service) collectionClosed(ctx context.Context, rs *runState) (time.Time, error) {
	events, err := s.Store.Index.Events(ctx, rs.run.ID)
	if err != nil {
		return time.Time{}, err
	}
	for _, e := range events {
		if e.FromState == "collecting" && e.ToState == "evaluating" {
			return schema.ParseTimestamp(e.At)
		}
	}
	return time.Now(), nil
}

// resumeRun returns an interrupted run to the stage it was interrupted in.
func (s *Service) resumeRun(ctx context.Context, c *invocation) (prepared, error) {
	if err := requireRoot(c); err != nil {
		return prepared{}, err
	}
	in, err := decode[runRef](c.Input)
	if err != nil {
		return prepared{}, err
	}
	rs, err := s.loadRun(ctx, in.RunID)
	if err != nil {
		return prepared{}, err
	}
	if rs.sub.State != "interrupted" || rs.sub.Resume == "" {
		return prepared{}, apierr.New("invalid_state", "run %s is %s, not interrupted", in.RunID, rs.sub.State).With("run_id", in.RunID)
	}
	reason := "resumed"
	ev, err := s.seal(schema.KindLifecycleEvent, transition(*rs.sub, rs.sub.Resume, &reason, c.signer.ID()), c.signer)
	return prepared{result: map[string]string{"run_id": in.RunID, "state": rs.sub.Resume}, records: []envelope.Object{ev}}, err
}

// runStatus reports a run, its attempts and frozen candidates, and how it
// ended. It never runs anything.
func (s *Service) runStatus(ctx context.Context, id string) (any, error) {
	rs, err := s.loadRun(ctx, id)
	if err != nil {
		return nil, err
	}
	records, err := s.Store.Index.Attempts(ctx, id)
	if err != nil {
		return nil, err
	}
	selections, err := s.Store.Index.RunSelections(ctx, id)
	if err != nil {
		return nil, err
	}
	attempts := []map[string]any{}
	for _, a := range rs.attempts {
		row := map[string]any{
			"workspace_id": a.binding.WorkspaceID, "strategy": a.binding.Strategy, "workspace_state": a.sub.State,
			"session_id": a.sub.Owner, "submitted": a.proposal.Manifest != "", "manifest": nil,
			"attempt_record": nil, "outcome": nil,
		}
		s.phase1(row, map[string]any{"commit": nil, "branch": nil})
		if a.proposal.Manifest != "" {
			row["manifest"] = a.proposal.Manifest
			s.phase1(row, map[string]any{"commit": a.proposal.Commit, "branch": a.proposal.Branch})
		}
		if a.sub.State == "selected" {
			row["outcome"] = "selected"
		}
		for _, r := range records {
			if r.WorkspaceID == a.binding.WorkspaceID {
				row["attempt_record"], row["outcome"] = r.Hash, r.Outcome
			}
		}
		attempts = append(attempts, row)
	}
	var selected any
	if len(selections) > 0 {
		selected = selections[0]
	}
	var resume any
	if rs.sub.State == "interrupted" {
		resume = rs.sub.Resume
	}
	var reason any
	if events, err := s.Store.Index.Events(ctx, id); err != nil {
		return nil, err
	} else if len(events) > 0 && events[len(events)-1].Reason != nil {
		reason = *events[len(events)-1].Reason
	}
	return runBase(rs.run.SpeculationRun).result(map[string]any{
		"run_id": id, "intent_id": rs.run.IntentID, "state": rs.sub.State, "resume": resume, "reason": reason,
		"deadline": rs.run.Deadline, "deadline_passed": speculate.DeadlinePassed(rs.run.SpeculationRun, time.Now()),
		"scorer": rs.run.Scorer, "scorer_kind": rs.run.ScorerDefinition.Kind, "checks": rs.run.Checks,
		"attempts": attempts, "selection": selected,
	}), nil
}

// submit freezes an attempt's candidate: the run workspace's one proposal.
func (s *Service) submit(ctx context.Context, c *invocation) (prepared, error) {
	in, err := decode[struct {
		RunID string `json:"run_id"`
		proposeInput
	}](c.Input)
	if err != nil {
		return prepared{}, err
	}
	r, err := s.Store.Index.RunOf(ctx, in.WorkspaceID)
	if err != nil {
		return prepared{}, err
	}
	if r == nil || r.ID != in.RunID {
		return prepared{}, apierr.New("not_an_attempt", "workspace %s is not an attempt of run %s", in.WorkspaceID, in.RunID)
	}
	p, err := s.proposeIn(ctx, c, in.proposeInput)
	if err != nil {
		return prepared{}, err
	}
	result := p.result.(map[string]string)
	result["run_id"] = in.RunID
	return p, nil
}

// submittable refuses a proposal of a run workspace unless its run is
// collecting, the deadline has not passed, and nothing was submitted yet.
func (s *Service) submittable(ctx context.Context, ws *index.Subject) error {
	r, err := s.Store.Index.RunOf(ctx, ws.ID)
	if err != nil || r == nil {
		return err
	}
	sub, err := s.subject(ctx, r.ID, lifecycle.SpeculationRun)
	if err != nil {
		return err
	}
	switch {
	case ws.State != "open":
		return apierr.New("already_submitted", "workspace %s already submitted its candidate to run %s", ws.ID, r.ID).With("run_id", r.ID)
	case sub.State != "collecting":
		return apierr.New("run_not_collecting", "run %s is %s and accepts no submissions", r.ID, sub.State).With("run_id", r.ID).With("state", sub.State)
	case speculate.DeadlinePassed(r.SpeculationRun, time.Now()):
		return apierr.New("deadline_passed", "run %s stopped collecting at %s", r.ID, r.Deadline).With("run_id", r.ID)
	}
	return nil
}

// notInRun refuses moving a workspace of a run that has not ended: only the
// run's resolution or cancellation moves it.
func (s *Service) notInRun(ctx context.Context, workspaceID string) error {
	r, err := s.Store.Index.RunOf(ctx, workspaceID)
	if err != nil || r == nil {
		return err
	}
	sub, err := s.subject(ctx, r.ID, lifecycle.SpeculationRun)
	if err != nil {
		return err
	}
	if sub.State == "resolved" || sub.State == "cancelled" {
		return nil
	}
	return apierr.New("run_active", "workspace %s belongs to speculation run %s (%s); only the run's resolution or cancellation moves it", workspaceID, r.ID, sub.State).With("run_id", r.ID)
}
