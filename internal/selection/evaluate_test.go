package selection

import (
	"fmt"
	"slices"
	"testing"

	"github.com/CoachCoe/Ledger/internal/schema"
)

const (
	manifest = "m1"
	commit   = "c1"
	tree     = "t1"
	defHash  = "d1"
)

func binding() Binding {
	return Binding{Manifest: manifest, Commit: commit, Tree: tree, Criteria: []schema.Criterion{
		{ID: "tests", Verification: schema.Verification{Kind: "check", Check: "unit", DefinitionHash: defHash}},
		{ID: "review", Verification: schema.Verification{Kind: "judgment", Reviewer: "human"}},
	}}
}

func zero() *int64 { v := int64(0); return &v }

// mech is an independent, consistent mechanical attestation for "tests".
func mech(hash, result string) Evidence {
	a := schema.EvidenceAttestation{
		Manifest: manifest, Commit: commit, ExecutedTree: tree, CheckDefinition: defHash, Category: "mechanical",
		Criteria: []string{"tests"}, Result: result, Isolation: "none",
		Check: &schema.Phase{ExitStatus: zero()},
	}
	if result == "fail" {
		why := "check_failed"
		one := int64(1)
		a.Reason, a.Check = &why, &schema.Phase{ExitStatus: &one}
	}
	return Evidence{Hash: hash, Att: a, Signer: "ses_v", SignerKind: "service", Independent: true}
}

