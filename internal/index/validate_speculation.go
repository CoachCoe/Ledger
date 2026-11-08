package index

import (
	"encoding/json/v2"
	"slices"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/lifecycle"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/selection"
	"github.com/CoachCoe/Ledger/internal/speculate"
)

// Speculation publication rules (ADR 0023). A run workspace is proposed at
// most once, only while its run collects and before its deadline. It moves
// to selected or closed only in the group that ends its run, and that group
// accounts for every attempt: one attempt record per closed workspace and at
// most one speculation selection.

var terminalRun = map[string]bool{"resolved": true, "cancelled": true}

// intent returns the creation payload of intent id from this group or the index.
func (v *validator) intent(id string) (schema.Intent, error) {
	var p schema.Intent
	if o, ok := v.local[id]; ok && o.Envelope.Kind == schema.KindIntent {
		return p, o.Decode(&p)
	}
	kind, raw, err := creation(v.ctx, v.q, id)
	if err != nil {
		return p, err
	}
	if kind != schema.KindIntent {
		return p, apierr.New("unknown_entity", "no intent %s", id)
	}
	return p, json.Unmarshal(raw, &p)
}

func (v *validator) checkRun(p *schema.SpeculationRun, signer string) error {
	if isSession(signer) {
		return unauthorized("only the root may start a speculation run")
	}
	if _, err := v.subjectIn(p.IntentID, lifecycle.Intent, "open"); err != nil {
		return err
	}
	intent, err := v.intent(p.IntentID)
	if err != nil {
		return err
	}
	if err := speculate.ValidateRun(*p, intent); err != nil {
		return err
	}
	for _, a := range p.Attempts {
		o, ok := v.local[a.WorkspaceID]
		if !ok || o.Envelope.Kind != schema.KindWorkspace {
			return apierr.New("invalid_run", "attempt workspace %s must be created with its run", a.WorkspaceID)
		}
		var w schema.Workspace
		if err := o.Decode(&w); err != nil {
			return err
		}
		if w.IntentID != p.IntentID || w.BaseCommit != p.BaseCommit {
			return apierr.New("invalid_run", "attempt workspace %s differs from the run's intent or base", a.WorkspaceID)
		}
	}
	return nil
}

// runFor returns the run that binds workspace id, from this group or the
// index, or nil.
func (v *validator) runFor(workspaceID string) (*Run, error) {
	for _, o := range v.objects {
		if o.Envelope.Kind != schema.KindSpeculationRun {
			continue
		}
		var r schema.SpeculationRun
		if err := o.Decode(&r); err != nil {
			return nil, err
		}
		if slices.ContainsFunc(r.Attempts, func(a schema.CandidateBinding) bool { return a.WorkspaceID == workspaceID }) {
			return &Run{Creation: o.Hash, Signer: o.Envelope.Signer, SpeculationRun: r}, nil
		}
	}
	return runOf(v.ctx, v.q, workspaceID)
}

// run returns run id from this group or the index.
func (v *validator) run(id string) (*Run, error) {
	if o, ok := v.local[id]; ok && o.Envelope.Kind == schema.KindSpeculationRun {
		r := &Run{Creation: o.Hash, Signer: o.Envelope.Signer}
		return r, o.Decode(&r.SpeculationRun)
	}
	return runCreation(v.ctx, v.q, id)
}

// ending returns the terminal state this group moves run id to, or "".
func (v *validator) ending(runID string) string {
	for _, o := range v.objects {
		var e schema.LifecycleEvent
		if o.Envelope.Kind == schema.KindLifecycleEvent && o.Decode(&e) == nil && e.SubjectID == runID && terminalRun[e.ToState] {
			return e.ToState
		}
	}
	return ""
}

// workspaceEnding returns the state this group moves workspace id to, or "".
func (v *validator) workspaceEnding(id string) string {
	for _, o := range v.objects {
		var e schema.LifecycleEvent
		if o.Envelope.Kind == schema.KindLifecycleEvent && o.Decode(&e) == nil && e.SubjectID == id && (e.ToState == "selected" || e.ToState == "closed") {
			return e.ToState
		}
	}
	return ""
}

