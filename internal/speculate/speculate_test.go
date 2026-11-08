package speculate

import (
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

var a, b, c = strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)

func TestParseScores(t *testing.T) {
	ids := []string{a, b}
	good := `{"scores":{"` + a + `":-3,"` + b + `":9007199254740991}}`
	got, err := ParseScores([]byte(good), ids)
	if err != nil || got[a] != -3 || got[b] != 1<<53-1 {
		t.Fatalf("%v %v", got, err)
	}
	bad := map[string]string{
		"not json":     `nope`,
		"no scores":    `{}`,
		"extra member": `{"scores":{"` + a + `":1,"` + b + `":2},"winner":"` + a + `"}`,
		"missing":      `{"scores":{"` + a + `":1}}`,
		"unknown id":   `{"scores":{"` + a + `":1,"` + b + `":2,"` + c + `":3}}`,
		"fraction":     `{"scores":{"` + a + `":1.5,"` + b + `":2}}`,
		"exponent":     `{"scores":{"` + a + `":1e3,"` + b + `":2}}`,
		"unsafe":       `{"scores":{"` + a + `":9007199254740992,"` + b + `":2}}`,
		"string":       `{"scores":{"` + a + `":"1","` + b + `":2}}`,
		"duplicate":    `{"scores":{"` + a + `":1,"` + a + `":2,"` + b + `":2}}`,
		"array":        `{"scores":[1,2]}`,
	}
	for name, raw := range bad {
		if _, err := ParseScores([]byte(raw), ids); !apierr.HasCode(err, "invalid_scores") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWinnerTieRule(t *testing.T) {
	if w := Winner(map[string]int64{a: 1, b: 5, c: 5}); w != b {
		t.Fatalf("highest score, ties by ascending hash: %s", w)
	}
	if w := Winner(map[string]int64{c: -1, b: -1, a: -2}); w != b {
		t.Fatalf("negative tie: %s", w)
	}
	if Winner(nil) != "" {
		t.Fatal("no scores, no winner")
	}
}

func TestCheckScoring(t *testing.T) {
	raw := func(scores string) jsontext.Value {
		return jsontext.Value(`{"eligible":["` + a + `","` + b + `"],"rule":"` + TieRule + `","scorer":"` + c + `","scores":` + scores + `}`)
	}
	if _, err := CheckScoring(raw(`{"`+a+`":2,"`+b+`":2}`), c, a); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		raw    jsontext.Value
		scorer string
		winner string
	}{
		"wrong winner":  {raw(`{"` + a + `":2,"` + b + `":2}`), c, b},
		"wrong scorer":  {raw(`{"` + a + `":2,"` + b + `":1}`), a, a},
		"unscored":      {raw(`{"` + a + `":2}`), c, a},
		"unknown field": {jsontext.Value(`{"x":1}`), c, a},
	} {
		if _, err := CheckScoring(tc.raw, tc.scorer, tc.winner); !apierr.HasCode(err, "invalid_selection") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckAttempt(t *testing.T) {
	score := int64(3)
	base := schema.AttemptRecord{WorkspaceID: "wsp_x", Evidence: []string{}, FailedCriteria: []string{}}
	ok := []struct {
		rec       func(r *schema.AttemptRecord)
		manifests []string
		cancelled bool
	}{
		{func(r *schema.AttemptRecord) {
			r.Manifest, r.Outcome, r.FailedCriteria = &a, OutcomeFailed, []string{"ok"}
		}, []string{a}, false},
		{func(r *schema.AttemptRecord) { r.Manifest, r.Outcome, r.Score = &a, OutcomeTiebreaker, &score }, []string{a}, false},
		{func(r *schema.AttemptRecord) { r.Outcome = OutcomeTimeout }, nil, false},
		{func(r *schema.AttemptRecord) { r.Outcome = OutcomeNotProposed }, nil, false},
		{func(r *schema.AttemptRecord) { r.Manifest, r.Outcome = &a, OutcomeCancelled }, []string{a}, true},
		{func(r *schema.AttemptRecord) { r.Outcome = OutcomeCancelled }, nil, true},
	}
	for i, tc := range ok {
		r := base
		tc.rec(&r)
		if err := CheckAttempt(r, tc.manifests, tc.cancelled); err != nil {
			t.Errorf("valid %d: %v", i, err)
		}
	}
	bad := []struct {
		rec       func(r *schema.AttemptRecord)
		manifests []string
		cancelled bool
	}{
		{func(r *schema.AttemptRecord) { r.Outcome = OutcomeTimeout }, []string{a}, false},                    // hides the candidate
		{func(r *schema.AttemptRecord) { r.Manifest, r.Outcome = &b, OutcomeFailed }, []string{a}, false},     // wrong candidate
		{func(r *schema.AttemptRecord) { r.Manifest, r.Outcome = &a, OutcomeFailed }, []string{a}, false},     // no failed criteria
		{func(r *schema.AttemptRecord) { r.Manifest, r.Outcome = &a, OutcomeTiebreaker }, []string{a}, false}, // no score
		{func(r *schema.AttemptRecord) { r.Outcome, r.Score = OutcomeNotProposed, &score }, nil, false},
		{func(r *schema.AttemptRecord) { r.Outcome = OutcomeCancelled }, nil, false},  // run was not cancelled
		{func(r *schema.AttemptRecord) { r.Outcome = OutcomeNotProposed }, nil, true}, // run was cancelled
		{func(r *schema.AttemptRecord) { r.Manifest, r.Outcome = &a, OutcomeNotProposed }, []string{a}, false},
		{func(r *schema.AttemptRecord) { r.Outcome = OutcomeNotProposed; r.Evidence = []string{a} }, nil, false},
		{func(r *schema.AttemptRecord) { r.Manifest, r.Outcome = &a, OutcomeCancelled }, []string{a, b}, true},
	}
	for i, tc := range bad {
		r := base
		tc.rec(&r)
		if err := CheckAttempt(r, tc.manifests, tc.cancelled); !apierr.HasCode(err, "invalid_attempt") {
			t.Errorf("invalid %d: %v", i, err)
		}
	}
}

func TestValidateRun(t *testing.T) {
	timeout := int64(5)
	cmd := "score"
	def := schema.ScorerDefinition{Kind: "command", Command: &cmd, EnvAllowlist: []string{}, TimeoutSeconds: &timeout}
	h, err := ScorerHash(def)
	if err != nil {
		t.Fatal(err)
	}
	intent := schema.Intent{ID: "int_x", Criteria: []schema.Criterion{
		{ID: "t", Verification: schema.Verification{Kind: "check", Check: "unit", DefinitionHash: c}},
		{ID: "u", Verification: schema.Verification{Kind: "check", Check: "unit", DefinitionHash: c}},
		{ID: "r", Verification: schema.Verification{Kind: "judgment", Reviewer: "human"}},
	}}
	run := func() schema.SpeculationRun {
		return schema.SpeculationRun{ID: "run_x", IntentID: "int_x", Strategies: []schema.Strategy{{Name: "s", Spec: "x"}},
			Attempts: []schema.CandidateBinding{{WorkspaceID: "wsp_a", Strategy: "s"}, {WorkspaceID: "wsp_b", Strategy: "s"}},
			Checks:   []string{c}, Scorer: h, ScorerDefinition: def, Deadline: "2026-11-05T13:00:00Z", CreatedAt: "2026-11-05T12:00:00Z"}
	}
	if err := ValidateRun(run(), intent); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(r *schema.SpeculationRun){
		"checks":            func(r *schema.SpeculationRun) { r.Checks = []string{a} },
		"scorer hash":       func(r *schema.SpeculationRun) { r.Scorer = a },
		"duplicate attempt": func(r *schema.SpeculationRun) { r.Attempts[1].WorkspaceID = "wsp_a" },
		"unknown strategy":  func(r *schema.SpeculationRun) { r.Attempts[0].Strategy = "q" },
		"deadline":          func(r *schema.SpeculationRun) { r.Deadline = r.CreatedAt },
		"intent":            func(r *schema.SpeculationRun) { r.IntentID = "int_y" },
	}
	for name, edit := range cases {
		r := run()
		edit(&r)
		if err := ValidateRun(r, intent); !apierr.HasCode(err, "invalid_run") {
			t.Errorf("%s: %v", name, err)
		}
	}
	intent.Criteria = intent.Criteria[2:]
	if err := ValidateRun(run(), intent); !apierr.HasCode(err, "no_check_criterion") {
		t.Fatalf("judgment-only intent: %v", err)
	}
}