// judged is an independent judgment attestation for "review" by kind.
func judged(hash, result, kind string) Evidence {
	return Evidence{Hash: hash, Signer: "ses_j", SignerKind: kind, Independent: true, Att: schema.EvidenceAttestation{
		Manifest: manifest, Commit: commit, ExecutedTree: tree, CheckDefinition: "j", Category: "judgment",
		Criteria: []string{"review"}, Result: result, Isolation: "none",
	}}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func statusOf(r Result, id string) schema.CriterionStatus {
	for _, c := range r.Criteria {
		if c.CriterionID == id {
			return c
		}
	}
	return schema.CriterionStatus{}
}

func TestAC113_TruthTable(t *testing.T) {
	// AC-113: none = outstanding, all pass = satisfied, any fail = failed,
	// mixed = failed with the passing evidence reported as the conflict.
	for passes := range 4 {
		for fails := range 4 {
			t.Run(fmt.Sprintf("pass%d_fail%d", passes, fails), func(t *testing.T) {
				var wantPass, wantFail []string
				var ev []Evidence
				for i := range passes {
					e := mech(fmt.Sprintf("p%d", i), "pass")
					ev, wantPass = append(ev, e), append(wantPass, e.Hash)
				}
				for i := range fails {
					e := mech(fmt.Sprintf("f%d", i), "fail")
					ev, wantFail = append(ev, e), append(wantFail, e.Hash)
				}
				res := Evaluate(binding(), ev, ev)
				got := statusOf(res, "tests")
				switch {
				case passes+fails == 0:
					if got.Status != Outstanding || len(got.Deciding) != 0 || len(got.Conflicting) != 0 {
						t.Fatalf("none: %+v", got)
					}
				case fails == 0:
					if got.Status != Satisfied || !slices.Equal(got.Deciding, wantPass) || len(got.Conflicting) != 0 {
						t.Fatalf("all pass: %+v", got)
					}
				case passes == 0:
					if got.Status != Failed || !slices.Equal(got.Deciding, wantFail) || len(got.Conflicting) != 0 {
						t.Fatalf("all fail: %+v", got)
					}
				default:
					if got.Status != Failed || !slices.Equal(got.Deciding, wantFail) || !slices.Equal(got.Conflicting, wantPass) {
						t.Fatalf("mixed: %+v", got)
					}
				}
				if got.Deciding == nil || got.Conflicting == nil {
					t.Fatal("hash lists must be empty arrays, never null")
				}
				// The judgment criterion has no evidence in any row.
				if r := statusOf(res, "review"); r.Status != Outstanding {
					t.Fatalf("review: %+v", r)
				}
				if n := countStatuses(res); res.MechanicalFailed != (fails > 0) || n[Outstanding] != 1+btoi(passes+fails == 0) || n[Satisfied]+n[Failed]+n[Outstanding] != 2 {
					t.Fatalf("summary: %v mechanical failure %v", n, res.MechanicalFailed)
				}
			})
		}
	}
}

func countStatuses(r Result) map[string]int {
	n := map[string]int{}
	for _, c := range r.Criteria {
		n[c.Status]++
	}
	return n
}

func TestAC113_TruthTableJudgment(t *testing.T) {
	// The same table holds for judgment criteria, and outstanding judgment
	// does not mark the summary as mechanically failed.
	pass, fail := judged("jp", "pass", "human"), judged("jf", "fail", "human")
	for name, tc := range map[string]struct {
		set    []Evidence
		status string
	}{
		"none":  {nil, Outstanding},
		"pass":  {[]Evidence{pass}, Satisfied},
		"fail":  {[]Evidence{fail}, Failed},
		"mixed": {[]Evidence{pass, fail}, Failed},
	} {
		res := Evaluate(binding(), tc.set, tc.set)
		got := statusOf(res, "review")
		if got.Status != tc.status {
			t.Fatalf("%s: %+v", name, got)
		}
		if name == "mixed" && (!slices.Equal(got.Deciding, []string{"jf"}) || !slices.Equal(got.Conflicting, []string{"jp"})) {
			t.Fatalf("mixed detail: %+v", got)
		}
		if res.MechanicalFailed {
			t.Fatalf("%s: a judgment failure is not a mechanical failure", name)
		}
	}
}

func TestAC112_ApplicabilityMatrix(t *testing.T) {
	// AC-112: each way an attestation can fail to apply makes it
	// not applicable and is listed in rejected with a reason.
	check, review := binding().Criteria[0], binding().Criteria[1]
	one := int64(1)
	mutate := func(f func(*Evidence)) Evidence { e := mech("x", "pass"); f(&e); return e }
	cases := []struct {
		name string
		c    schema.Criterion
		e    Evidence
		why  string
	}{
		{"baseline", check, mech("x", "pass"), ""},
		{"wrong definition hash", check, mutate(func(e *Evidence) { e.Att.CheckDefinition = "other" }), ReasonDefinitionMismatch},
		{"other manifest", check, mutate(func(e *Evidence) { e.Att.Manifest = "m2" }), ReasonOtherManifest},
		{"other commit", check, mutate(func(e *Evidence) { e.Att.Commit = "c2" }), ReasonOtherCommit},
		{"other tree", check, mutate(func(e *Evidence) { e.Att.ExecutedTree = "t2" }), ReasonOtherTree},
		{"criterion not listed", check, mutate(func(e *Evidence) { e.Att.Criteria = []string{"lint"} }), ReasonCriterionNotListed},
		{"judgment category on check criterion", check, mutate(func(e *Evidence) { e.Att.Category = "judgment" }), ReasonCategoryMismatch},
		{"mechanical category on judgment criterion", review, func() Evidence {
			e := judged("x", "pass", "human")
			e.Att.Category = "mechanical"
			return e
		}(), ReasonCategoryMismatch},
		{"not independent", check, mutate(func(e *Evidence) { e.Independent, e.IndependenceReason = false, "signer_is_author" }), ReasonNotIndependent},
		{"agent where human required", review, judged("x", "pass", "agent"), ReasonReviewerKind},
		{"service never reviews", review, judged("x", "pass", "service"), ReasonReviewerKind},
		{"unknown signer kind", review, judged("x", "pass", ""), ReasonReviewerKind},
		{"pass with failing check", check, mutate(func(e *Evidence) { e.Att.Check.ExitStatus = &one }), ReasonInconsistent},
		{"pass with timed out check", check, mutate(func(e *Evidence) { e.Att.Check.TimedOut = true }), ReasonInconsistent},
		{"pass with failed setup", check, mutate(func(e *Evidence) { e.Att.Setup = &schema.Phase{ExitStatus: &one} }), ReasonInconsistent},
		{"pass with no check phase", check, mutate(func(e *Evidence) { e.Att.Check = nil }), ReasonInconsistent},
		{"pass carrying a reason", check, mutate(func(e *Evidence) { why := "x"; e.Att.Reason = &why }), ReasonInconsistent},
		{"fail without a reason", check, mutate(func(e *Evidence) { e.Att.Result, e.Att.Reason = "fail", nil }), ReasonInconsistent},
		{"judgment with process phase", review, func() Evidence {
			e := judged("x", "pass", "human")
			e.Att.Check = &schema.Phase{ExitStatus: zero()}
			return e
		}(), ReasonInconsistent},
		{"isolation other than none", check, mutate(func(e *Evidence) { e.Att.Isolation = "container" }), ReasonIsolation},
		{"agent for any is allowed", schema.Criterion{ID: "review", Verification: schema.Verification{Kind: "judgment", Reviewer: "any"}}, judged("x", "pass", "agent"), ""},
		{"human for agent is not", schema.Criterion{ID: "review", Verification: schema.Verification{Kind: "judgment", Reviewer: "agent"}}, judged("x", "pass", "human"), ReasonReviewerKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := Applicable(binding(), tc.c, tc.e)
			if ok != (tc.why == "") || why != tc.why {
				t.Fatalf("Applicable = %v, %q; want reason %q", ok, why, tc.why)
			}
			if tc.why == "" {
				return
			}
			b := binding()
			b.Criteria = []schema.Criterion{tc.c}
			e := tc.e
			if !slices.Contains(e.Att.Criteria, tc.c.ID) {
				return // the claim names another criterion, which this binding does not have
			}
			res := Evaluate(b, []Evidence{e}, nil)
			if st := res.Criteria[0]; st.Status != Outstanding || len(st.Deciding) != 0 {
				t.Fatalf("an inapplicable attestation decided a criterion: %+v", st)
			}
			if len(res.Rejected) != 1 || res.Rejected[0].Reason != tc.why || res.Rejected[0].Hash != "x" || res.Rejected[0].Criterion != tc.c.ID {
				t.Fatalf("rejected: %+v", res.Rejected)
			}
		})
	}
}

