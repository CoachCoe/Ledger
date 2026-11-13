package core

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// binding names a proposal for the criteria queries.
func binding(p map[string]any, extra map[string]any) map[string]any {
	in := map[string]any{"manifest": p["manifest"]}
	for k, v := range extra {
		in[k] = v
	}
	return in
}

// statuses returns criterion id -> status object from intent.criteriaStatus.
func statuses(t *testing.T, s *Service, p map[string]any, extra map[string]any) (map[string]map[string]any, map[string]any) {
	t.Helper()
	out := call(t, s, request("intent.criteriaStatus", "", binding(p, extra)))
	byID := map[string]map[string]any{}
	for _, c := range out["criteria"].([]any) {
		c := c.(map[string]any)
		byID[c["criterion_id"].(string)] = c
	}
	return byID, out
}

func hashesOf(v any) []string {
	out := []string{}
	for _, h := range v.([]any) {
		out = append(out, h.(string))
	}
	return out
}

func rejectedReasons(out map[string]any) map[string][]string {
	got := map[string][]string{}
	for _, r := range out["rejected"].([]any) {
		r := r.(map[string]any)
		key := r["criterion_id"].(string)
		got[key] = append(got[key], r["reason"].(string))
	}
	return got
}

func judge(t *testing.T, s *Service, sid string, p map[string]any, criterion string) map[string]any {
	t.Helper()
	return call(t, s, request("evidence.judge", sid, binding(p, map[string]any{"criterion_id": criterion, "result": "pass"})))
}

func sessionOfKind(t *testing.T, s *Service, kind string) string {
	t.Helper()
	return call(t, s, request("session.create", "", map[string]any{"agent_name": kind + "-reviewer", "participant_kind": kind}))["session_id"].(string)
}

// judgmentFix is a proposal whose criteria are judgments with each reviewer kind.
type judgmentFix struct {
	sid, wid, dir string
	proposal      map[string]any
}

