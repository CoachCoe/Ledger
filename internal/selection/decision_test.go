package selection

import (
	"slices"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

var rootSelector = schema.Selector{Kind: "root", ID: "fp"}

func TestAC059_171_BuildRecomputesStatuses(t *testing.T) {
	pass, fail := mech("hb", "pass"), mech("ha", "fail")
	set := []Evidence{pass, fail, pass} // duplicate and unsorted on purpose
	d, err := Build(binding(), "int_1", set, set, rootSelector, []string{"m3", "m2"}, nil, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if d.IntentID != "int_1" || d.Manifest != manifest || d.Commit != commit || d.Selector != rootSelector {
		t.Fatalf("binding fields: %+v", d)
	}
	if !slices.Equal(d.Evidence, []string{"ha", "hb"}) || !slices.Equal(d.Competitors, []string{"m2", "m3"}) {
		t.Fatalf("evidence %v competitors %v must be sorted and unique", d.Evidence, d.Competitors)
	}
	if string(d.Scoring) != "null" {
		t.Fatalf("absent scoring is null, got %q", d.Scoring)
	}
	tests := d.Criteria[0]
	if tests.CriterionID != "tests" || tests.Status != Failed || !slices.Equal(tests.Deciding, []string{"ha"}) || !slices.Equal(tests.Conflicting, []string{"hb"}) {
		t.Fatalf("a mixed result is failed with the passing evidence as conflict: %+v", tests)
	}
	review := d.Criteria[1]
	if review.Status != Outstanding || len(review.Deciding) != 0 || len(review.Conflicting) != 0 {
		t.Fatalf("an undecided criterion stays outstanding with nothing deciding it: %+v", review)
	}
	if !slices.Equal(OutstandingIDs(d.Criteria), []string{"review"}) {
		t.Fatalf("outstanding: %v", OutstandingIDs(d.Criteria))
	}
}

func TestAC060_OutstandingNeverBecomesSatisfied(t *testing.T) {
	// No evidence at all: every criterion is outstanding, none satisfied.
	d, err := Build(binding(), "int_1", nil, nil, rootSelector, nil, nil, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Criteria {
		if c.Status != Outstanding || len(c.Deciding) != 0 {
			t.Fatalf("%+v", c)
		}
	}
	if len(d.Evidence) != 0 || len(d.ContraryEvidence) != 0 || len(d.Competitors) != 0 {
		t.Fatalf("lists must be empty, not missing: %+v", d)
	}
	// A reviewer-kind mismatch does not decide the criterion either.
	d, err = Build(binding(), "int_1", []Evidence{judged("hj", "pass", "agent")}, nil, rootSelector, nil, nil, "t")
	if err != nil {
		t.Fatal(err)
	}
	if st := d.Criteria[1]; st.Status != Outstanding {
		t.Fatalf("an agent judgment must not satisfy a human criterion: %+v", st)
	}
}

func TestAC171_BuildRejectsCrossRevisionEvidence(t *testing.T) {
	for name, edit := range map[string]func(*Evidence){
		"manifest": func(e *Evidence) { e.Att.Manifest = "other" },
		"commit":   func(e *Evidence) { e.Att.Commit = "other" },
		"tree":     func(e *Evidence) { e.Att.ExecutedTree = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			e := mech("hx", "pass")
			edit(&e)
			_, err := Build(binding(), "int_1", []Evidence{mech("hy", "pass"), e}, nil, rootSelector, nil, nil, "t")
			if !apierr.HasCode(err, "cross_revision_evidence") {
				t.Fatalf("got %v", err)
			}
			if apierr.As(err).Details["evidence"] != "hx" {
				t.Fatalf("details name the offending evidence: %v", apierr.As(err).Details)
			}
		})
	}
}

func TestAC113_BuildKeepsContraryEvidence(t *testing.T) {
	// The failure is known for the binding but was not relied upon.
	pass, fail := mech("hp", "pass"), mech("hf", "fail")
	d, err := Build(binding(), "int_1", []Evidence{pass}, []Evidence{pass, fail}, rootSelector, nil, []byte(`{"score":1}`), "t")
	if err != nil {
		t.Fatal(err)
	}
	if st := d.Criteria[0]; st.Status != Satisfied || !slices.Equal(st.Deciding, []string{"hp"}) {
		t.Fatalf("the submitted set decides the status: %+v", st)
	}
	if !slices.Equal(d.ContraryEvidence, []string{"hf"}) || !slices.Equal(d.Evidence, []string{"hp"}) {
		t.Fatalf("contrary %v evidence %v", d.ContraryEvidence, d.Evidence)
	}
	if string(d.Scoring) != `{"score":1}` {
		t.Fatalf("scoring is carried verbatim: %s", d.Scoring)
	}
}