// checkSubmission guards a manifest of a run workspace: the run collects,
// the deadline has not passed at the recorded time, and the workspace has
// no earlier manifest.
func (v *validator) checkSubmission(workspaceID string) error {
	r, err := v.runFor(workspaceID)
	if err != nil || r == nil {
		return err
	}
	rs, err := v.subject(r.ID)
	if err != nil {
		return err
	}
	if rs == nil || rs.State != "collecting" {
		return apierr.New("run_not_collecting", "speculation run %s is not collecting submissions", r.ID).With("run_id", r.ID)
	}
	at, err := time.Parse(time.RFC3339Nano, v.at)
	if err != nil {
		return err
	}
	if speculate.DeadlinePassed(r.SpeculationRun, at) {
		return apierr.New("deadline_passed", "speculation run %s stopped collecting at %s", r.ID, r.Deadline).With("run_id", r.ID)
	}
	earlier, err := workspaceManifests(v.ctx, v.q, workspaceID)
	if err != nil {
		return err
	}
	if len(earlier) > 0 {
		return apierr.New("already_submitted", "workspace %s already submitted %s to run %s", workspaceID, earlier[0], r.ID).With("run_id", r.ID)
	}
	return nil
}

// checkRunWorkspaceEvent guards lifecycle events of run workspaces.
func (v *validator) checkRunWorkspaceEvent(e *schema.LifecycleEvent) error {
	r, err := v.runFor(e.SubjectID)
	if err != nil || r == nil {
		return err
	}
	if e.FromState == "open" && e.ToState == "proposed" {
		return nil
	}
	if (e.ToState == "selected" || e.ToState == "closed") && v.ending(r.ID) != "" {
		return nil
	}
	return apierr.New("run_active", "workspace %s belongs to speculation run %s; only the run's resolution or cancellation may move it from %s", e.SubjectID, r.ID, e.FromState).With("run_id", r.ID)
}

// checkRunEnding verifies that a group ending run id accounts for every
// attempt exactly once.
func (v *validator) checkRunEnding(runID, to string) error {
	r, err := v.run(runID)
	if err != nil {
		return err
	}
	selected := 0
	for _, a := range r.Attempts {
		switch v.workspaceEnding(a.WorkspaceID) {
		case "selected":
			selected++
			if n := v.count(schema.KindSelectionDecision, func(o envelope.Object) bool {
				var d schema.SelectionDecision
				if o.Decode(&d) != nil || d.Selector.Kind != "speculation_run" || d.Selector.ID != runID {
					return false
				}
				m, err := manifestIn(v.ctx, v.q, d.Manifest)
				return err == nil && m.WorkspaceID == a.WorkspaceID
			}); n != 1 {
				return apierr.New("run_incomplete", "selected workspace %s needs exactly one selection by run %s", a.WorkspaceID, runID)
			}
		case "closed":
			if n := v.count(schema.KindAttemptRecord, func(o envelope.Object) bool {
				var p schema.AttemptRecord
				return o.Decode(&p) == nil && p.RunID == runID && p.WorkspaceID == a.WorkspaceID
			}); n != 1 {
				return apierr.New("run_incomplete", "closed workspace %s needs exactly one attempt record", a.WorkspaceID)
			}
		default:
			return apierr.New("run_incomplete", "ending run %s must select or close attempt workspace %s", runID, a.WorkspaceID)
		}
	}
	if selected > 1 || (to == "cancelled" && selected > 0) {
		return apierr.New("run_incomplete", "run %s may select at most one candidate, and none when cancelled", runID)
	}
	if v.count(schema.KindSelectionDecision, func(o envelope.Object) bool {
		var d schema.SelectionDecision
		return o.Decode(&d) == nil && d.Selector.Kind == "speculation_run" && d.Selector.ID == runID
	}) != selected {
		return apierr.New("run_incomplete", "run %s records a selection without selecting its workspace", runID)
	}
	return nil
}

// scored reports whether this group's selection by run id records score
// for manifest.
func (v *validator) scored(runID, manifest string, score int64) bool {
	return v.count(schema.KindSelectionDecision, func(o envelope.Object) bool {
		var d schema.SelectionDecision
		var s speculate.Scoring
		if o.Decode(&d) != nil || d.Selector.Kind != "speculation_run" || d.Selector.ID != runID || json.Unmarshal(d.Scoring, &s) != nil {
			return false
		}
		n, ok := s.Scores[manifest]
		return ok && n == score
	}) == 1
}

func (v *validator) count(kind schema.Kind, match func(envelope.Object) bool) int {
	n := 0
	for _, o := range v.objects {
		if o.Envelope.Kind == kind && match(o) {
			n++
		}
	}
	return n
}

