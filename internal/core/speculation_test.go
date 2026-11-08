package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/speculate"
	"github.com/CoachCoe/Ledger/internal/verify"
)

// specFix is a three-attempt run over an intent with one pinned check
// ("ok": ok.txt exists) and one human judgment criterion.
type specFix struct {
	s        *Service
	intentID string
	start    map[string]any
	runID    string
	attempts []map[string]any
}

func okCheck() schema.CheckDefinition {
	return schema.CheckDefinition{Name: "ok", Category: "mechanical", Command: "test -f ok.txt", WorkingDirectory: ".", EnvAllowlist: []string{}, TimeoutSeconds: 10, OutputLimitBytes: 1024}
}

func specIntent(t *testing.T, s *Service) string {
	t.Helper()
	writeChecks(t, s, []schema.CheckDefinition{okCheck()})
	in := call(t, s, request("intent.create", "", map[string]any{"description": "speculate", "criteria": []any{
		map[string]any{"id": "ok", "statement": "ok.txt exists", "verification": map[string]any{"kind": "check", "check": "ok"}},
		map[string]any{"id": "review", "statement": "reads well", "verification": map[string]any{"kind": "judgment", "reviewer": "human"}},
	}}))
	return in["intent_id"].(string)
}

func submissionScorer() map[string]any { return map[string]any{"kind": "submission"} }

func startInputFor(intent string, scorer map[string]any, extra map[string]any) map[string]any {
	in := map[string]any{"intent_id": intent, "scorer": scorer, "deadline_seconds": 600, "strategies": []any{
		map[string]any{"name": "alpha", "spec": "Write ok.txt"},
		map[string]any{"name": "beta", "spec": "Write ok.txt differently"},
		map[string]any{"name": "gamma", "spec": "Forget ok.txt"},
	}}
	for k, v := range extra {
		in[k] = v
	}
	return in
}

func newSpec(t *testing.T, s *Service, scorer map[string]any, extra map[string]any) *specFix {
	t.Helper()
	f := &specFix{s: s, intentID: specIntent(t, s)}
	f.start = call(t, s, request("speculation.start", "", startInputFor(f.intentID, scorer, extra)))
	f.runID = f.start["run_id"].(string)
	for _, a := range f.start["attempts"].([]any) {
		f.attempts = append(f.attempts, a.(map[string]any))
	}
	return f
}

func (f *specFix) write(t *testing.T, i int, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.attempts[i]["path"].(string), name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *specFix) submitReq(i int) api.Request {
	a := f.attempts[i]
	return request("speculation.submit", a["session_id"].(string), map[string]any{"run_id": f.runID, "workspace_id": a["workspace_id"]})
}

func (f *specFix) submit(t *testing.T, i int) map[string]any {
	t.Helper()
	return call(t, f.s, f.submitReq(i))
}

func (f *specFix) runReq(op string) api.Request {
	return request(op, "", map[string]any{"run_id": f.runID})
}

func (f *specFix) status(t *testing.T) map[string]any {
	t.Helper()
	return call(t, f.s, f.runReq("speculation.status"))
}

// scripted submits alpha with ok.txt=score1, beta with ok.txt=score2 and
// gamma without ok.txt, so gamma fails the check.
func (f *specFix) scripted(t *testing.T, alpha, beta string) (manifests [3]string) {
	t.Helper()
	f.write(t, 0, "ok.txt", alpha)
	f.write(t, 1, "ok.txt", beta)
	f.write(t, 2, "other.txt", "no ok here")
	for i := range 3 {
		manifests[i] = f.submit(t, i)["manifest"].(string)
	}
	return manifests
}

func expectErr(t *testing.T, s *Service, r api.Request, code string) *apierr.Error {
	t.Helper()
	_, err := s.Handle(context.Background(), r)
	if !apierr.HasCode(err, code) {
		t.Fatalf("%s: want %s, got %v", r.Operation, code, err)
	}
	return apierr.As(err)
}

