package verify

import (
	"encoding/json/v2"
	"slices"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/lifecycle"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/speculate"
)

// RunReport verifies one speculation run: its creation against its intent,
// its derived state, and that an ended run accounts for every attempt.
type RunReport struct {
	ID         string          `json:"id"`
	IntentID   string          `json:"intent_id"`
	State      string          `json:"state"`
	Integrity  string          `json:"integrity"`
	Selections []string        `json:"selections"`
	Attempts   []AttemptReport `json:"attempts"`
	Detail     string          `json:"detail,omitempty"`
}

// AttemptReport verifies one attempt record.
type AttemptReport struct {
	Hash        string `json:"hash"`
	WorkspaceID string `json:"workspace_id"`
	Outcome     string `json:"outcome"`
	Integrity   string `json:"integrity"`
	Detail      string `json:"detail,omitempty"`
}

// manifestsOf returns the manifests of workspace id in the corpus.
func (g *graph) manifestsOf(id string) []string {
	out := []string{}
	for _, h := range sortedKeys(g.objects) {
		o := g.objects[h]
		if o.Envelope.Kind != schema.KindChangeManifest {
			continue
		}
		if m, err := decode[schema.ChangeManifest](o); err == nil && m.WorkspaceID == id {
			out = append(out, h)
		}
	}
	return out
}

// stateOfSubject derives a subject's state from the corpus's events.
func (g *graph) stateOfSubject(kind, id, creation string) lifecycle.State {
	var events []lifecycle.Event
	for _, h := range sortedKeys(g.objects) {
		o := g.objects[h]
		if o.Envelope.Kind != schema.KindLifecycleEvent {
			continue
		}
		e, err := decode[schema.LifecycleEvent](o)
		if err == nil && e.SubjectID == id && e.Creation == creation && e.Actor == o.Envelope.Signer {
			events = append(events, lifecycle.Event{Hash: h, LifecycleEvent: e})
		}
	}
	return lifecycle.Derive(kind, events)
}

func attemptOf(r schema.SpeculationRun, workspaceID string) bool {
	return slices.ContainsFunc(r.Attempts, func(a schema.CandidateBinding) bool { return a.WorkspaceID == workspaceID })
}

// runSelection checks what a run's selection adds to an ordinary one: the
// run's creator signed it, its competitors are exactly the run's other
// frozen candidates, every check criterion is satisfied, and the winner
// follows the recorded scores and tie rule.
func (g *graph) runSelection(o envelope.Object, d schema.SelectionDecision, m schema.ChangeManifest, intent schema.Intent) error {
	robj, err := g.entity(d.Selector.ID, schema.KindSpeculationRun)
	if err != nil {
		return err
	}
	r, err := decode[schema.SpeculationRun](robj)
	if err != nil {
		return err
	}
	if o.Envelope.Signer != robj.Envelope.Signer {
		return apierr.New("unauthorized", "a run's selection is signed by the run's creator")
	}
	if d.IntentID != r.IntentID || !attemptOf(r, m.WorkspaceID) {
		return apierr.New("invalid_selection", "the selected manifest is not a candidate of run %s", r.ID)
	}
	frozen := []string{}
	for _, a := range r.Attempts {
		if a.WorkspaceID != m.WorkspaceID {
			frozen = append(frozen, g.manifestsOf(a.WorkspaceID)...)
		}
	}
	slices.Sort(frozen)
	if !slices.Equal(frozen, d.Competitors) {
		for _, h := range d.Competitors {
			if _, ok := g.objects[h]; !ok {
				return apierr.New("dependency_unavailable", "missing competitor %s", h)
			}
		}
		return apierr.New("invalid_selection", "competitors differ from the run's other frozen candidates")
	}
	for _, c := range d.Criteria {
		i := slices.IndexFunc(intent.Criteria, func(ic schema.Criterion) bool { return ic.ID == c.CriterionID })
		if i >= 0 && intent.Criteria[i].Verification.Kind == "check" && c.Status != "satisfied" {
			return apierr.New("invalid_selection", "check criterion %s is %s; the run selected an ineligible candidate", c.CriterionID, c.Status)
		}
	}
	_, err = speculate.CheckScoring(d.Scoring, r.Scorer, d.Manifest)
	return err
}

// runs reports every speculation run in the corpus.
func (g *graph) runs() []RunReport {
	out := []RunReport{}
	for _, h := range sortedKeys(g.objects) {
		if o := g.objects[h]; o.Envelope.Kind == schema.KindSpeculationRun {
			out = append(out, g.run(o))
		}
	}
	return out
}