func (v *validator) checkAttempt(p *schema.AttemptRecord, signer string) error {
	r, err := v.run(p.RunID)
	if err != nil {
		return err
	}
	if signer != r.Signer {
		return unauthorized("attempt records of run %s are signed by its creator", p.RunID)
	}
	if p.IntentID != r.IntentID || !slices.ContainsFunc(r.Attempts, func(a schema.CandidateBinding) bool { return a.WorkspaceID == p.WorkspaceID }) {
		return apierr.New("invalid_attempt", "workspace %s is not an attempt of run %s for intent %s", p.WorkspaceID, p.RunID, p.IntentID)
	}
	ending := v.ending(p.RunID)
	if ending == "" || v.workspaceEnding(p.WorkspaceID) != "closed" {
		return apierr.New("run_active", "attempt records are written only when run %s ends and closes the workspace", p.RunID)
	}
	prior, err := attempts(v.ctx, v.q, p.RunID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(prior, func(a Attempt) bool { return a.WorkspaceID == p.WorkspaceID }) {
		return apierr.New("entity_exists", "workspace %s already has an attempt record", p.WorkspaceID)
	}
	manifests, err := workspaceManifests(v.ctx, v.q, p.WorkspaceID)
	if err != nil {
		return err
	}
	if err := speculate.CheckAttempt(*p, manifests, ending == "cancelled"); err != nil {
		return err
	}
	if p.Score != nil && !v.scored(p.RunID, *p.Manifest, *p.Score) {
		return apierr.New("invalid_attempt", "score %d of %s is not the score the run's selection records", *p.Score, *p.Manifest)
	}
	if p.Manifest == nil {
		return nil
	}
	found, err := evidenceByHash(v.ctx, v.q, p.Evidence)
	if err != nil {
		return err
	}
	for _, h := range p.Evidence {
		i := slices.IndexFunc(found, func(e selection.Evidence) bool { return e.Hash == h })
		if i < 0 {
			return apierr.New("unknown_evidence", "no committed evidence attestation %s", h).With("evidence", h)
		}
		if found[i].Att.Manifest != *p.Manifest {
			return apierr.New("cross_revision_evidence", "evidence %s was recorded for manifest %s, not %s", h, found[i].Att.Manifest, *p.Manifest)
		}
	}
	intent, err := v.intent(p.IntentID)
	if err != nil {
		return err
	}
	for _, id := range p.FailedCriteria {
		if !slices.ContainsFunc(intent.Criteria, func(c schema.Criterion) bool { return c.ID == id && c.Verification.Kind == "check" }) {
			return apierr.New("invalid_attempt", "failed criterion %q is not a check criterion of intent %s", id, p.IntentID)
		}
	}
	return nil
}

// checkRunSelection verifies the speculation-specific parts of a selection
// whose selector is a run: the run's creator signs it while the group
// resolves the run, the competitors are exactly the run's other frozen
// candidates, every check criterion is satisfied, and the winner follows the
// recorded scores and tie rule.
func (v *validator) checkRunSelection(p *schema.SelectionDecision, m schema.ChangeManifest, signer string) error {
	r, err := v.run(p.Selector.ID)
	if err != nil {
		return err
	}
	if signer != r.Signer {
		return unauthorized("only the creator of run %s may sign its selection", r.ID)
	}
	if p.IntentID != r.IntentID || !slices.ContainsFunc(r.Attempts, func(a schema.CandidateBinding) bool { return a.WorkspaceID == m.WorkspaceID }) {
		return apierr.New("invalid_selection", "manifest %s is not a candidate of run %s", p.Manifest, r.ID)
	}
	if v.ending(r.ID) != "resolved" {
		return apierr.New("run_active", "a run selection is recorded only when run %s resolves", r.ID)
	}
	frozen := []string{}
	for _, a := range r.Attempts {
		if a.WorkspaceID == m.WorkspaceID {
			continue
		}
		ms, err := workspaceManifests(v.ctx, v.q, a.WorkspaceID)
		if err != nil {
			return err
		}
		frozen = append(frozen, ms...)
	}
	slices.Sort(frozen)
	if !slices.Equal(frozen, p.Competitors) {
		return apierr.New("invalid_selection", "competitors must be exactly the other frozen candidates of run %s", r.ID)
	}
	intent, err := v.intent(p.IntentID)
	if err != nil {
		return err
	}
	for _, c := range p.Criteria {
		i := slices.IndexFunc(intent.Criteria, func(ic schema.Criterion) bool { return ic.ID == c.CriterionID })
		if i >= 0 && intent.Criteria[i].Verification.Kind == "check" && c.Status != selection.Satisfied {
			return apierr.New("invalid_selection", "check criterion %q is %s; an ineligible candidate cannot be selected", c.CriterionID, c.Status)
		}
	}
	scoring, err := speculate.CheckScoring(p.Scoring, r.Scorer, p.Manifest)
	if err != nil {
		return err
	}
	for id := range scoring.Scores {
		if id != p.Manifest && !slices.Contains(frozen, id) {
			return apierr.New("invalid_selection", "scored candidate %s is not frozen in run %s", id, r.ID)
		}
	}
	return nil
}