func readRecord[T any](t *testing.T, s *Service, hash string) T {
	t.Helper()
	o, err := s.Store.Read(hash)
	if err != nil {
		t.Fatal(err)
	}
	var p T
	if err := o.Decode(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func attemptsByWorkspace(t *testing.T, out map[string]any) map[string]map[string]any {
	t.Helper()
	m := map[string]map[string]any{}
	for _, a := range out["attempts"].([]any) {
		row := a.(map[string]any)
		m[row["workspace_id"].(string)] = row
	}
	return m
}

func TestAC196_ThreeScriptedAttemptsSelectOne(t *testing.T) {
	// AC-005, AC-195, AC-196, AC-208: one candidate fails the check, one
	// loses on the scorer, one is selected; the winner is an ordinary branch
	// and nothing is merged.
	s := fixture(t)
	f := newSpec(t, s, submissionScorer(), nil)
	ctx := context.Background()
	head, err := s.Store.Repo.ResolveRef(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	m := f.scripted(t, "5", "9")
	pending := call(t, s, f.runReq("speculation.evaluate"))
	if pending["awaiting_scores"] != true || len(pending["eligible"].([]any)) != 2 {
		t.Fatalf("evaluate should await scores for the two eligible candidates: %v", pending)
	}
	cands := pending["candidates"].(map[string]any)["candidates"].([]any)
	if len(cands) != 2 || slices.ContainsFunc(cands, func(c any) bool { return c.(map[string]any)["id"] == m[2] }) {
		t.Fatalf("the ineligible candidate must not be scored: %v", cands)
	}
	out := call(t, s, request("speculation.score", "", map[string]any{"run_id": f.runID, "scores": map[string]any{m[0]: 5, m[1]: 9}}))
	if out["state"] != "resolved" || out["accepted"] != false || out["merged"] != false {
		t.Fatalf("outcome: %v", out)
	}
	winner := out["winner"].(map[string]any)
	if winner["manifest"] != m[1] {
		t.Fatalf("beta scored highest: %v", winner)
	}
	rows := attemptsByWorkspace(t, out)
	alpha, beta, gamma := rows[f.attempts[0]["workspace_id"].(string)], rows[f.attempts[1]["workspace_id"].(string)], rows[f.attempts[2]["workspace_id"].(string)]
	if alpha["outcome"] != speculate.OutcomeTiebreaker || alpha["score"] != float64(5) || beta["outcome"] != "selected" ||
		gamma["outcome"] != speculate.OutcomeFailed || !slices.Equal(gamma["failed_criteria"].([]any), []any{"ok"}) {
		t.Fatalf("attempt outcomes: %v", rows)
	}

	// AC-061: one signed attempt record per loser, with its reason.
	lost := readRecord[schema.AttemptRecord](t, s, alpha["attempt_record"].(string))
	if lost.Manifest == nil || *lost.Manifest != m[0] || lost.Score == nil || *lost.Score != 5 || lost.Approach == nil || len(lost.Evidence) == 0 || lost.RunID != f.runID {
		t.Fatalf("lost attempt record: %+v", lost)
	}
	failed := readRecord[schema.AttemptRecord](t, s, gamma["attempt_record"].(string))
	if failed.Score != nil || !slices.Equal(failed.FailedCriteria, []string{"ok"}) {
		t.Fatalf("failed attempt record: %+v", failed)
	}

	// AC-060, AC-193, AC-232: the selection lists the competing manifests and
	// scoring inputs; the judgment criterion stays outstanding.
	d := readRecord[schema.SelectionDecision](t, s, out["selection"].(string))
	if d.Selector.Kind != "speculation_run" || d.Selector.ID != f.runID || !slices.Equal(d.Competitors, sortedOf(m[0], m[2])) {
		t.Fatalf("selection: %+v", d)
	}
	for _, c := range d.Criteria {
		if (c.CriterionID == "ok" && c.Status != "satisfied") || (c.CriterionID == "review" && c.Status != "outstanding") {
			t.Fatalf("criteria: %+v", d.Criteria)
		}
	}
	var scoring speculate.Scoring
	if err := json.Unmarshal(d.Scoring, &scoring); err != nil || scoring.Scores[m[0]] != 5 || scoring.Scores[m[1]] != 9 || scoring.Rule != speculate.TieRule {
		t.Fatalf("scoring: %s %v", d.Scoring, err)
	}

	// The winner is selected; the losers are closed and their worktrees gone.
	st := f.status(t)
	if st["state"] != "resolved" || st["selection"] != out["selection"] {
		t.Fatalf("status: %v", st)
	}
	for i, want := range []string{"closed", "selected", "closed"} {
		row := attemptsByWorkspace(t, st)[f.attempts[i]["workspace_id"].(string)]
		if row["workspace_state"] != want {
			t.Fatalf("attempt %d is %v, want %s", i, row["workspace_state"], want)
		}
		_, err := os.Stat(f.attempts[i]["path"].(string))
		if exists := err == nil; exists != (want == "selected") {
			t.Fatalf("attempt %d worktree exists=%v", i, exists)
		}
		sub, err := s.Store.Index.Subject(ctx, f.attempts[i]["session_id"].(string))
		if err != nil || sub.State != "ended" {
			t.Fatalf("attempt session %d: %+v %v", i, sub, err)
		}
	}

	// AC-247: the selected branch is an ordinary Git branch; HEAD did not move.
	tip, err := s.Store.Repo.ResolveRef(ctx, "refs/heads/"+winner["branch"].(string))
	if err != nil || tip != winner["commit"] {
		t.Fatalf("winner branch %v -> %s (%v)", winner["branch"], tip, err)
	}
	if now, _ := s.Store.Repo.ResolveRef(ctx, "HEAD"); now != head {
		t.Fatal("speculation moved HEAD")
	}

	// A resolved run accepts nothing further.
	expectErr(t, s, f.runReq("speculation.cancel"), "invalid_state")
	expectErr(t, s, f.runReq("speculation.evaluate"), "invalid_state")
}

func sortedOf(xs ...string) []string {
	out := slices.Clone(xs)
	slices.Sort(out)
	return out
}

func TestAC188_StartHandsOffScopedWorkspacesAndRetriesToOneRun(t *testing.T) {
	// AC-118, AC-188, AC-190: a retried start returns the same run; every
	// attempt has its own session, capability and worktree; nothing waits
	// for or launches an agent.
	s := fixture(t)
	intent := specIntent(t, s)
	r := request("speculation.start", "", startInputFor(intent, submissionScorer(), map[string]any{"n": 4, "allow": []any{"ok.txt", "other.txt"}}))
	first := call(t, s, r)
	again, err := s.Handle(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !again.(*record.Result).Replayed || !strings.Contains(string(again.(*record.Result).Result), first["run_id"].(string)) {
		t.Fatal("a retried start must replay the same run")
	}
	attempts := first["attempts"].([]any)
	if len(attempts) != 4 {
		t.Fatalf("n=4 gives four attempts: %d", len(attempts))
	}
	seen := map[string]bool{}
	for i, a := range attempts {
		row := a.(map[string]any)
		for _, k := range []string{"workspace_id", "session_id", "capability_id", "path"} {
			if seen[row[k].(string)] {
				t.Fatalf("attempt %d shares %s", i, k)
			}
			seen[row[k].(string)] = true
		}
		if want := []string{"alpha", "beta", "gamma", "alpha"}[i]; row["strategy"] != want {
			t.Fatalf("attempt %d strategy %v, want %s", i, row["strategy"], want)
		}
		if _, err := os.Stat(row["path"].(string)); err != nil {
			t.Fatalf("attempt %d has no worktree: %v", i, err)
		}
		c := created[schema.Capability](t, s, row["capability_id"].(string), schema.KindCapability)
		deadline, _ := schema.ParseTimestamp(first["deadline"].(string))
		if !slices.Equal(c.Allow, []string{"ok.txt", "other.txt"}) || c.ExpiresAt != schema.Timestamp(deadline.Add(attemptGrace)) {
			t.Fatalf("attempt capability: %+v", c)
		}
	}
	run := created[schema.SpeculationRun](t, s, first["run_id"].(string), schema.KindSpeculationRun)
	if len(run.Attempts) != 4 || run.Scorer != first["scorer"] || len(run.Checks) != 1 {
		t.Fatalf("run creation: %+v", run)
	}
	st := call(t, s, request("speculation.status", "", map[string]any{"run_id": first["run_id"]}))
	if st["state"] != "collecting" {
		t.Fatalf("a started run collects: %v", st)
	}
	// An attempt session cannot start or cancel runs.
	sid := attempts[0].(map[string]any)["session_id"].(string)
	expectErr(t, s, request("speculation.start", sid, startInputFor(intent, submissionScorer(), nil)), "unauthorized")
	expectErr(t, s, request("speculation.cancel", sid, map[string]any{"run_id": first["run_id"]}), "unauthorized")
	// The scope binds: a write outside allow is refused at submission.
	f := &specFix{s: s, runID: first["run_id"].(string)}
	for _, a := range attempts {
		f.attempts = append(f.attempts, a.(map[string]any))
	}
	f.write(t, 0, "a.ts", "export const x=9;\n")
	expectErr(t, s, f.submitReq(0), "scope_violation")
}

func created[T any](t *testing.T, s *Service, id string, kind schema.Kind) T {
	t.Helper()
	p, err := creation[T](context.Background(), s.Store.Index, id, kind)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAC189_SpeculateRequiresACheckCriterion(t *testing.T) {
	// AC-045, AC-189: an intent without a check criterion is refused with a
	// structured error and nothing is created.
	s := fixture(t)
	in := call(t, s, request("intent.create", "", map[string]any{"description": "judgment only", "criteria": []any{
		map[string]any{"id": "review", "statement": "reads well", "verification": map[string]any{"kind": "judgment", "reviewer": "human"}},
	}}))
	e := expectErr(t, s, request("speculation.start", "", startInputFor(in["intent_id"].(string), submissionScorer(), nil)), "no_check_criterion")
	if e.Details["intent_id"] != in["intent_id"] {
		t.Fatalf("details: %v", e.Details)
	}
	runs, err := s.Store.Index.SubjectsIn(context.Background(), "speculation_run", "collecting")
	if err != nil || len(runs) != 0 {
		t.Fatalf("no run may exist: %v %v", runs, err)
	}
	// Inputs: a scorer without a command, no deadline, or fewer attempts
	// than strategies are refused.
	intent := specIntent(t, s)
	expectErr(t, s, request("speculation.start", "", startInputFor(intent, map[string]any{"kind": "command"}, nil)), "invalid_scorer")
	bad := startInputFor(intent, submissionScorer(), nil)
	delete(bad, "deadline_seconds")
	expectErr(t, s, request("speculation.start", "", bad), "schema_invalid")
	both := startInputFor(intent, submissionScorer(), map[string]any{"deadline": "2099-01-01T00:00:00Z"})
	expectErr(t, s, request("speculation.start", "", both), "schema_invalid")
	expectErr(t, s, request("speculation.start", "", startInputFor(intent, submissionScorer(), map[string]any{"n": 2})), "invalid_input")
}

func TestAC119_SubmissionFreezesTheCandidate(t *testing.T) {
	// AC-119: a submitted workspace cannot be re-proposed, reworked, closed
	// or selected by hand while its run is active; disk edits after
	// submission are outside the frozen candidate.
	s := fixture(t)
	f := newSpec(t, s, submissionScorer(), nil)
	f.write(t, 0, "ok.txt", "1")
	first := f.submit(t, 0)
	sid, wid := f.attempts[0]["session_id"].(string), f.attempts[0]["workspace_id"].(string)
	f.write(t, 0, "ok.txt", "2")
	expectErr(t, s, f.submitReq(0), "already_submitted")
	expectErr(t, s, request("workspace.propose", sid, map[string]any{"workspace_id": wid}), "already_submitted")
	expectErr(t, s, request("workspace.rework", sid, map[string]any{"workspace_id": wid}), "run_active")
	expectErr(t, s, request("workspace.close", sid, map[string]any{"workspace_id": wid, "reason": "done"}), "run_active")
	expectErr(t, s, request("workspace.close", "", map[string]any{"workspace_id": wid, "reason": "done"}), "run_active")
	expectErr(t, s, request("selection.record", "", map[string]any{"manifest": first["manifest"], "commit": first["commit"], "evidence": []any{}}), "run_active")
	// Another attempt's session cannot submit this workspace, and a
	// workspace of another run is refused.
	expectErr(t, s, request("speculation.submit", f.attempts[1]["session_id"].(string), map[string]any{"run_id": f.runID, "workspace_id": wid}), "unauthorized")
	expectErr(t, s, request("speculation.submit", sid, map[string]any{"run_id": "run_" + strings.Repeat("a", 26), "workspace_id": wid}), "not_an_attempt")

	// Evaluation closes collection: later submissions are refused.
	call(t, s, f.runReq("speculation.evaluate"))
	f.write(t, 1, "ok.txt", "3")
	expectErr(t, s, f.submitReq(1), "run_not_collecting")
	st := f.status(t)
	row := attemptsByWorkspace(t, st)[wid]
	if row["manifest"] != first["manifest"] || row["submitted"] != true {
		t.Fatalf("the frozen candidate changed: %v", row)
	}
}

func TestAC119_DeadlineTimesOutUnsubmittedAttempts(t *testing.T) {
	// AC-119: after the deadline nothing is accepted and attempts that never
	// submitted close with outcome timeout; closing early is not_proposed.
	s := fixture(t)
	f := newSpec(t, s, submissionScorer(), map[string]any{"deadline_seconds": 3})
	f.write(t, 0, "ok.txt", "1")
	m := f.submit(t, 0)["manifest"].(string)
	deadline, err := schema.ParseTimestamp(f.start["deadline"].(string))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(deadline) + 50*time.Millisecond)
	f.write(t, 1, "ok.txt", "1")
	expectErr(t, s, f.submitReq(1), "deadline_passed")
	if st := f.status(t); st["deadline_passed"] != true {
		t.Fatalf("status: %v", st)
	}
	call(t, s, f.runReq("speculation.evaluate"))
	// Beta's late edit was never submitted: its worktree is preserved and
	// the committed resolution reports the pending cleanup.
	e := expectErr(t, s, request("speculation.score", "", map[string]any{"run_id": f.runID, "scores": map[string]any{m: 1}}), "cleanup_pending")
	if e.Details["committed"] != true {
		t.Fatalf("details: %v", e.Details)
	}
	if _, err := os.Stat(filepath.Join(f.attempts[1]["path"].(string), "ok.txt")); err != nil {
		t.Fatalf("an unsubmitted edit was deleted: %v", err)
	}
	rows := attemptsByWorkspace(t, f.status(t))
	for i, want := range []string{"selected", speculate.OutcomeTimeout, speculate.OutcomeTimeout} {
		if got := rows[f.attempts[i]["workspace_id"].(string)]["outcome"]; got != want {
			t.Fatalf("attempt %d: %v, want %s", i, got, want)
		}
	}

	g := newSpec(t, fixture(t), submissionScorer(), nil)
	g.write(t, 0, "other.txt", "x")
	g.submit(t, 0)
	out := call(t, g.s, g.runReq("speculation.evaluate"))
	rows = attemptsByWorkspace(t, out)
	for i, want := range []string{speculate.OutcomeFailed, speculate.OutcomeNotProposed, speculate.OutcomeNotProposed} {
		if got := rows[g.attempts[i]["workspace_id"].(string)]["outcome"]; got != want {
			t.Fatalf("early close attempt %d: %v, want %s", i, got, want)
		}
	}
}

func TestAC194_AllFailSelectsNothing(t *testing.T) {
	// AC-194, AC-196: no eligible candidate resolves the run with no
	// selection; every attempt is closed with its failing criteria; the
	// scorer never runs.
	s := fixture(t)
	f := newSpec(t, s, map[string]any{"kind": "command", "command": "exit 7", "timeout_seconds": 5}, nil)
	for i := range 3 {
		f.write(t, i, "other.txt", "x")
		f.submit(t, i)
	}
	out := call(t, s, f.runReq("speculation.evaluate"))
	if out["state"] != "resolved" || out["selection"] != nil || out["winner"] != nil {
		t.Fatalf("all-fail outcome: %v", out)
	}
	for _, row := range attemptsByWorkspace(t, out) {
		if row["outcome"] != speculate.OutcomeFailed || !slices.Equal(row["failed_criteria"].([]any), []any{"ok"}) {
			t.Fatalf("attempt: %v", row)
		}
	}
	if sel, _ := s.Store.Index.RunSelections(context.Background(), f.runID); len(sel) != 0 {
		t.Fatalf("all-fail recorded a selection: %v", sel)
	}
}

func TestAC119_CancellationSelectsNothing(t *testing.T) {
	// AC-119: cancellation from collecting, evaluating or interrupted closes
	// every attempt as cancelled and selects nothing.
	for _, stage := range []string{"collecting", "evaluating", "interrupted"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			s := fixture(t)
			f := newSpec(t, s, submissionScorer(), nil)
			m := f.scripted(t, "1", "2")
			if stage != "collecting" {
				call(t, s, f.runReq("speculation.evaluate"))
			}
			if stage == "interrupted" {
				expectErr(t, s, request("speculation.score", "", map[string]any{"run_id": f.runID, "scores": map[string]any{m[0]: 1}}), "invalid_scores")
				if st := f.status(t); st["state"] != "interrupted" {
					t.Fatalf("status: %v", st)
				}
			}
			out := call(t, s, request("speculation.cancel", "", map[string]any{"run_id": f.runID, "reason": "operator"}))
			if out["state"] != "cancelled" || out["selection"] != nil {
				t.Fatalf("cancel: %v", out)
			}
			for i, a := range f.attempts {
				row := attemptsByWorkspace(t, out)[a["workspace_id"].(string)]
				rec := readRecord[schema.AttemptRecord](t, s, row["attempt_record"].(string))
				if rec.Outcome != speculate.OutcomeCancelled || rec.Manifest == nil || *rec.Manifest != m[i] {
					t.Fatalf("attempt %d: %+v", i, rec)
				}
				if stage != "collecting" && len(rec.Evidence) == 0 {
					t.Fatalf("attempt %d lost its committed evidence", i)
				}
			}
			expectErr(t, s, f.runReq("speculation.cancel"), "invalid_state")
		})
	}
}

func TestAC119_ScorerFailuresInterruptWithoutAWinner(t *testing.T) {
	// AC-119, M5.3: malformed, missing, extra, fractional and out-of-range
	// scores, a failing command and a timeout all interrupt the run with no
	// winner; resuming retries the same frozen candidates and evidence.
	cases := map[string]string{
		"exit":        `exit 3`,
		"timeout":     `sleep 30`,
		"no output":   `true`,
		"malformed":   `printf 'not json' > "$LEDGER_SCORER_OUTPUT"`,
		"missing":     `printf '{"scores":{}}' > "$LEDGER_SCORER_OUTPUT"`,
		"extra":       `printf '{"scores":{"x":1},"more":true}' > "$LEDGER_SCORER_OUTPUT"`,
		"fractional":  `ids=$(grep -o '"id":"[0-9a-f]*"' "$LEDGER_SCORER_INPUT" | cut -d'"' -f4); printf '{"scores":{' > "$LEDGER_SCORER_OUTPUT"; sep=; for id in $ids; do printf '%s"%s":1.5' "$sep" "$id" >> "$LEDGER_SCORER_OUTPUT"; sep=,; done; printf '}}' >> "$LEDGER_SCORER_OUTPUT"`,
		"unsafe":      `ids=$(grep -o '"id":"[0-9a-f]*"' "$LEDGER_SCORER_INPUT" | cut -d'"' -f4); printf '{"scores":{' > "$LEDGER_SCORER_OUTPUT"; sep=; for id in $ids; do printf '%s"%s":9007199254740992' "$sep" "$id" >> "$LEDGER_SCORER_OUTPUT"; sep=,; done; printf '}}' >> "$LEDGER_SCORER_OUTPUT"`,
		"unknown key": `printf '{"scores":{"0000000000000000000000000000000000000000000000000000000000000000":1}}' > "$LEDGER_SCORER_OUTPUT"`,
	}
	for name, command := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := fixture(t)
			f := newSpec(t, s, map[string]any{"kind": "command", "command": command, "timeout_seconds": 1}, map[string]any{"strategies": []any{map[string]any{"name": "solo", "spec": "Write ok.txt"}}})
			f.write(t, 0, "ok.txt", "1")
			f.submit(t, 0)
			_, err := s.Handle(context.Background(), f.runReq("speculation.evaluate"))
			e := apierr.As(err)
			if err == nil || (e.Code != "scorer_failed" && e.Code != "scorer_timeout" && e.Code != "invalid_scores") || e.Details["interrupted"] != true {
				t.Fatalf("want an interrupting scorer error, got %v", err)
			}
			st := f.status(t)
			if st["state"] != "interrupted" || st["resume"] != "evaluating" || st["selection"] != nil {
				t.Fatalf("status: %v", st)
			}
			for _, row := range st["attempts"].([]any) {
				if row.(map[string]any)["outcome"] != nil {
					t.Fatalf("an interrupted run closes no attempt: %v", row)
				}
			}
			expectErr(t, s, f.runReq("speculation.evaluate"), "run_interrupted")
		})
	}
}