func (g *graph) run(o envelope.Object) RunReport {
	rep := RunReport{Integrity: Verified, Selections: []string{}, Attempts: []AttemptReport{}}
	fail := func(err error) RunReport {
		rep.Integrity, rep.Detail = stateOf(err), apierr.As(err).Message
		return rep
	}
	r, err := decode[schema.SpeculationRun](o)
	if err != nil {
		return fail(err)
	}
	rep.ID, rep.IntentID = r.ID, r.IntentID
	st := g.stateOfSubject(lifecycle.SpeculationRun, r.ID, o.Hash)
	rep.State = st.State
	if err := g.signature(o); err != nil {
		return fail(err)
	}
	if strings.HasPrefix(o.Envelope.Signer, schema.PrefixSession) {
		return fail(apierr.New("unauthorized", "only a root key may start a speculation run"))
	}
	iobj, err := g.entity(r.IntentID, schema.KindIntent)
	if err != nil {
		return fail(err)
	}
	intent, err := decode[schema.Intent](iobj)
	if err != nil {
		return fail(err)
	}
	if err := speculate.ValidateRun(r, intent); err != nil {
		return fail(err)
	}
	for _, a := range r.Attempts {
		wobj, err := g.entity(a.WorkspaceID, schema.KindWorkspace)
		if err != nil {
			return fail(err)
		}
		w, err := decode[schema.Workspace](wobj)
		if err != nil {
			return fail(err)
		}
		if w.IntentID != r.IntentID || w.BaseCommit != r.BaseCommit || w.BaseTree != r.BaseTree {
			return fail(apierr.New("invalid_run", "attempt workspace %s differs from the run's intent or base", a.WorkspaceID))
		}
		if ms := g.manifestsOf(a.WorkspaceID); len(ms) > 1 {
			return fail(apierr.New("invalid_run", "attempt workspace %s has more than one manifest", a.WorkspaceID))
		}
	}
	var scoring *speculate.Scoring
	for _, h := range sortedKeys(g.objects) {
		so := g.objects[h]
		if so.Envelope.Kind != schema.KindSelectionDecision {
			continue
		}
		if d, err := decode[schema.SelectionDecision](so); err == nil && d.Selector.Kind == "speculation_run" && d.Selector.ID == r.ID {
			rep.Selections = append(rep.Selections, h)
			var s speculate.Scoring
			if json.Unmarshal(d.Scoring, &s) == nil {
				scoring = &s
			}
		}
	}
	states := []string{}
	recorded := map[string]int{}
	for _, h := range sortedKeys(g.objects) {
		ao := g.objects[h]
		if ao.Envelope.Kind != schema.KindAttemptRecord {
			continue
		}
		a, err := decode[schema.AttemptRecord](ao)
		if err != nil || a.RunID != r.ID {
			continue
		}
		ar := g.attempt(ao, a, r, o.Envelope.Signer, st.State, scoring)
		recorded[a.WorkspaceID]++
		rep.Attempts = append(rep.Attempts, ar)
		states = append(states, ar.Integrity)
	}
	switch {
	case len(rep.Selections) > 1 || (st.State == "cancelled" && len(rep.Selections) > 0):
		return fail(apierr.New("invalid_run", "run %s records more than one selection, or one while cancelled", r.ID))
	case st.State == "resolved" || st.State == "cancelled":
		for _, a := range r.Attempts {
			wobj, _ := g.entity(a.WorkspaceID, schema.KindWorkspace)
			ws := g.stateOfSubject(lifecycle.Workspace, a.WorkspaceID, wobj.Hash)
			switch {
			case recorded[a.WorkspaceID] > 1:
				return fail(apierr.New("invalid_run", "workspace %s has more than one attempt record", a.WorkspaceID))
			case ws.State == "selected" && (recorded[a.WorkspaceID] > 0 || len(rep.Selections) != 1):
				return fail(apierr.New("invalid_run", "selected workspace %s needs the run's selection and no attempt record", a.WorkspaceID))
			case ws.State != "selected" && recorded[a.WorkspaceID] == 0:
				states = append(states, Unavailable)
				rep.Detail = "missing the attempt record of " + a.WorkspaceID
			}
		}
	case len(rep.Attempts) > 0:
		states = append(states, Unavailable)
		rep.Detail = "attempt records exist but the run's ending is missing"
	}
	rep.Integrity = worst(states...)
	return rep
}

func (g *graph) attempt(o envelope.Object, a schema.AttemptRecord, r schema.SpeculationRun, runSigner, runState string, scoring *speculate.Scoring) AttemptReport {
	rep := AttemptReport{Hash: o.Hash, WorkspaceID: a.WorkspaceID, Outcome: a.Outcome, Integrity: Verified}
	fail := func(err error) AttemptReport {
		rep.Integrity, rep.Detail = stateOf(err), apierr.As(err).Message
		return rep
	}
	if err := g.signature(o); err != nil {
		return fail(err)
	}
	if o.Envelope.Signer != runSigner {
		return fail(apierr.New("unauthorized", "an attempt record is signed by its run's creator"))
	}
	if a.IntentID != r.IntentID || !attemptOf(r, a.WorkspaceID) {
		return fail(apierr.New("invalid_attempt", "workspace %s is not an attempt of run %s", a.WorkspaceID, r.ID))
	}
	if a.Manifest != nil {
		if _, ok := g.objects[*a.Manifest]; !ok {
			return fail(apierr.New("dependency_unavailable", "missing manifest %s", *a.Manifest))
		}
	}
	if err := speculate.CheckAttempt(a, g.manifestsOf(a.WorkspaceID), runState == "cancelled"); err != nil {
		return fail(err)
	}
	for _, h := range a.Evidence {
		eo, ok := g.objects[h]
		if !ok {
			return fail(apierr.New("dependency_unavailable", "missing evidence %s", h))
		}
		att, err := decode[schema.EvidenceAttestation](eo)
		if err != nil || eo.Envelope.Kind != schema.KindEvidenceAttestation || att.Manifest != *a.Manifest {
			return fail(apierr.New("cross_revision_evidence", "evidence %s is not an attestation of the attempt's candidate", h))
		}
	}
	if a.Score != nil {
		if scoring == nil {
			return fail(apierr.New("dependency_unavailable", "the run's selection, which records the scores, is missing"))
		}
		if n, ok := scoring.Scores[*a.Manifest]; !ok || n != *a.Score {
			return fail(apierr.New("invalid_attempt", "the score differs from the run's recorded scoring"))
		}
	}
	return rep
}
