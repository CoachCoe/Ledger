package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/selection"
)

// selectIn is the closed input of selection.record for f's current proposal.
func (f *runFix) selectIn(evidence []string, extra map[string]any) map[string]any {
	in := map[string]any{"manifest": f.proposal["manifest"], "evidence": evidence}
	for k, v := range extra {
		in[k] = v
	}
	return in
}

func (f *runFix) decision(t *testing.T, hash string) schema.SelectionDecision {
	t.Helper()
	o, err := f.s.Store.Read(hash)
	if err != nil {
		t.Fatal(err)
	}
	var d schema.SelectionDecision
	if err := o.Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *runFix) selections(t *testing.T) []string {
	t.Helper()
	got, err := f.s.Store.Index.SelectionsFor(context.Background(), f.proposal["manifest"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// forgeSelection publishes a decision built from the real evaluation and
// edited by edit, through the validating publish path, signed by signer.
func forgeSelection(t *testing.T, s *Service, f *runFix, signer identity.Signer, edit func(*schema.SelectionDecision)) error {
	t.Helper()
	ctx := context.Background()
	_, sb, err := s.criteriaBinding(ctx, f.proposal["manifest"].(string))
	if err != nil {
		t.Fatal(err)
	}
	known, err := s.Store.Index.EvidenceFor(ctx, sb.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	kind := "root"
	if strings.HasPrefix(signer.ID(), "ses_") {
		kind = "session"
	}
	m, err := s.Store.Index.Manifest(ctx, sb.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	d, err := selection.Build(sb, m.IntentID, known, known, schema.Selector{Kind: kind, ID: signer.ID()}, nil, nil, now())
	if err != nil {
		t.Fatal(err)
	}
	edit(&d)
	o, err := envelope.SealVersion(2, schema.KindSelectionDecision, d, signer)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Store.Do(ctx, record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "test.forge", Digest: canon.Hash([]byte(o.Hash)), Signer: signer}, func(*index.Index) (any, []envelope.Object, error) {
		return map[string]any{}, []envelope.Object{o}, nil
	})
	return err
}

// line is the head of line main, which only a line_advance moves.
func (f *runFix) line(t *testing.T) index.LineHead {
	t.Helper()
	head, err := f.s.Store.Index.LineHead(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func TestAC059_171_DecisionFields(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	att := call(t, s, request("evidence.run", "", f.input(nil)))["attestation"].(string)

	out := call(t, s, request("selection.record", "", f.selectIn([]string{att}, nil)))
	if out["accepted"] != false || out["manifest"] != f.proposal["manifest"] || out["commit"] != nil || out["intent_id"] != f.intent["intent_id"] {
		t.Fatalf("result: %v", out)
	}
	if !slices.Equal(hashesOf(out["outstanding"]), []string{"review"}) {
		t.Fatalf("outstanding lists the undecided judgment criterion: %v", out["outstanding"])
	}
	d := f.decision(t, out["selection"].(string))
	o, err := s.Store.Read(out["selection"].(string))
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.root()
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Verify(root.PublicKey()); err != nil || o.Envelope.Signer != root.ID() {
		t.Fatalf("the decision is signed by the root key: %v signer %s", err, o.Envelope.Signer)
	}
	if d.IntentID != f.intent["intent_id"] || d.Manifest != f.proposal["manifest"] || d.Commit != "" ||
		!slices.Equal(d.Evidence, []string{att}) || d.Competitors == nil || len(d.Competitors) != 0 ||
		d.ContraryEvidence == nil || len(d.ContraryEvidence) != 0 || string(d.Scoring) != "null" ||
		d.Selector != (schema.Selector{Kind: "root", ID: root.ID()}) || d.At == "" {
		t.Fatalf("decision: %+v", d)
	}
	byID := map[string]schema.CriterionStatus{}
	for _, c := range d.Criteria {
		byID[c.CriterionID] = c
	}
	if len(byID) != 3 {
		t.Fatalf("every criterion is recorded: %+v", d.Criteria)
	}
	for _, id := range []string{"tests", "lint"} {
		if c := byID[id]; c.Status != "satisfied" || !slices.Equal(c.Deciding, []string{att}) || len(c.Conflicting) != 0 {
			t.Fatalf("%s: %+v", id, c)
		}
	}
	if c := byID["review"]; c.Status != "outstanding" || len(c.Deciding) != 0 || len(c.Conflicting) != 0 {
		t.Fatalf("review: %+v", c)
	}
	if got := f.selections(t); !slices.Equal(got, []string{out["selection"].(string)}) {
		t.Fatalf("indexed selections: %v", got)
	}

	// The recorded statuses equal what the query computes for the same set.
	st, _ := statuses(t, s, f.proposal, map[string]any{"evidence": []string{att}})
	for _, c := range d.Criteria {
		if st[c.CriterionID]["status"] != c.Status {
			t.Fatalf("%s: query says %v, decision says %s", c.CriterionID, st[c.CriterionID]["status"], c.Status)
		}
	}

	// Scoring and competitors are carried through; a retry replays the result.
	rival := newRival(t, s, f)
	scored := request("selection.record", "", f.selectIn([]string{att}, map[string]any{"competitors": []string{rival}, "scoring": map[string]any{"score": 3}}))
	second := call(t, s, scored)
	d2 := f.decision(t, second["selection"].(string))
	if !slices.Equal(d2.Competitors, []string{rival}) || string(d2.Scoring) != `{"score":3}` {
		t.Fatalf("competitors/scoring: %+v", d2)
	}
	again := call(t, s, scored)
	if again["selection"] != second["selection"] {
		t.Fatal("retrying the same operation id must replay the frozen decision")
	}

	// A non-author session may select too; its identity is the selector.
	reviewer := sessionOfKind(t, s, "agent")
	bySession := call(t, s, request("selection.record", reviewer, f.selectIn([]string{att}, nil)))
	if got := f.decision(t, bySession["selection"].(string)).Selector; got != (schema.Selector{Kind: "session", ID: reviewer}) {
		t.Fatalf("session selector: %+v", got)
	}
}

// newRival proposes a second change for f's intent from another session.
func newRival(t *testing.T, s *Service, f *runFix) string {
	t.Helper()
	session := call(t, s, request("session.create", "", map[string]any{"agent_name": "rival"}))
	sid := session["session_id"].(string)
	ws := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": f.intent["intent_id"], "capability_id": session["capability_id"]}))
	if err := os.WriteFile(filepath.Join(ws["path"].(string), "a.ts"), []byte("export const x=7;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := call(t, s, request("workspace.propose", sid, map[string]any{"workspace_id": ws["workspace_id"]}))
	return p["manifest"].(string)
}

func TestAC060_232_247_SelectionIsNotAcceptance(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	att := call(t, s, request("evidence.run", "", f.input(nil)))["attestation"].(string)
	ctx := context.Background()
	before := f.line(t)
	manifestBytes, err := os.ReadFile(s.Store.ObjectPath(f.proposal["manifest"].(string)))
	if err != nil {
		t.Fatal(err)
	}

	out := call(t, s, request("selection.record", "", f.selectIn([]string{att}, nil)))

	// Outstanding stays outstanding in the record, and the result says it is not accepted.
	d := f.decision(t, out["selection"].(string))
	if out["accepted"] != false || !slices.Equal(hashesOf(out["outstanding"]), []string{"review"}) {
		t.Fatalf("result: %v", out)
	}
	for _, c := range d.Criteria {
		if c.CriterionID == "review" && c.Status != "outstanding" {
			t.Fatalf("an unreviewed criterion was recorded as %s", c.Status)
		}
	}
	// The line did not move: selection writes no line_advance.
	if after := f.line(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("selection moved line main: %+v -> %+v", before, after)
	}
	if now, _ := os.ReadFile(s.Store.ObjectPath(f.proposal["manifest"].(string))); string(now) != string(manifestBytes) {
		t.Fatal("manifest object changed")
	}
	// The workspace and intent keep their state: selection is not a lifecycle transition.
	ws, err := s.Store.Index.Subject(ctx, f.wid)
	if err != nil || ws.State != "proposed" {
		t.Fatalf("workspace after selection: %+v %v", ws, err)
	}
	in, err := s.Store.Index.Subject(ctx, f.intent["intent_id"].(string))
	if err != nil || in.State != "open" {
		t.Fatalf("intent after selection: %+v %v", in, err)
	}
	// There is no merge or accept operation to call.
	for _, op := range []string{"selection.accept", "selection.merge"} {
		if _, err := s.Handle(ctx, request(op, "", map[string]any{})); err == nil {
			t.Fatalf("%s must not exist", op)
		}
	}
	// A selection with no evidence at all is allowed and says everything is outstanding.
	empty := call(t, s, request("selection.record", "", f.selectIn([]string{}, nil)))
	if len(hashesOf(empty["outstanding"])) != 3 {
		t.Fatalf("no evidence, no satisfaction: %v", empty)
	}
}

func TestAC171_RejectsFabricatedAndCrossRevision(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	ctx := context.Background()
	rev1 := call(t, s, request("evidence.run", "", f.input(nil)))["attestation"].(string)
	oldProposal := f.proposal

	// Statuses, outstanding lists and selectors are never caller input.
	for _, extra := range []map[string]any{
		{"criteria": []any{}}, {"status": "satisfied"}, {"contrary_evidence": []string{}},
		{"outstanding": []string{}}, {"selector": map[string]any{"kind": "root", "id": "x"}}, {"accepted": true},
	} {
		if _, err := s.Handle(ctx, request("selection.record", "", f.selectIn([]string{rev1}, extra))); !apierr.HasCode(err, "schema_invalid") {
			t.Fatalf("extra %v: %v", extra, err)
		}
	}
	// A missing evidence member is not "no evidence".
	if _, err := s.Handle(ctx, request("selection.record", "", map[string]any{"manifest": f.proposal["manifest"]})); !apierr.HasCode(err, "schema_invalid") {
		t.Fatalf("missing evidence: %v", err)
	}
	// Unknown evidence, and a commit, which v2 bindings do not name.
	if _, err := s.Handle(ctx, request("selection.record", "", f.selectIn([]string{strings.Repeat("ab", 32)}, nil))); !apierr.HasCode(err, "unknown_evidence") {
		t.Fatalf("unknown evidence: %v", err)
	}
	newRival(t, s, f)
	if _, err := s.Handle(ctx, request("selection.record", "", f.selectIn([]string{rev1}, map[string]any{"commit": strings.Repeat("1", 40)}))); !apierr.HasCode(err, "schema_invalid") {
		t.Fatalf("wrong commit: %v", err)
	}
	// Competitors must be other manifests of the same intent.
	if _, err := s.Handle(ctx, request("selection.record", "", f.selectIn([]string{rev1}, map[string]any{"competitors": []string{f.proposal["manifest"].(string)}}))); !apierr.HasCode(err, "invalid_competitor") {
		t.Fatalf("self as competitor: %v", err)
	}
	if _, err := s.Handle(ctx, request("selection.record", "", f.selectIn([]string{rev1}, map[string]any{"competitors": []string{strings.Repeat("cd", 32)}}))); !apierr.HasCode(err, "unknown_manifest") {
		t.Fatalf("unknown competitor: %v", err)
	}
	foreign := newRunFix(t, s, "true", nil)
	if _, err := s.Handle(ctx, request("selection.record", "", f.selectIn([]string{rev1}, map[string]any{"competitors": []string{foreign.proposal["manifest"].(string)}}))); !apierr.HasCode(err, "invalid_competitor") {
		t.Fatalf("competitor of another intent: %v", err)
	}

	// The author cannot select its own change.
	if _, err := s.Handle(ctx, request("selection.record", f.sid, f.selectIn([]string{rev1}, nil))); !apierr.HasCode(err, "unauthorized") {
		t.Fatalf("author: %v", err)
	}

	// Rework: while the workspace is open there is nothing to select.
	call(t, s, request("workspace.rework", f.sid, map[string]any{"workspace_id": f.wid}))
	if _, err := s.Handle(ctx, request("selection.record", "", f.selectIn([]string{rev1}, nil))); !apierr.HasCode(err, "invalid_state") {
		t.Fatalf("open workspace: %v", err)
	}
	f.propose(t, "export const x=3;\n")

	// Revision 1 is stale; revision 1's evidence on revision 2 is cross-revision.
	stale := map[string]any{"manifest": oldProposal["manifest"], "evidence": []string{rev1}}
	if _, err := s.Handle(ctx, request("selection.record", "", stale)); !apierr.HasCode(err, "stale_revision") {
		t.Fatalf("stale revision: %v", err)
	}
	_, err := s.Handle(ctx, request("selection.record", "", f.selectIn([]string{rev1}, nil)))
	if !apierr.HasCode(err, "cross_revision_evidence") || apierr.As(err).Details["evidence_manifest"] != oldProposal["manifest"] {
		t.Fatalf("cross revision: %v", err)
	}
	if len(f.selections(t)) != 0 {
		t.Fatal("a rejected selection left a record")
	}

	// A caller who bypasses the API cannot publish a fabricated decision:
	// the index re-derives every status from the recorded evidence set.
	rev2 := call(t, s, request("evidence.run", "", f.input(nil)))["attestation"].(string)
	root, err := s.root()
	if err != nil {
		t.Fatal(err)
	}
	reviewer := sessionOfKind(t, s, "agent")
	forgeries := map[string]struct {
		signer identity.Signer
		edit   func(*schema.SelectionDecision)
		code   string
	}{
		"forged satisfied": {root, func(d *schema.SelectionDecision) {
			for i := range d.Criteria {
				if d.Criteria[i].CriterionID == "review" {
					d.Criteria[i].Status, d.Criteria[i].Deciding = "satisfied", []string{rev2}
				}
			}
		}, "status_mismatch"},
		"forged deciding":  {root, func(d *schema.SelectionDecision) { d.Criteria[0].Deciding = []string{} }, "status_mismatch"},
		"dropped contrary": {root, func(d *schema.SelectionDecision) { d.ContraryEvidence = []string{rev2} }, "status_mismatch"},
		"cross revision":   {root, func(d *schema.SelectionDecision) { d.Evidence = []string{rev1} }, "cross_revision_evidence"},
		"unknown evidence": {root, func(d *schema.SelectionDecision) { d.Evidence = []string{strings.Repeat("ef", 32)} }, "unknown_evidence"},
		"stale manifest": {root, func(d *schema.SelectionDecision) {
			d.Manifest = oldProposal["manifest"].(string)
		}, "stale_revision"},
		"wrong intent":     {root, func(d *schema.SelectionDecision) { d.IntentID = foreign.intent["intent_id"].(string) }, "invalid_selection"},
		"selector forgery": {root, func(d *schema.SelectionDecision) { d.Selector = schema.Selector{Kind: "session", ID: reviewer} }, "unauthorized"},
		"unsorted":         {root, func(d *schema.SelectionDecision) { d.Evidence = []string{rev2, strings.Repeat("0", 64)} }, "invalid_selection"},
		"author signed": {s.signers[f.sid], func(d *schema.SelectionDecision) {
			d.Selector = schema.Selector{Kind: "session", ID: f.sid}
		}, "unauthorized"},
		"self competitor": {root, func(d *schema.SelectionDecision) { d.Competitors = []string{d.Manifest} }, "invalid_selection"},
	}
	for name, c := range forgeries {
		t.Run(name, func(t *testing.T) {
			if err := forgeSelection(t, s, f, c.signer, c.edit); !apierr.HasCode(err, c.code) {
				t.Fatalf("got %v, want %s", err, c.code)
			}
		})
	}
	if len(f.selections(t)) != 0 {
		t.Fatal("a forged decision was published")
	}
	// The genuine decision publishes through the same validator.
	if err := forgeSelection(t, s, f, root, func(*schema.SelectionDecision) {}); err != nil {
		t.Fatalf("genuine decision: %v", err)
	}

	// A closed intent has nothing left to select.
	fix := newRunFix(t, s, "true", nil)
	call(t, s, request("intent.close", "", map[string]any{"intent_id": fix.intent["intent_id"], "reason": "abandoned"}))
	if _, err := s.Handle(ctx, request("selection.record", "", fix.selectIn([]string{}, nil))); !apierr.HasCode(err, "invalid_state") {
		t.Fatalf("closed intent: %v", err)
	}
}

func TestAC113_SelectionKeepsContraryEvidence(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	pass := call(t, s, request("evidence.run", "", f.input(nil)))["attestation"].(string)
	third := sessionOfKind(t, s, "service")
	fail := forgeEvidence(t, s, f, third, func(a *schema.EvidenceAttestation) {
		one, reason := int64(1), "check_failed"
		a.Result, a.Reason, a.Check = "fail", &reason, &schema.Phase{ExitStatus: &one}
	})

	// The caller relies only on the passing run; the failure is still listed.
	out := call(t, s, request("selection.record", "", f.selectIn([]string{pass}, nil)))
	if !slices.Equal(hashesOf(out["contrary_evidence"]), []string{fail}) {
		t.Fatalf("contrary evidence: %v", out["contrary_evidence"])
	}
	d := f.decision(t, out["selection"].(string))
	if !slices.Equal(d.Evidence, []string{pass}) || !slices.Equal(d.ContraryEvidence, []string{fail}) {
		t.Fatalf("decision: %+v", d)
	}
	if d.Criteria[0].Status != "satisfied" {
		t.Fatalf("the relied-upon set decides the status, contrary evidence stays visible: %+v", d.Criteria)
	}
	// Relying on both records the conflict.
	both := call(t, s, request("selection.record", "", f.selectIn([]string{pass, fail}, nil)))
	for _, c := range both["criteria"].([]any) {
		c := c.(map[string]any)
		if c["criterion_id"] == "tests" && (c["status"] != "failed" || !slices.Equal(hashesOf(c["conflicting"]), []string{pass})) {
			t.Fatalf("mixed evidence: %v", c)
		}
	}
}

func TestAC210_SelectedWinnerVerifiesSeparately(t *testing.T) {
	// Core-level chain: the manifest binds the tree, evidence binds the
	// manifest and tree, the selection binds the evidence. Each link is
	// checkable on its own, and the winner stays a manifest: no line moves.
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	ctx := context.Background()
	before := f.line(t)
	att := call(t, s, request("evidence.run", "", f.input(nil)))["attestation"].(string)
	sel := call(t, s, request("selection.record", "", f.selectIn([]string{att}, nil)))["selection"].(string)

	m, err := s.Store.Index.Manifest(ctx, f.proposal["manifest"].(string))
	if err != nil || m.Tree != f.proposal["tree"] {
		t.Fatalf("the manifest binds the proposed tree: %+v %v", m, err)
	}
	if after := f.line(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("selecting the winner moved line main: %+v -> %+v", before, after)
	}
	a := f.attestation(t, att)
	d := f.decision(t, sel)
	if a.Manifest != d.Manifest || a.Tree != m.Tree || d.Manifest != f.proposal["manifest"] || !slices.Contains(d.Evidence, att) {
		t.Fatalf("chain: attestation %+v decision %+v", a, d)
	}
	// The decision re-derives from the stored records alone.
	_, sb, err := s.criteriaBinding(ctx, d.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	set, err := s.Store.Index.EvidenceByHash(ctx, d.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.SameStatuses(selection.Evaluate(sb, set, set).Criteria, d.Criteria) {
		t.Fatal("recorded statuses differ from a fresh evaluation")
	}
}

func TestSelectionPublication(t *testing.T) {
	// Failure immediately before and after every record store boundary of a
	// selection. After a restart the same operation id either replays one
	// committed decision or commits exactly one; rebuilding the index from
	// the record files changes nothing.
	stages := []string{record.StageJournal, record.StageEffects, record.StageObjects, record.StageReceipt, record.StageLog, record.StageIndex, record.StageCleanup, record.StageDone}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			armed := false
			s := fixtureWith(t, record.Options{Fault: func(st string) error {
				if armed && st == stage {
					armed = false
					return errCrash
				}
				return nil
			}})
			f := newRunFix(t, s, "true", nil)
			att := call(t, s, request("evidence.run", "", f.input(nil)))["attestation"].(string)
			ctx := context.Background()
			before := f.line(t)
			req := request("selection.record", "", f.selectIn([]string{att}, nil))
			armed = true
			if _, err := s.Handle(ctx, req); !errors.Is(err, errCrash) {
				t.Fatalf("expected the injected crash, got %v", err)
			}

			restarted, err := New(ctx, s.Store, s.Keys)
			if err != nil {
				t.Fatal(err)
			}
			f.s = restarted
			result, err := restarted.Handle(ctx, req)
			if err != nil {
				t.Fatalf("retry with the original operation id: %v", err)
			}
			wire, _ := canon.Marshal(result)
			got := f.selections(t)
			if len(got) != 1 {
				t.Fatalf("exactly one decision must exist, got %v", got)
			}
			if !strings.Contains(string(wire), got[0]) {
				t.Fatalf("retry result %s does not name the committed decision %s", wire, got[0])
			}
			d := f.decision(t, got[0])
			if d.Manifest != f.proposal["manifest"] || !slices.Equal(d.Evidence, []string{att}) {
				t.Fatalf("decision: %+v", d)
			}
			// A second retry replays; nothing new is created.
			replay, err := restarted.Handle(ctx, req)
			if err != nil || !replay.(*record.Result).Replayed {
				t.Fatalf("second retry: %v", err)
			}
			if after := f.line(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("recovery moved line main: %+v -> %+v", before, after)
			}
			if _, err := restarted.Store.Reindex(ctx); err != nil {
				t.Fatal(err)
			}
			if again := f.selections(t); !slices.Equal(again, got) {
				t.Fatalf("reindex changed the selections: %v -> %v", got, again)
			}
		})
	}
}