func TestAC119_ResumeRetriesWithTheSameFrozenInputs(t *testing.T) {
	// AC-119, AC-120: after a scorer failure, resume and evaluate again use
	// the same candidates and the same attestations; ties resolve by
	// ascending manifest hash.
	s := fixture(t)
	marker := filepath.Join(t.TempDir(), "fail-once")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Equal scores for every candidate: the lowest manifest hash wins.
	command := `if [ -e "$MARKER" ]; then rm "$MARKER"; exit 1; fi; ids=$(grep -o '"id":"[0-9a-f]*"' "$LEDGER_SCORER_INPUT" | cut -d'"' -f4); printf '{"scores":{' > "$LEDGER_SCORER_OUTPUT"; sep=; for id in $ids; do printf '%s"%s":4' "$sep" "$id" >> "$LEDGER_SCORER_OUTPUT"; sep=,; done; printf '}}' >> "$LEDGER_SCORER_OUTPUT"`
	t.Setenv("MARKER", marker)
	t.Setenv("PATH", os.Getenv("PATH"))
	f := newSpec(t, s, map[string]any{"kind": "command", "command": command, "timeout_seconds": 10, "env_allowlist": []any{"MARKER", "PATH"}}, nil)
	m := f.scripted(t, "1", "1")
	expectErr(t, s, f.runReq("speculation.evaluate"), "scorer_failed")
	before := runAttestations(t, s, m[:]...)
	resumed := call(t, s, f.runReq("speculation.resume"))
	if resumed["state"] != "evaluating" {
		t.Fatalf("resume: %v", resumed)
	}
	expectErr(t, s, f.runReq("speculation.resume"), "invalid_state")
	out := call(t, s, f.runReq("speculation.evaluate"))
	if after := runAttestations(t, s, m[:]...); !slices.Equal(before, after) {
		t.Fatalf("resume re-ran evidence:\n%v\n%v", before, after)
	}
	want := min(m[0], m[1])
	if out["winner"].(map[string]any)["manifest"] != want {
		t.Fatalf("tie must go to the lower manifest hash %s: %v", want, out["winner"])
	}
}