func newJudgmentFix(t *testing.T, s *Service) *judgmentFix {
	t.Helper()
	session := call(t, s, request("session.create", "", map[string]any{"agent_name": "author"}))
	f := &judgmentFix{sid: session["session_id"].(string)}
	var criteria []any
	for _, who := range []string{"human", "agent", "any"} {
		criteria = append(criteria, map[string]any{"id": "by-" + who, "statement": who + " review", "verification": map[string]any{"kind": "judgment", "reviewer": who}})
	}
	intent := call(t, s, request("intent.create", "", map[string]any{"description": "judgments", "criteria": criteria}))
	ws := call(t, s, request("workspace.fork", f.sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]}))
	f.wid, f.dir = ws["workspace_id"].(string), ws["path"].(string)
	if err := os.WriteFile(filepath.Join(f.dir, "a.ts"), []byte("export const j=1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.proposal = call(t, s, request("workspace.propose", f.sid, map[string]any{"workspace_id": f.wid}))
	return f
}

func TestAC040_041_CheckAndJudgmentCriteria(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)

	// Nothing recorded: every criterion is outstanding with nothing deciding it.
	st, out := statuses(t, s, f.proposal, nil)
	if len(st) != 3 || len(out["evidence_set"].([]any)) != 0 || len(out["contrary_evidence"].([]any)) != 0 {
		t.Fatalf("empty: %v", out)
	}
	for id, c := range st {
		if c["status"] != "outstanding" || len(c["deciding"].([]any)) != 0 || len(c["conflicting"].([]any)) != 0 {
			t.Fatalf("%s: %v", id, c)
		}
	}

	// AC-040: a passing mechanical attestation for the pinned check satisfies
	// both check criteria it serves and says which attestation decided them.
	run := call(t, s, request("evidence.run", "", f.input(nil)))
	att := run["attestation"].(string)
	st, out = statuses(t, s, f.proposal, nil)
	for _, id := range []string{"tests", "lint"} {
		if st[id]["status"] != "satisfied" || !slices.Equal(hashesOf(st[id]["deciding"]), []string{att}) || len(st[id]["conflicting"].([]any)) != 0 {
			t.Fatalf("%s: %v", id, st[id])
		}
	}
	if st["review"]["status"] != "outstanding" || out["intent_id"] != f.intent["intent_id"] {
		t.Fatalf("review before judgment: %v", out)
	}

	// AC-041: an agent's judgment does not satisfy a human reviewer.
	agent := sessionOfKind(t, s, "agent")
	j := judge(t, s, agent, f.proposal, "review")
	if j["reviewer_kind"] != "agent" || j["reviewer_session"] != agent {
		t.Fatalf("agent judgment: %v", j)
	}
	st, out = statuses(t, s, f.proposal, nil)
	if st["review"]["status"] != "outstanding" {
		t.Fatalf("agent judgment satisfied a human criterion: %v", st["review"])
	}
	if got := rejectedReasons(out)["review"]; !slices.Equal(got, []string{"reviewer_kind_mismatch"}) {
		t.Fatalf("rejected: %v", out["rejected"])
	}

	// A root-provisioned human verifier session satisfies it, and is ended.
	h := judge(t, s, "", f.proposal, "review")
	if h["reviewer_kind"] != "human" || h["reviewer_session"] == agent || h["reviewer_session"] == f.sid {
		t.Fatalf("human judgment: %v", h)
	}
	if got := f.sessionState(t, h["reviewer_session"].(string)); got != "ended" {
		t.Fatalf("the human verifier session is %s, not ended", got)
	}
	st, _ = statuses(t, s, f.proposal, nil)
	if st["review"]["status"] != "satisfied" || !slices.Equal(hashesOf(st["review"]["deciding"]), []string{h["attestation"].(string)}) {
		t.Fatalf("human judgment: %v", st["review"])
	}
	o, err := s.Store.Read(h["attestation"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var a schema.EvidenceAttestation
	if err := o.Decode(&a); err != nil {
		t.Fatal(err)
	}
	if a.Category != "judgment" || a.Setup != nil || a.Check != nil || !slices.Equal(a.Criteria, []string{"review"}) || a.Isolation != "none" || a.VerifierSession != o.Envelope.Signer || a.Result != "pass" {
		t.Fatalf("judgment attestation: %+v", a)
	}

	// Reviewer kinds any/agent/human, and a service session that never satisfies one.
	j2 := newJudgmentFix(t, s)
	svc := sessionOfKind(t, s, "service")
	judge(t, s, svc, j2.proposal, "by-any")
	st, out = statuses(t, s, j2.proposal, nil)
	if st["by-any"]["status"] != "outstanding" || !slices.Equal(rejectedReasons(out)["by-any"], []string{"reviewer_kind_mismatch"}) {
		t.Fatalf("a service session satisfied a judgment: %v", out)
	}
	judge(t, s, agent, j2.proposal, "by-any")
	judge(t, s, agent, j2.proposal, "by-agent")
	judge(t, s, agent, j2.proposal, "by-human")
	st, _ = statuses(t, s, j2.proposal, nil)
	if st["by-any"]["status"] != "satisfied" || st["by-agent"]["status"] != "satisfied" || st["by-human"]["status"] != "outstanding" {
		t.Fatalf("agent judgments: %v", st)
	}
	judge(t, s, "", j2.proposal, "by-human")
	judge(t, s, "", j2.proposal, "by-agent") // a human is not an agent
	st, out = statuses(t, s, j2.proposal, nil)
	if st["by-human"]["status"] != "satisfied" || len(st["by-agent"]["deciding"].([]any)) != 1 {
		t.Fatalf("human judgment: %v", st)
	}
	if !slices.Contains(rejectedReasons(out)["by-agent"], "reviewer_kind_mismatch") {
		t.Fatalf("human judgment on an agent criterion: %v", out["rejected"])
	}
}

func TestAC113_EndToEndTruthTable(t *testing.T) {
	// AC-113 through the real stack: none -> outstanding, all pass ->
	// satisfied, any fail -> failed, mixed -> failed with conflicting detail.
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	if st, _ := statuses(t, s, f.proposal, nil); st["tests"]["status"] != "outstanding" {
		t.Fatalf("none: %v", st["tests"])
	}
	pass := call(t, s, request("evidence.run", "", f.input(nil)))["attestation"].(string)
	if st, _ := statuses(t, s, f.proposal, nil); st["tests"]["status"] != "satisfied" {
		t.Fatalf("all pass: %v", st["tests"])
	}
	third := sessionOfKind(t, s, "service")
	fail := forgeEvidence(t, s, f, third, func(a *schema.EvidenceAttestation) {
		one, reason := int64(1), "check_failed"
		a.Result, a.Reason, a.Check = "fail", &reason, &schema.Phase{ExitStatus: &one}
	})
	st, out := statuses(t, s, f.proposal, nil)
	for _, id := range []string{"tests", "lint"} {
		if st[id]["status"] != "failed" || !slices.Equal(hashesOf(st[id]["deciding"]), []string{fail}) || !slices.Equal(hashesOf(st[id]["conflicting"]), []string{pass}) {
			t.Fatalf("mixed %s: %v", id, st[id])
		}
	}
	// Only the failure: failed with nothing in conflict.
	st, out2 := statuses(t, s, f.proposal, map[string]any{"evidence": []string{fail}})
	if st["tests"]["status"] != "failed" || len(st["tests"]["conflicting"].([]any)) != 0 || !slices.Equal(hashesOf(out2["evidence_set"]), []string{fail}) {
		t.Fatalf("all fail: %v", st["tests"])
	}
	if !slices.Equal(hashesOf(out["contrary_evidence"]), []string{fail}) {
		t.Fatalf("contrary evidence: %v", out["contrary_evidence"])
	}
}

// template returns the run's genuine attestation. Replayed test groups live
// only in the index, so the first readable mechanical attestation is used.
func template(t *testing.T, f *runFix) schema.EvidenceAttestation {
	t.Helper()
	for _, h := range f.attestations(t) {
		if o, err := f.s.Store.Read(h); err == nil {
			var a schema.EvidenceAttestation
			if err := o.Decode(&a); err == nil && a.Category != "judgment" && a.Result == "pass" {
				return a
			}
		}
	}
	t.Fatal("no genuine attestation to copy")
	return schema.EvidenceAttestation{}
}

// forgeEvidence publishes a copy of the run's attestation signed by sid,
// through the validating publish path, after applying edit.
func forgeEvidence(t *testing.T, s *Service, f *runFix, sid string, edit func(*schema.EvidenceAttestation)) string {
	t.Helper()
	att := template(t, f)
	att.VerifierSession = sid
	if edit != nil {
		edit(&att)
	}
	key := s.signers[sid]
	o, err := envelope.SealVersion(2, schema.KindEvidenceAttestation, att, key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Store.Do(context.Background(), record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "test.forge", Digest: canon.Hash([]byte(o.Hash)), Signer: key}, func(*index.Index) (any, []envelope.Object, error) {
		return map[string]any{}, []envelope.Object{o}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return o.Hash
}

// replayEvidence indexes an attestation signed by sid as a fetched group
// would be: signatures and membership are checked, publish-time authority
// rules are not. Readers must re-derive independence themselves.
func replayEvidence(t *testing.T, s *Service, f *runFix, sid string, edit func(*schema.EvidenceAttestation)) string {
	t.Helper()
	att := template(t, f)
	att.VerifierSession = sid
	if edit != nil {
		edit(&att)
	}
	o, err := envelope.SealVersion(2, schema.KindEvidenceAttestation, att, s.signers[sid])
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.root()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := envelope.SealVersion(2, schema.KindOperationReceipt, schema.OperationReceipt{
		OperationID: schema.NewID(schema.PrefixOperation), Operation: "test.replay", RequestDigest: canon.Hash([]byte(o.Hash)),
		Result: []byte(`{}`), Members: []string{o.Hash}, At: now(),
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Index.Apply(context.Background(), index.Group{Receipt: receipt, Members: []envelope.Object{o}}); err != nil {
		t.Fatal(err)
	}
	return o.Hash
}

func TestAC112_JudgmentBindsCriterionAndRevision(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	ctx := context.Background()

	// The reviewer kind is never caller-supplied: the schema has no such member.
	for _, extra := range []map[string]any{{"reviewer": "human"}, {"reviewer_kind": "human"}, {"signer": "x"}, {"criteria": []string{"review"}}} {
		in := binding(f.proposal, map[string]any{"criterion_id": "review", "result": "pass"})
		for k, v := range extra {
			in[k] = v
		}
		if _, err := s.Handle(ctx, request("evidence.judge", "", in)); !apierr.HasCode(err, "schema_invalid") {
			t.Fatalf("input %v: %v", extra, err)
		}
	}
	if _, err := s.Handle(ctx, request("evidence.judge", "", binding(f.proposal, map[string]any{"criterion_id": "review", "result": "maybe"}))); !apierr.HasCode(err, "schema_invalid") {
		t.Fatalf("result enum: %v", err)
	}
	// A judgment names a judgment criterion of this intent.
	expectCode(t, s, "evidence.judge", "", binding(f.proposal, map[string]any{"criterion_id": "tests", "result": "pass"}), "not_judgment")
	expectCode(t, s, "evidence.judge", "", binding(f.proposal, map[string]any{"criterion_id": "nope", "result": "pass"}), "unknown_criterion")
	// It binds the exact commit of the manifest.
	expectCode(t, s, "evidence.judge", "", map[string]any{"manifest": f.proposal["manifest"], "commit": strings.Repeat("1", 40), "criterion_id": "review", "result": "pass"}, "schema_invalid")
	expectCode(t, s, "evidence.judge", "", map[string]any{"manifest": strings.Repeat("2", 64), "criterion_id": "review", "result": "pass"}, "unknown_manifest")
	// The author and a rebound holder cannot judge their own change.
	expectCode(t, s, "evidence.judge", f.sid, binding(f.proposal, map[string]any{"criterion_id": "review", "result": "pass"}), "unauthorized")
	if got := f.attestations(t); len(got) != 0 {
		t.Fatalf("refused judgments were stored: %v", got)
	}

	// A judgment of revision one, with a reason, retried by operation id.
	first := f.proposal
	req := request("evidence.judge", "", binding(first, map[string]any{"criterion_id": "review", "result": "pass", "reason": "reads well"}))
	one := call(t, s, req)
	replay, err := s.Handle(ctx, req)
	if err != nil || !replay.(*record.Result).Replayed {
		t.Fatalf("retry should replay: %v %v", replay, err)
	}
	a := f.attestation(t, one["attestation"].(string))
	if a.Manifest != first["manifest"] || a.Tree != first["tree"] || a.Reason == nil || *a.Reason != "reads well" {
		t.Fatalf("binding: %+v", a)
	}
	if st, _ := statuses(t, s, first, nil); st["review"]["status"] != "satisfied" {
		t.Fatalf("revision one: %v", st["review"])
	}

	// Revision two: the earlier judgment does not carry over, and a new
	// judgment of the stale revision is refused.
	call(t, s, request("workspace.rework", f.sid, map[string]any{"workspace_id": f.wid}))
	f.propose(t, "export const x=3;\n")
	second := f.proposal
	st, out := statuses(t, s, second, nil)
	if st["review"]["status"] != "outstanding" || len(out["evidence_set"].([]any)) != 0 {
		t.Fatalf("revision two inherited a judgment: %v", out)
	}
	expectCode(t, s, "intent.criteriaStatus", "", binding(second, map[string]any{"evidence": []string{one["attestation"].(string)}}), "cross_revision_evidence")
	expectCode(t, s, "evidence.judge", "", binding(first, map[string]any{"criterion_id": "review", "result": "pass"}), "stale_revision")
	judge(t, s, "", second, "review")
	if st, _ := statuses(t, s, second, nil); st["review"]["status"] != "satisfied" {
		t.Fatalf("revision two: %v", st["review"])
	}

	holder := call(t, s, request("session.recover", "", map[string]any{"workspace_id": f.wid}))["session_id"].(string)
	expectCode(t, s, "evidence.judge", holder, binding(second, map[string]any{"criterion_id": "review", "result": "pass"}), "unauthorized")

	// An author-signed judgment cannot be published.
	genuine := f.attestation(t, one["attestation"].(string))
	genuine.Manifest, genuine.Tree = second["manifest"].(string), second["tree"].(string)
	genuine.VerifierSession = f.sid
	key := s.signers[f.sid]
	o, err := envelope.SealVersion(2, schema.KindEvidenceAttestation, genuine, key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Store.Do(ctx, record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "test.forge", Digest: canon.Hash([]byte(o.Hash)), Signer: key}, func(*index.Index) (any, []envelope.Object, error) {
		return map[string]any{}, []envelope.Object{o}, nil
	})
	if !apierr.HasCode(err, "author_signed_evidence") {
		t.Fatalf("author-signed judgment: %v", err)
	}
	// Judgment shape: a judgment that records a process phase, names two
	// criteria or references a different definition is refused at publish.
	third := sessionOfKind(t, s, "agent")
	shaped := func(edit func(*schema.EvidenceAttestation)) error {
		att := f.attestation(t, one["attestation"].(string))
		att.Manifest, att.Tree, att.VerifierSession = second["manifest"].(string), second["tree"].(string), third
		edit(&att)
		k := s.signers[third]
		o, err := envelope.SealVersion(2, schema.KindEvidenceAttestation, att, k)
		if err != nil {
			return err
		}
		_, err = s.Store.Do(ctx, record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "test.forge", Digest: canon.Hash([]byte(o.Hash)), Signer: k}, func(*index.Index) (any, []envelope.Object, error) {
			return map[string]any{}, []envelope.Object{o}, nil
		})
		return err
	}
	if err := shaped(func(a *schema.EvidenceAttestation) { a.Check = &schema.Phase{ExitStatus: new(int64)} }); !apierr.HasCode(err, "invalid_attestation") {
		t.Fatalf("judgment with phase: %v", err)
	}
	if err := shaped(func(a *schema.EvidenceAttestation) { a.Criteria = []string{"review", "tests"} }); !apierr.HasCode(err, "invalid_attestation") {
		t.Fatalf("judgment with two criteria: %v", err)
	}
	if err := shaped(func(a *schema.EvidenceAttestation) { a.Criteria = []string{"tests"} }); !apierr.HasCode(err, "invalid_attestation") {
		t.Fatalf("judgment of a check criterion: %v", err)
	}
	if err := shaped(func(a *schema.EvidenceAttestation) { a.Result = "pass" }); err != nil {
		t.Fatalf("a well-formed judgment by an independent agent session: %v", err)
	}
}

func TestAC169_IndependenceQuery(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	ctx := context.Background()
	genuine := call(t, s, request("evidence.run", "", f.input(nil)))["attestation"].(string)

	// Evidence signed by the author or a rebound holder cannot be published,
	// but a fetched group can carry it; the query flags it either way.
	holder := call(t, s, request("session.recover", "", map[string]any{"workspace_id": f.wid}))["session_id"].(string)
	failAs := func(a *schema.EvidenceAttestation) {
		one, reason := int64(1), "check_failed"
		a.Result, a.Reason, a.Check = "fail", &reason, &schema.Phase{ExitStatus: &one}
	}
	byAuthor := replayEvidence(t, s, f, f.sid, failAs)
	byHolder := replayEvidence(t, s, f, holder, failAs)
	stranger := sessionOfKind(t, s, "agent")
	mismatched := replayEvidence(t, s, f, stranger, func(a *schema.EvidenceAttestation) { a.VerifierSession = holder })

	out := call(t, s, request("evidence.isIndependent", "", binding(f.proposal, nil)))
	got := map[string]map[string]any{}
	for _, a := range out["attestations"].([]any) {
		a := a.(map[string]any)
		got[a["hash"].(string)] = a
	}
	if len(got) != 4 {
		t.Fatalf("attestations: %v", out["attestations"])
	}
	if g := got[genuine]; g["independent"] != true || g["reason"] != nil || g["signer_kind"] != "service" || g["result"] != "pass" || g["category"] != "mechanical" {
		t.Fatalf("genuine: %v", g)
	}
	for hash, want := range map[string]string{byAuthor: "signer_is_author", byHolder: "signer_held_workspace", mismatched: "signer_is_not_verifier_session"} {
		if g := got[hash]; g["independent"] != false || g["reason"] != want {
			t.Fatalf("%s: want %s, got %v", hash, want, g)
		}
	}
	if !slices.Equal(hashesOf(out["independent_passing"]), []string{genuine}) {
		t.Fatalf("independent passing: %v", out["independent_passing"])
	}
	// Independence is not correctness, and the response says so.
	if note, _ := out["note"].(string); !strings.Contains(note, "does not establish") || !strings.Contains(note, "correct") {
		t.Fatalf("note: %q", out["note"])
	}

	// Non-independent evidence never decides a criterion, for or against,
	// and is not contrary evidence; it is reported as rejected.
	st, status := statuses(t, s, f.proposal, nil)
	if st["tests"]["status"] != "satisfied" || !slices.Equal(hashesOf(st["tests"]["deciding"]), []string{genuine}) {
		t.Fatalf("tests: %v", st["tests"])
	}
	if len(status["contrary_evidence"].([]any)) != 0 {
		t.Fatalf("contrary: %v", status["contrary_evidence"])
	}
	if r := rejectedReasons(status)["tests"]; len(r) != 3 || r[0] != "not_independent" {
		t.Fatalf("rejected: %v", status["rejected"])
	}

	// An unrelated session is independent; v2 bindings name no commit.
	expectCode(t, s, "evidence.isIndependent", "", map[string]any{"manifest": f.proposal["manifest"], "commit": strings.Repeat("1", 40)}, "schema_invalid")
	expectCode(t, s, "evidence.isIndependent", "", map[string]any{"manifest": strings.Repeat("2", 64)}, "unknown_manifest")
	for _, in := range []map[string]any{{}, binding(f.proposal, map[string]any{"extra": 1})} {
		if _, err := s.Handle(ctx, request("evidence.isIndependent", "", in)); !apierr.HasCode(err, "schema_invalid") {
			t.Fatalf("input %v: %v", in, err)
		}
	}
}

func TestAC170_180_CriteriaStatusQuery(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	ctx := context.Background()
	pass := call(t, s, request("evidence.run", "", f.input(nil)))["attestation"].(string)
	fail := forgeEvidence(t, s, f, sessionOfKind(t, s, "service"), func(a *schema.EvidenceAttestation) {
		one, reason := int64(1), "check_failed"
		a.Result, a.Reason, a.Check = "fail", &reason, &schema.Phase{ExitStatus: &one}
	})
	first := f.proposal

	// Default set: every known attestation for the exact revision.
	st, out := statuses(t, s, first, nil)
	if want := []string{pass, fail}; !slices.Equal(hashesOf(out["evidence_set"]), sortedCopy(want)) {
		t.Fatalf("default set: %v", out["evidence_set"])
	}
	if st["tests"]["status"] != "failed" || st["review"]["status"] != "outstanding" {
		t.Fatalf("default: %v", st)
	}
	// Explicit sets decide on their own; contrary evidence is still returned.
	st, out = statuses(t, s, first, map[string]any{"evidence": []string{pass}})
	if st["tests"]["status"] != "satisfied" || !slices.Equal(hashesOf(st["tests"]["deciding"]), []string{pass}) {
		t.Fatalf("explicit pass: %v", st["tests"])
	}
	if !slices.Equal(hashesOf(out["contrary_evidence"]), []string{fail}) || !slices.Equal(hashesOf(out["evidence_set"]), []string{pass}) {
		t.Fatalf("contrary evidence outside the set: %v", out)
	}
	st, _ = statuses(t, s, first, map[string]any{"evidence": []string{}})
	if st["tests"]["status"] != "outstanding" {
		t.Fatalf("empty explicit set: %v", st["tests"])
	}
	// Duplicates and order do not matter.
	_, out = statuses(t, s, first, map[string]any{"evidence": []string{pass, fail}})
	_, rev := statuses(t, s, first, map[string]any{"evidence": []string{fail, pass}})
	a, _ := canon.Marshal(out)
	b, _ := canon.Marshal(rev)
	if string(a) != string(b) {
		t.Fatal("evidence order changed the result")
	}

	// Unknown hashes, non-attestation hashes and other revisions are errors.
	expectCode(t, s, "intent.criteriaStatus", "", binding(first, map[string]any{"evidence": []string{strings.Repeat("9", 64)}}), "unknown_evidence")
	expectCode(t, s, "intent.criteriaStatus", "", binding(first, map[string]any{"evidence": []string{first["manifest"].(string)}}), "unknown_evidence")
	call(t, s, request("workspace.rework", f.sid, map[string]any{"workspace_id": f.wid}))
	f.propose(t, "export const x=3;\n")
	second := f.proposal
	expectCode(t, s, "intent.criteriaStatus", "", binding(second, map[string]any{"evidence": []string{pass}}), "cross_revision_evidence")
	st, out = statuses(t, s, second, nil)
	if st["tests"]["status"] != "outstanding" || len(out["contrary_evidence"].([]any)) != 0 {
		t.Fatalf("revision two inherited revision one evidence: %v", out)
	}
	// Revision one is unchanged by the later revision.
	if st, _ := statuses(t, s, first, nil); st["tests"]["status"] != "failed" {
		t.Fatalf("revision one: %v", st["tests"])
	}

	// The binding and schema are strict.
	expectCode(t, s, "intent.criteriaStatus", "", map[string]any{"manifest": first["manifest"], "commit": strings.Repeat("1", 40)}, "schema_invalid")
	expectCode(t, s, "intent.criteriaStatus", "", map[string]any{"manifest": strings.Repeat("2", 64)}, "unknown_manifest")
	for _, in := range []map[string]any{
		{},
		binding(first, map[string]any{"status": "satisfied"}),
		binding(first, map[string]any{"evidence": []string{"not-a-hash"}}),
		binding(first, map[string]any{"evidence": []string{pass, pass}}),
	} {
		if _, err := s.Handle(ctx, request("intent.criteriaStatus", "", in)); !apierr.HasCode(err, "schema_invalid") {
			t.Fatalf("input %v: %v", in, err)
		}
	}
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func TestAC112_HumanJudgmentNeedsRootAuthentication(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	ctx := context.Background()
	agent := sessionOfKind(t, s, "agent")
	req := request("evidence.judge", "", binding(f.proposal, map[string]any{"criterion_id": "review", "result": "pass"}))
	done := call(t, s, req)

	// Without the root key no human verifier session can be provisioned and
	// nothing is recorded; a session whose key was lost cannot sign either.
	restarted, err := New(ctx, s.Store, unavailableKeys{})
	if err != nil {
		t.Fatal(err)
	}
	before := f.attestations(t)
	expectCode(t, restarted, "evidence.judge", "", binding(f.proposal, map[string]any{"criterion_id": "review", "result": "fail"}), "root_unavailable")
	expectCode(t, restarted, "evidence.judge", agent, binding(f.proposal, map[string]any{"criterion_id": "review", "result": "fail"}), "session_key_unavailable")
	if after := f.attestations(t); !slices.Equal(before, after) {
		t.Fatalf("a refused judgment recorded evidence: %v -> %v", before, after)
	}
	// The committed operation still replays its frozen result.
	replay, err := restarted.Handle(ctx, req)
	if err != nil || !replay.(*record.Result).Replayed {
		t.Fatalf("retry without keys: %v %v", replay, err)
	}
	if replayed := call(t, restarted, req); replayed["attestation"] != done["attestation"] {
		t.Fatalf("replayed result: %v", replayed)
	}
	// No verifier session is left active by judgments.
	active, err := s.Store.Index.SubjectsIn(ctx, "session", "active")
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range active {
		if sub.ID == done["reviewer_session"] {
			t.Fatal("the human verifier session is still active")
		}
	}
}