func TestAC113_ContraryEvidence(t *testing.T) {
	// A failure outside the submitted set is still returned.
	pass, fail := mech("p", "pass"), mech("f", "fail")
	res := Evaluate(binding(), []Evidence{pass}, []Evidence{pass, fail})
	if got := statusOf(res, "tests"); got.Status != Satisfied {
		t.Fatalf("the submitted set alone decides the status: %+v", got)
	}
	if !slices.Equal(res.Contrary, []string{"f"}) {
		t.Fatalf("contrary evidence: %v", res.Contrary)
	}
	// Evidence for another revision is not current and is never contrary.
	stale := mech("s", "fail")
	stale.Att.Manifest = "m0"
	res = Evaluate(binding(), []Evidence{pass}, []Evidence{pass, stale})
	if len(res.Contrary) != 0 {
		t.Fatalf("stale evidence reported as contrary: %v", res.Contrary)
	}
	// A non-independent failure is not applicable, so it is not contrary.
	own := mech("o", "fail")
	own.Independent = false
	if res := Evaluate(binding(), nil, []Evidence{own}); len(res.Contrary) != 0 {
		t.Fatalf("inapplicable failure reported as contrary: %v", res.Contrary)
	}
	// Duplicates collapse and the lists are sorted and never null.
	res = Evaluate(binding(), []Evidence{pass, pass}, []Evidence{fail, fail, mech("a", "fail")})
	if !slices.Equal(res.Contrary, []string{"a", "f"}) || !slices.Equal(statusOf(res, "tests").Deciding, []string{"p"}) {
		t.Fatalf("dedupe/sort: %+v", res)
	}
	if empty := Evaluate(binding(), nil, nil); empty.Contrary == nil || empty.Rejected == nil || empty.Criteria == nil {
		t.Fatal("empty result lists must not be nil")
	}
}