func runAttestations(t *testing.T, s *Service, manifests ...string) []string {
	t.Helper()
	var out []string
	for _, m := range manifests {
		hs, err := s.Store.Index.AttestationsFor(context.Background(), m)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, hs...)
	}
	slices.Sort(out)
	return out
}

func TestAC120_RestartDuringEvaluationInterruptsAndResumes(t *testing.T) {
	// AC-120: a restart while evaluating interrupts the run (keys are gone);
	// resuming evaluates the same frozen candidates under new verifier
	// sessions and reuses committed evidence.
	s := fixture(t)
	f := newSpec(t, s, submissionScorer(), nil)
	m := f.scripted(t, "3", "8")
	call(t, s, f.runReq("speculation.evaluate"))
	before := runAttestations(t, s, m[:]...)
	restarted, err := New(context.Background(), s.Store, s.Keys)
	if err != nil {
		t.Fatal(err)
	}
	f.s = restarted
	st := f.status(t)
	if st["state"] != "interrupted" || st["reason"] != "daemon_restart" || st["resume"] != "evaluating" {
		t.Fatalf("status after restart: %v", st)
	}
	expectErr(t, restarted, request("speculation.score", "", map[string]any{"run_id": f.runID, "scores": map[string]any{m[0]: 1, m[1]: 2}}), "invalid_state")
	call(t, restarted, f.runReq("speculation.resume"))
	pending := call(t, restarted, f.runReq("speculation.evaluate"))
	if pending["awaiting_scores"] != true {
		t.Fatalf("evaluate: %v", pending)
	}
	if after := runAttestations(t, restarted, m[:]...); !slices.Equal(before, after) {
		t.Fatal("restart re-ran committed evidence")
	}
	out := call(t, restarted, request("speculation.score", "", map[string]any{"run_id": f.runID, "scores": map[string]any{m[0]: 3, m[1]: 8}}))
	if out["winner"].(map[string]any)["manifest"] != m[1] {
		t.Fatalf("winner: %v", out["winner"])
	}
}

func TestAC120_RestartDuringCollectionRecoversAttempts(t *testing.T) {
	// AC-120: attempt keys do not survive a restart. Root-authorized
	// recovery rebinds the workspace to a replacement session that can
	// submit; the run keeps collecting.
	s := fixture(t)
	f := newSpec(t, s, submissionScorer(), nil)
	restarted, err := New(context.Background(), s.Store, s.Keys)
	if err != nil {
		t.Fatal(err)
	}
	f.s = restarted
	if st := f.status(t); st["state"] != "collecting" {
		t.Fatalf("a collecting run survives restart: %v", st)
	}
	f.write(t, 0, "ok.txt", "1")
	expectErr(t, restarted, f.submitReq(0), "session_key_unavailable")
	rec := call(t, restarted, request("session.recover", "", map[string]any{"workspace_id": f.attempts[0]["workspace_id"]}))
	f.attempts[0]["session_id"] = rec["session_id"]
	sub := f.submit(t, 0)
	if sub["run_id"] != f.runID {
		t.Fatalf("submit: %v", sub)
	}
	out := call(t, restarted, f.runReq("speculation.evaluate"))
	if out["awaiting_scores"] != true {
		t.Fatalf("evaluate: %v", out)
	}
	res := call(t, restarted, request("speculation.score", "", map[string]any{"run_id": f.runID, "scores": map[string]any{sub["manifest"].(string): 0}}))
	if res["winner"].(map[string]any)["manifest"] != sub["manifest"] {
		t.Fatalf("winner: %v", res)
	}
}

func TestAC120_ResolutionPublicationIsOneIdempotentGroup(t *testing.T) {
	// AC-120: a crash at every record store boundary of the resolution leaves
	// either no terminal outcome (resume and resolve again) or exactly one,
	// with one outcome per attempt and no duplicate winner.
	stages := []string{record.StageJournal, record.StageEffects, record.StageObjects, record.StageReceipt, record.StageRef, record.StageRefCAS, record.StageIndex, record.StageCleanup, record.StageDone}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			var armed atomic.Bool
			s := fixtureWith(t, record.Options{Fault: func(st string) error {
				if st == stage && armed.CompareAndSwap(true, false) {
					return errCrash
				}
				return nil
			}})
			f := newSpec(t, s, submissionScorer(), nil)
			m := f.scripted(t, "2", "6")
			call(t, s, f.runReq("speculation.evaluate"))
			ctx := context.Background()
			req := request("speculation.score", "", map[string]any{"run_id": f.runID, "scores": map[string]any{m[0]: 2, m[1]: 6}})
			armed.Store(true)
			if _, err := s.Handle(ctx, req); err == nil {
				t.Fatal("expected the injected crash")
			}
			restarted, err := New(ctx, s.Store, s.Keys)
			if err != nil {
				t.Fatal(err)
			}
			f.s = restarted
			if st := f.status(t); st["state"] == "interrupted" {
				call(t, restarted, f.runReq("speculation.resume"))
				call(t, restarted, f.runReq("speculation.evaluate"))
			}
			if _, err := restarted.Handle(ctx, req); err != nil && !apierr.HasCode(err, "cleanup_pending") {
				t.Fatalf("retry: %v", err)
			}
			st := f.status(t)
			if st["state"] != "resolved" {
				t.Fatalf("status: %v", st)
			}
			sels, err := restarted.Store.Index.RunSelections(ctx, f.runID)
			if err != nil || len(sels) != 1 {
				t.Fatalf("exactly one selection: %v %v", sels, err)
			}
			records, err := restarted.Store.Index.Attempts(ctx, f.runID)
			if err != nil || len(records) != 2 {
				t.Fatalf("exactly two attempt records: %v %v", records, err)
			}
			if _, err := restarted.Store.Reindex(ctx); err != nil {
				t.Fatal(err)
			}
			if again, _ := restarted.Store.Index.RunSelections(ctx, f.runID); !slices.Equal(again, sels) {
				t.Fatalf("reindex changed the selection: %v", again)
			}
			for i := range 3 {
				if i == 1 {
					continue
				}
				if _, err := os.Stat(f.attempts[i]["path"].(string)); err == nil {
					t.Fatalf("losing worktree %d survived recovery", i)
				}
			}
		})
	}
}

func TestAC190_CommandScorerAndEvidenceIndependence(t *testing.T) {
	// AC-190, AC-192: Ledger runs the pinned check against each frozen
	// candidate under verifier sessions that are not the attempt, then
	// scores eligible candidates with the pinned command scorer.
	s := fixture(t)
	t.Setenv("PATH", os.Getenv("PATH"))
	command := `printf '{"scores":{' > "$LEDGER_SCORER_OUTPUT"; sep=; for id in $(grep -o '"id":"[0-9a-f]*"' "$LEDGER_SCORER_INPUT" | cut -d'"' -f4); do n=$(git -C "$LEDGER_REPOSITORY" show "refs/ledger/proposals/$id:ok.txt"); printf '%s"%s":%s' "$sep" "$id" "$n" >> "$LEDGER_SCORER_OUTPUT"; sep=,; done; printf '}}' >> "$LEDGER_SCORER_OUTPUT"`
	f := newSpec(t, s, map[string]any{"kind": "command", "command": command, "timeout_seconds": 30, "env_allowlist": []any{"PATH"}}, nil)
	m := f.scripted(t, "12", "7")
	out := call(t, s, f.runReq("speculation.evaluate"))
	if out["winner"].(map[string]any)["manifest"] != m[0] {
		t.Fatalf("alpha scored 12: %v", out)
	}
	ctx := context.Background()
	for i, h := range m {
		known, err := s.Store.Index.EvidenceFor(ctx, h)
		if err != nil || len(known) != 1 {
			t.Fatalf("candidate %d evidence: %v %v", i, known, err)
		}
		if !known[0].Independent || known[0].Signer == f.attempts[i]["session_id"] {
			t.Fatalf("candidate %d evidence is not independent: %+v", i, known[0])
		}
	}
	// A retried evaluate replays the committed resolution.
	r := f.runReq("speculation.evaluate")
	r.OperationID = "op_" + strings.Repeat("e", 26)
	expectErr(t, s, r, "invalid_state")
	raw, _ := canon.Marshal(out)
	if !strings.Contains(string(raw), m[0]) {
		t.Fatal("outcome must name the winner")
	}
}

func TestAC120_PublicationRejectsForgedRunGroups(t *testing.T) {
	// AC-118, AC-119, AC-120: whatever a caller assembles, the index accepts
	// a run's terminal group only when it accounts for every attempt once,
	// selects the scoring winner, and records the scores it used.
	s := fixture(t)
	f := newSpec(t, s, submissionScorer(), nil)
	m := f.scripted(t, "4", "9")
	call(t, s, f.runReq("speculation.evaluate"))
	ctx := context.Background()
	root, err := s.root()
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.loadRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	cands, err := s.evaluateCandidates(ctx, rs, false)
	if err != nil {
		t.Fatal(err)
	}
	scores := map[string]int64{m[0]: 4, m[1]: 9}
	valid, err := s.endRun(ctx, rs, root, "resolved", "evaluated", cands, scores)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(records []envelope.Object) error {
		_, err := s.Store.Do(ctx, record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "test.forge", Digest: canon.Hash([]byte(schema.NewID("x_"))), Signer: root}, func(*index.Index) (any, []envelope.Object, error) {
			return map[string]any{}, records, nil
		})
		return err
	}
	reseal := func(o envelope.Object, edit func(any)) envelope.Object {
		t.Helper()
		p, err := o.Payload()
		if err != nil {
			t.Fatal(err)
		}
		edit(p)
		out, err := envelope.Seal(o.Envelope.Kind, p, root)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	without := func(kind schema.Kind, n int) []envelope.Object {
		out := []envelope.Object{}
		for _, o := range valid.records {
			if o.Envelope.Kind == kind {
				if n == 0 {
					n--
					continue
				}
				n--
			}
			out = append(out, o)
		}
		return out
	}
	replace := func(kind schema.Kind, edit func(any)) []envelope.Object {
		out := []envelope.Object{}
		for _, o := range valid.records {
			if o.Envelope.Kind == kind {
				o = reseal(o, edit)
			}
			out = append(out, o)
		}
		return out
	}
	cases := map[string]struct {
		records []envelope.Object
		code    string
	}{
		"run event alone":   {valid.records[:1], "run_incomplete"},
		"attempt alone":     {[]envelope.Object{find(t, valid.records, schema.KindAttemptRecord)}, "run_active"},
		"missing attempt":   {without(schema.KindAttemptRecord, 0), "run_incomplete"},
		"missing selection": {without(schema.KindSelectionDecision, 0), "run_incomplete"},
		"winner against scores": {replace(schema.KindSelectionDecision, func(p any) {
			d := p.(*schema.SelectionDecision)
			d.Scoring = jsontext.Value(`{"eligible":["` + sortedOf(m[0], m[1])[0] + `","` + sortedOf(m[0], m[1])[1] + `"],"rule":"` + speculate.TieRule + `","scorer":"` + rs.run.Scorer + `","scores":{"` + m[0] + `":4,"` + m[1] + `":3}}`)
		}), "invalid_selection"},
		"forged score": {replace(schema.KindAttemptRecord, func(p any) {
			if a := p.(*schema.AttemptRecord); a.Score != nil {
				n := int64(99)
				a.Score = &n
			}
		}), "invalid_attempt"},
		"forged outcome": {replace(schema.KindAttemptRecord, func(p any) {
			if a := p.(*schema.AttemptRecord); a.Score != nil {
				a.Outcome, a.Score = speculate.OutcomeCancelled, nil
			}
		}), "invalid_attempt"},
		"session selector": {replace(schema.KindSelectionDecision, func(p any) {
			p.(*schema.SelectionDecision).Selector = schema.Selector{Kind: "root", ID: root.ID()}
		}), "run_incomplete"},
	}
	for name, c := range cases {
		if err := publish(c.records); !apierr.HasCode(err, c.code) {
			t.Errorf("%s: want %s, got %v", name, c.code, err)
		}
	}
	if err := publish(valid.records); err != nil {
		t.Fatalf("the assembled group is valid: %v", err)
	}
}

func find(t *testing.T, records []envelope.Object, kind schema.Kind) envelope.Object {
	t.Helper()
	for _, o := range records {
		if o.Envelope.Kind == kind {
			return o
		}
	}
	t.Fatalf("no %s", kind)
	return envelope.Object{}
}

func TestAC120_ExportVerifiesRunsWithoutTheIndex(t *testing.T) {
	// AC-061, AC-120, AC-195: a resolved run exported by its winner's
	// manifest carries the run, every attempt record and the selection, and
	// verifies from the bundle alone; a forged attempt record does not.
	s := fixture(t)
	f := newSpec(t, s, submissionScorer(), nil)
	m := f.scripted(t, "1", "3")
	call(t, s, f.runReq("speculation.evaluate"))
	call(t, s, request("speculation.score", "", map[string]any{"run_id": f.runID, "scores": map[string]any{m[0]: 1, m[1]: 3}}))
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "run.ledger")
	if _, err := verify.Export(ctx, s.Store.Repo, verify.ExportOptions{Out: out, Manifests: []string{m[1]}}); err != nil {
		t.Fatal(err)
	}
	src, err := verify.OpenBundleSource(ctx, out, verify.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	rep, err := verify.Verify(ctx, src, verify.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Integrity != verify.Verified || !rep.Complete || len(rep.Runs) != 1 {
		t.Fatalf("bundle: %s complete=%v runs=%d missing=%v invalid=%v", rep.Integrity, rep.Complete, len(rep.Runs), rep.Missing, rep.Invalid)
	}
	run := rep.Runs[0]
	if run.ID != f.runID || run.State != "resolved" || run.Integrity != verify.Verified || len(run.Attempts) != 2 || len(run.Selections) != 1 {
		t.Fatalf("run report: %+v", run)
	}
	wire, err := canon.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidateAPI("bundle.verify.output", wire); err != nil {
		t.Fatal(err)
	}

	// A root-signed attempt record that contradicts the run is invalid, even
	// though its signature verifies.
	root, err := s.root()
	if err != nil {
		t.Fatal(err)
	}
	recs, err := s.Store.Index.Attempts(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	forged := recs[0].AttemptRecord
	n := int64(1000)
	forged.Outcome, forged.Score, forged.FailedCriteria = speculate.OutcomeTiebreaker, &n, []string{}
	o, err := envelope.Seal(schema.KindAttemptRecord, forged, root)
	if err != nil {
		t.Fatal(err)
	}
	g, err := verify.OpenRepoSource(ctx, s.Store.Repo)
	if err != nil {
		t.Fatal(err)
	}
	g.Corpus.Objects[o.Hash] = o
	g.Corpus.Committed[o.Hash] = true
	rep, err = verify.Verify(ctx, g, verify.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Integrity != verify.Invalid || rep.Runs[0].Integrity != verify.Invalid {
		t.Fatalf("a contradicting attempt record must be invalid: %s %+v", rep.Integrity, rep.Runs[0])
	}
}

func TestAC119_CancelStopsAnInFlightEvaluation(t *testing.T) {
	// AC-119: cancelling a run while its checks execute stops them, closes
	// every attempt as cancelled and selects nothing; the evaluation that
	// was running reports run_cancelled instead of a winner.
	s := fixture(t)
	writeChecks(t, s, []schema.CheckDefinition{{Name: "ok", Category: "mechanical", Command: "sleep 20", WorkingDirectory: ".", EnvAllowlist: []string{}, TimeoutSeconds: 60, OutputLimitBytes: 1024}})
	intent := call(t, s, request("intent.create", "", map[string]any{"description": "slow", "criteria": []any{
		map[string]any{"id": "ok", "statement": "slow check", "verification": map[string]any{"kind": "check", "check": "ok"}},
	}}))
	f := &specFix{s: s, intentID: intent["intent_id"].(string)}
	f.start = call(t, s, request("speculation.start", "", startInputFor(f.intentID, submissionScorer(), map[string]any{"strategies": []any{map[string]any{"name": "solo", "spec": "x"}}})))
	f.runID = f.start["run_id"].(string)
	f.attempts = []map[string]any{f.start["attempts"].([]any)[0].(map[string]any)}
	f.write(t, 0, "ok.txt", "1")
	f.submit(t, 0)
	done := make(chan error, 1)
	go func() {
		_, err := s.Handle(context.Background(), f.runReq("speculation.evaluate"))
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st := f.status(t); st["state"] == "evaluating" {
			s.mu.Lock()
			running := s.evaluating[f.runID] != nil
			s.mu.Unlock()
			if running {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("evaluation never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	started := time.Now()
	out := call(t, s, request("speculation.cancel", "", map[string]any{"run_id": f.runID, "reason": "superseded"}))
	if out["state"] != "cancelled" || out["selection"] != nil {
		t.Fatalf("cancel: %v", out)
	}
	select {
	case err := <-done:
		if !apierr.HasCode(err, "run_cancelled") {
			t.Fatalf("the running evaluation must report run_cancelled: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancellation did not stop the running check")
	}
	if time.Since(started) > 15*time.Second {
		t.Fatal("cancellation waited for the check to finish")
	}
}
