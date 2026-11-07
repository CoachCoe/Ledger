package core

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/evidence"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
)

var errCrash = errors.New("simulated crash")

// runFix is a proposed change whose intent pins one mechanical check.
type runFix struct {
	s        *Service
	sid      string
	cap      string
	intent   map[string]any
	wid, dir string
	proposal map[string]any
	def      string // pinned definition hash
}

func newRunFix(t *testing.T, s *Service, command string, mod func(*schema.CheckDefinition)) *runFix {
	t.Helper()
	d := unitCheck()
	d.Command = command
	if mod != nil {
		mod(&d)
	}
	writeChecks(t, s, []schema.CheckDefinition{d})
	session := call(t, s, request("session.create", "", map[string]any{"agent_name": "author"}))
	f := &runFix{s: s, sid: session["session_id"].(string), cap: session["capability_id"].(string)}
	f.intent = call(t, s, request("intent.create", "", checkIntent()))
	ws := call(t, s, request("workspace.fork", f.sid, map[string]any{"intent_id": f.intent["intent_id"], "capability_id": f.cap}))
	f.wid, f.dir = ws["workspace_id"].(string), ws["path"].(string)
	f.propose(t, "export const x=2;\n")
	return f
}

func (f *runFix) propose(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "a.ts"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.proposal = call(t, f.s, request("workspace.propose", f.sid, map[string]any{"workspace_id": f.wid}))
	bound := call(t, f.s, request("evidence.resolve", "", map[string]any{"manifest": f.proposal["manifest"]}))
	f.def = bound["checks"].([]any)[0].(map[string]any)["definition_hash"].(string)
}

func (f *runFix) input(extra map[string]any) map[string]any {
	in := map[string]any{"manifest": f.proposal["manifest"], "commit": f.proposal["commit"], "check_definition": f.def}
	for k, v := range extra {
		in[k] = v
	}
	return in
}

func (f *runFix) attestation(t *testing.T, hash string) schema.EvidenceAttestation {
	t.Helper()
	o, err := f.s.Store.Read(hash)
	if err != nil {
		t.Fatal(err)
	}
	var a schema.EvidenceAttestation
	if err := o.Decode(&a); err != nil {
		t.Fatal(err)
	}
	if o.Envelope.Signer != a.VerifierSession {
		t.Fatalf("signer %s is not the attested verifier %s", o.Envelope.Signer, a.VerifierSession)
	}
	return a
}

func (f *runFix) attestations(t *testing.T) []string {
	t.Helper()
	hashes, err := f.s.Store.Index.AttestationsFor(context.Background(), f.proposal["manifest"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return hashes
}

func (f *runFix) sessionState(t *testing.T, id string) string {
	t.Helper()
	sub, err := f.s.Store.Index.Subject(context.Background(), id)
	if err != nil || sub == nil {
		t.Fatalf("session %s: %v", id, err)
	}
	return sub.State
}

func (f *runFix) verifyDirEmpty(t *testing.T) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(f.s.Store.Dir, "runtime", "verify"))
	if len(entries) != 0 {
		t.Fatalf("verification worktrees remain: %v", entries)
	}
}

func TestAC167_168_058_RunSignsAttestation(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "echo verified", nil)
	req := request("evidence.run", "", f.input(nil))
	res, err := s.Handle(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	out := call(t, s, req) // a retry replays the frozen result
	if out["attestation"] == nil || out["result"] != "pass" || out["reason"] != nil || out["kept_worktree"] != nil {
		t.Fatalf("result: %v", out)
	}
	if rec, ok := res.(*record.Result); !ok || rec.Replayed || len(rec.Members) != 1 {
		t.Fatalf("first run: %#v", res)
	}
	replay, err := s.Handle(context.Background(), req)
	if err != nil || !replay.(*record.Result).Replayed {
		t.Fatalf("retry should replay: %v %v", replay, err)
	}
	a := f.attestation(t, out["attestation"].(string))
	if a.Manifest != f.proposal["manifest"] || a.Commit != f.proposal["commit"] || a.ExecutedTree != f.proposal["tree"] || a.CheckDefinition != f.def {
		t.Fatalf("binding: %+v", a)
	}
	if a.Category != "mechanical" || a.Command != "echo verified" || len(a.Criteria) != 2 || a.Criteria[0] != "lint" || a.Criteria[1] != "tests" {
		t.Fatalf("definition: %+v", a)
	}
	if a.Result != "pass" || a.Isolation != "none" || a.VerifierSession != out["verifier_session"] || a.Check == nil || a.Setup != nil {
		t.Fatalf("run: %+v", a)
	}
	if a.OutputBytes != int64(len("verified\n")) || a.OutputDigest == "" || a.EnvironmentHash == "" || a.Environment.OS == "" {
		t.Fatalf("output/environment: %+v", a)
	}
	// Exactly one run, one attestation, and the verifier session is over.
	if got := f.attestations(t); len(got) != 1 || got[0] != out["attestation"] {
		t.Fatalf("attestations %v", got)
	}
	if st := f.sessionState(t, a.VerifierSession); st != "ended" {
		t.Fatalf("verifier session is %s", st)
	}
	if _, held := s.signers[a.VerifierSession]; held {
		t.Fatal("verifier key still held")
	}
	kind, raw, err := s.Store.Index.Creation(context.Background(), a.VerifierSession)
	if err != nil || kind != schema.KindSession {
		t.Fatal(kind, err)
	}
	var vs schema.Session
	if err := json.Unmarshal(raw, &vs); err != nil || vs.ParticipantKind != "service" {
		t.Fatalf("verifier session %+v %v", vs, err)
	}
	f.verifyDirEmpty(t)
	status := call(t, s, request("system.status", "", map[string]any{}))
	if runs, _ := status["interrupted_runs"].([]any); len(runs) != 0 {
		t.Fatalf("interrupted: %v", runs)
	}
}

func TestEvidenceRunPrechecks(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	ctx := context.Background()
	other := strings.Repeat("c", 64)
	expectCode(t, s, "evidence.run", "", f.input(map[string]any{"check_definition": other}), "check_not_pinned")
	expectCode(t, s, "evidence.run", "", f.input(map[string]any{"commit": strings.Repeat("1", 40)}), "binding_mismatch")
	expectCode(t, s, "evidence.run", "", f.input(map[string]any{"manifest": other}), "unknown_manifest")
	for _, in := range []map[string]any{
		{"manifest": f.proposal["manifest"], "commit": f.proposal["commit"]},
		{"manifest": f.proposal["manifest"], "check_definition": f.def},
		f.input(map[string]any{"extra": 1}),
		f.input(map[string]any{"keep_worktree": "sometimes"}),
		f.input(map[string]any{"verifier_session": f.sid}),
	} {
		if _, err := s.Handle(ctx, request("evidence.run", "", in)); !apierr.HasCode(err, "schema_invalid") {
			t.Fatalf("input %v: %v", in, err)
		}
	}
	// A rework creates a new revision: the old commit does not bind to it.
	call(t, s, request("workspace.rework", f.sid, map[string]any{"workspace_id": f.wid}))
	old := f.proposal
	f.propose(t, "export const x=3;\n")
	expectCode(t, s, "evidence.run", "", map[string]any{"manifest": f.proposal["manifest"], "commit": old["commit"], "check_definition": f.def}, "binding_mismatch")
	if got := f.attestations(t); len(got) != 0 {
		t.Fatalf("a refused run recorded evidence: %v", got)
	}
	// A judgment-category definition is pinned but never run.
	s2 := fixture(t)
	jf := newRunFix(t, s2, "true", func(d *schema.CheckDefinition) { d.Category = "judgment" })
	expectCode(t, s2, "evidence.run", "", jf.input(nil), "not_runnable")
	// Without the root key nothing starts: no verifier can be provisioned.
	noRoot, err := New(ctx, s.Store, unavailableKeys{})
	if err != nil {
		t.Fatal(err)
	}
	expectCode(t, noRoot, "evidence.run", "", f.input(nil), "root_unavailable")
}

func TestAC092_176_VerifierNotAuthor(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	ctx := context.Background()
	// The author and a rebound holder cannot even request the run.
	expectCode(t, s, "evidence.run", f.sid, f.input(nil), "unauthorized")
	holder := call(t, s, request("session.recover", "", map[string]any{"workspace_id": f.wid}))
	holderSID := holder["session_id"].(string)
	expectCode(t, s, "evidence.run", holderSID, f.input(nil), "unauthorized")

	// An unrelated session may request it; the signer is still a fresh
	// root-authenticated service session, never the requester.
	bystander := call(t, s, request("session.create", "", map[string]any{"agent_name": "reviewer"}))
	out := call(t, s, request("evidence.run", bystander["session_id"].(string), f.input(nil)))
	a := f.attestation(t, out["attestation"].(string))
	if a.VerifierSession == f.sid || a.VerifierSession == holderSID || a.VerifierSession == bystander["session_id"] {
		t.Fatalf("verifier %s is not distinct", a.VerifierSession)
	}

	// Attestations signed by the author or any holder are refused at publish.
	genuine := a
	forge := func(sid string, edit func(*schema.EvidenceAttestation)) error {
		att := genuine
		att.VerifierSession = sid
		if edit != nil {
			edit(&att)
		}
		key := s.signers[sid]
		o, err := envelope.Seal(schema.KindEvidenceAttestation, att, key)
		if err != nil {
			return err
		}
		_, err = s.Store.Do(ctx, record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "test.forge", Digest: canon.Hash([]byte(o.Hash)), Signer: key}, func(*index.Index) (any, []envelope.Object, error) {
			return map[string]any{}, []envelope.Object{o}, nil
		})
		return err
	}
	if err := forge(f.sid, nil); !apierr.HasCode(err, "author_signed_evidence") {
		t.Fatalf("author-signed: %v", err)
	}
	if err := forge(holderSID, nil); !apierr.HasCode(err, "author_signed_evidence") {
		t.Fatalf("holder-signed: %v", err)
	}
	if got := f.attestations(t); len(got) != 1 {
		t.Fatalf("refused attestations were stored: %v", got)
	}

	// Other published-shape violations from an otherwise acceptable signer.
	third := call(t, s, request("session.create", "", map[string]any{"agent_name": "third"}))["session_id"].(string)
	one := int64(1)
	cases := map[string]struct {
		edit func(*schema.EvidenceAttestation)
		code string
	}{
		"other commit":           {func(a *schema.EvidenceAttestation) { a.Commit = strings.Repeat("2", 40) }, "binding_mismatch"},
		"other tree":             {func(a *schema.EvidenceAttestation) { a.ExecutedTree = strings.Repeat("3", 40) }, "tree_mismatch"},
		"unknown definition":     {func(a *schema.EvidenceAttestation) { a.CheckDefinition = strings.Repeat("4", 64) }, "unknown_entity"},
		"unknown criterion":      {func(a *schema.EvidenceAttestation) { a.Criteria = []string{"nope"} }, "invalid_attestation"},
		"unpinned criterion":     {func(a *schema.EvidenceAttestation) { a.Criteria = []string{"review"} }, "invalid_attestation"},
		"category differs":       {func(a *schema.EvidenceAttestation) { a.Category = "behavioral" }, "invalid_attestation"},
		"pass with failed check": {func(a *schema.EvidenceAttestation) { a.Check = &schema.Phase{ExitStatus: &one} }, "inconsistent_result"},
		"pass without check":     {func(a *schema.EvidenceAttestation) { a.Check = nil }, "inconsistent_result"},
		"pass timed out":         {func(a *schema.EvidenceAttestation) { a.Check = &schema.Phase{ExitStatus: new(int64), TimedOut: true} }, "inconsistent_result"},
		"pass with failed setup": {func(a *schema.EvidenceAttestation) { a.Setup = &schema.Phase{ExitStatus: &one} }, "inconsistent_result"},
		"pass with reason":       {func(a *schema.EvidenceAttestation) { r := "check_failed"; a.Reason = &r }, "inconsistent_result"},
		"fail without reason":    {func(a *schema.EvidenceAttestation) { a.Result = "fail" }, "inconsistent_result"},
		"other manifest":         {func(a *schema.EvidenceAttestation) { a.Manifest = strings.Repeat("5", 64) }, "unknown_manifest"},
	}
	for name, c := range cases {
		if err := forge(third, c.edit); !apierr.HasCode(err, c.code) {
			t.Fatalf("%s: want %s, got %v", name, c.code, err)
		}
	}
	if got := f.attestations(t); len(got) != 1 {
		t.Fatalf("refused attestations were stored: %v", got)
	}
}

func TestAC174_175_230_LaterEvidenceAndEditedWorkspace(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "grep -q 'x=2' a.ts && test \"$(cat a.ts)\" = 'export const x=2;'", nil)
	ctx := context.Background()
	refs := func() string {
		out, err := gitx.Run(ctx, s.Store.Repo.Dir, nil, nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/ledger/proposals")
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	manifestPath := s.Store.ObjectPath(f.proposal["manifest"].(string))
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	before := refs()
	// The author is still editing; the run must see the proposed content.
	if err := os.WriteFile(filepath.Join(f.dir, "a.ts"), []byte("export const x=9;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := call(t, s, request("evidence.run", "", f.input(nil)))
	if out["result"] != "pass" {
		t.Fatalf("run saw the edited workspace: %v", out)
	}
	a := f.attestation(t, out["attestation"].(string))
	if a.Commit != f.proposal["commit"] || a.ExecutedTree != f.proposal["tree"] {
		t.Fatalf("attestation must name the bound commit and tree: %+v", a)
	}
	if after := refs(); after != before {
		t.Fatalf("evidence moved a ref or commit:\n%s\n%s", before, after)
	}
	if now, _ := os.ReadFile(manifestPath); string(now) != string(manifestBytes) {
		t.Fatal("manifest object changed")
	}
	if body, _ := os.ReadFile(filepath.Join(f.dir, "a.ts")); string(body) != "export const x=9;\n" {
		t.Fatalf("author workspace was touched: %q", body)
	}
	if head, err := gitx.Run(ctx, s.Store.Repo.Dir, nil, nil, "rev-parse", f.proposal["branch"].(string)); err != nil || strings.TrimSpace(string(head)) != f.proposal["commit"] {
		t.Fatalf("branch no longer at the bound commit: %s %v", head, err)
	}
}

func TestAC004_056_ChangeRecordCarriesEvidence(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	ctx := context.Background()
	if got := f.attestations(t); len(got) != 0 {
		t.Fatalf("evidence before any run: %v", got)
	}
	first := call(t, s, request("evidence.run", "", f.input(nil)))
	second := call(t, s, request("evidence.run", "", f.input(nil)))
	hashes := f.attestations(t)
	if len(hashes) != 2 {
		t.Fatalf("evidence list: %v", hashes)
	}
	manifest, err := s.Store.Index.Manifest(ctx, f.proposal["manifest"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.IntentID != f.intent["intent_id"] || manifest.AuthorSession != f.sid {
		t.Fatalf("manifest %+v", manifest)
	}
	want := map[string]bool{first["attestation"].(string): true, second["attestation"].(string): true}
	for _, h := range hashes {
		if !want[h] || f.attestation(t, h).Manifest != f.proposal["manifest"] {
			t.Fatalf("attestation %s does not reference the manifest", h)
		}
	}
}

func TestAC209_CleanCheckoutIndependentVerifier(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "pwd -P; git rev-parse HEAD; git status --porcelain --untracked-files=no", nil)
	out := call(t, s, request("evidence.run", "", f.input(map[string]any{"keep_worktree": "always"})))
	kept, _ := out["kept_worktree"].(string)
	runID := out["run_id"].(string)
	if kept == "" || !strings.HasSuffix(kept, filepath.Join("runtime", "verify", runID)) {
		t.Fatalf("kept worktree %q", kept)
	}
	raw, err := os.ReadFile(evidence.OutputPath(filepath.Join(s.Store.Dir, "runtime"), runID))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	realKept, _ := filepath.EvalSymlinks(kept)
	if len(lines) != 2 || lines[0] != realKept || lines[1] != f.proposal["commit"] {
		t.Fatalf("check ran in %v, want %s at %s", lines, realKept, f.proposal["commit"])
	}
	if strings.HasPrefix(lines[0], f.dir) {
		t.Fatal("check ran in the author's workspace")
	}
	if a := f.attestation(t, out["attestation"].(string)); a.VerifierSession == f.sid {
		t.Fatal("verifier is the author")
	}
	if err := s.Store.Repo.WorktreeRemove(context.Background(), kept); err != nil {
		t.Fatal(err)
	}
	f.verifyDirEmpty(t)
}

func TestEvidenceCancellationPublishesFailure(t *testing.T) {
	s := fixture(t)
	started := filepath.Join(t.TempDir(), "started")
	f := newRunFix(t, s, "touch "+started+"; sleep 300", nil)
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	go func() {
		for range 400 {
			if _, err := os.Stat(started); err == nil {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		once.Do(cancel)
	}()
	res, err := s.Handle(ctx, request("evidence.run", "", f.input(nil)))
	if err != nil {
		t.Fatal(err)
	}
	var inner struct {
		Result map[string]any `json:"result"`
	}
	raw, _ := canon.Marshal(res)
	if err := json.Unmarshal(raw, &inner); err != nil {
		t.Fatal(err)
	}
	if inner.Result["result"] != "fail" || inner.Result["reason"] != evidence.ReasonCancelled {
		t.Fatalf("cancelled run: %v", inner.Result)
	}
	a := f.attestation(t, inner.Result["attestation"].(string))
	if a.Check == nil || a.Check.ExitStatus != nil || a.Check.TimedOut {
		t.Fatalf("check phase %+v", a.Check)
	}
	if st := f.sessionState(t, a.VerifierSession); st != "ended" {
		t.Fatalf("verifier is %s", st)
	}
	f.verifyDirEmpty(t)
}

func TestEvidenceFailedRunsPublishFailedAttestations(t *testing.T) {
	cases := []struct {
		name, command, setup, reason string
		timeout                      int64
	}{
		{"nonzero", "exit 4", "", evidence.ReasonCheckFailed, 0},
		{"timeout", "sleep 300", "", evidence.ReasonCheckTimeout, 1},
		{"setup failure", "true", "exit 9", evidence.ReasonSetupFailed, 0},
		{"setup timeout", "true", "sleep 300", evidence.ReasonSetupTimeout, 1},
		{"tracked mutation by check", "echo x >> a.ts", "", evidence.ReasonTrackedMutation, 0},
		{"tracked mutation by setup", "true", "rm a.ts", evidence.ReasonTrackedMutation, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := fixture(t)
			f := newRunFix(t, s, c.command, func(d *schema.CheckDefinition) {
				if c.setup != "" {
					d.Setup = &c.setup
				}
				if c.timeout != 0 {
					d.TimeoutSeconds = c.timeout
				}
			})
			out := call(t, s, request("evidence.run", "", f.input(nil)))
			if out["result"] != "fail" || out["reason"] != c.reason {
				t.Fatalf("result: %v", out)
			}
			a := f.attestation(t, out["attestation"].(string))
			if a.Result != "fail" || a.Reason == nil || *a.Reason != c.reason || a.Isolation != "none" || a.ExecutedTree != f.proposal["tree"] {
				t.Fatalf("attestation: %+v", a)
			}
			if st := f.sessionState(t, a.VerifierSession); st != "ended" {
				t.Fatalf("verifier is %s", st)
			}
			f.verifyDirEmpty(t)
		})
	}
}

func TestEvidenceAbortProducesNoAttestation(t *testing.T) {
	// A checkout that fails verification aborts before anything runs: no
	// attestation, journal aborted, verifier session ended, nothing left over.
	s := fixture(t)
	f := newRunFix(t, s, "echo must-not-run", nil)
	s.evidenceHooks = &evidence.Hooks{AfterCheckout: func(path string) {
		_ = os.WriteFile(filepath.Join(path, "a.ts"), []byte("tampered\n"), 0o600)
	}}
	req := request("evidence.run", "", f.input(nil))
	if _, err := s.Handle(context.Background(), req); !apierr.HasCode(err, "dirty_checkout") {
		t.Fatalf("dirty checkout: %v", err)
	}
	if got := f.attestations(t); len(got) != 0 {
		t.Fatalf("attestation after abort: %v", got)
	}
	journals, err := evidence.ListJournals(filepath.Join(s.Store.Dir, "runtime"))
	if err != nil || len(journals) != 1 || journals[0].State != evidence.StateAborted || journals[0].Reason != "dirty_checkout" {
		t.Fatalf("journals %+v %v", journals, err)
	}
	if st := f.sessionState(t, journals[0].VerifierSession); st != "ended" {
		t.Fatalf("verifier is %s", st)
	}
	f.verifyDirEmpty(t)
	// The same operation id may retry once the cause is gone.
	s.evidenceHooks = nil
	if _, err := s.Handle(context.Background(), req); err != nil {
		t.Fatalf("retry after abort: %v", err)
	}
}

func TestEvidencePublication(t *testing.T) {
	// Interruption before and after every publication boundary of an
	// attestation, and verifier key loss. After a restart the result is
	// either one committed attestation (replayed on retry) or an explicit
	// interrupted run; a passing attestation is never fabricated.
	type crash struct {
		name      string
		storeStep string // record store stage to fail, "" for none
		hookStep  string // evidence stage to fail
		committed bool   // the group survives the crash and recovery completes it
	}
	cases := []crash{
		{"before_sign", "", evidence.StageBeforeSign, false},
		{"after_sign", "", evidence.StageAfterSign, false},
		{"journal", record.StageJournal, "", false},
	}
	for _, stage := range []string{record.StageEffects, record.StageObjects, record.StageReceipt, record.StageRef, record.StageRefCAS, record.StageIndex, record.StageCleanup, record.StageDone} {
		cases = append(cases, crash{stage, stage, "", true})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			armed := false
			s := fixtureWith(t, record.Options{Fault: func(stage string) error {
				if armed && stage == c.storeStep {
					armed = false
					return errCrash
				}
				return nil
			}})
			f := newRunFix(t, s, "true", nil)
			s.evidenceHooks = &evidence.Hooks{Fault: func(stage string) error {
				if stage == evidence.StageAfterSign && c.storeStep != "" {
					armed = true
				}
				if c.hookStep != "" && stage == c.hookStep {
					return errCrash
				}
				return nil
			}}
			ctx := context.Background()
			req := request("evidence.run", "", f.input(nil))
			if _, err := s.Handle(ctx, req); !errors.Is(err, errCrash) {
				t.Fatalf("expected the injected crash, got %v", err)
			}

			restarted, err := New(ctx, s.Store, s.Keys)
			if err != nil {
				t.Fatal(err)
			}
			f.s = restarted
			status := call(t, restarted, request("system.status", "", map[string]any{}))
			runs, _ := status["interrupted_runs"].([]any)
			hashes := f.attestations(t)
			if c.committed {
				if len(hashes) != 1 || len(runs) != 0 {
					t.Fatalf("recovery should complete the group: attestations %v interrupted %v", hashes, runs)
				}
				replay, err := restarted.Handle(ctx, req)
				if err != nil || !replay.(*record.Result).Replayed {
					t.Fatalf("retry should replay the committed result: %v", err)
				}
				var inner struct {
					Result map[string]any `json:"result"`
				}
				raw, _ := canon.Marshal(replay)
				if err := json.Unmarshal(raw, &inner); err != nil || inner.Result["attestation"] != hashes[0] {
					t.Fatalf("replayed result %v %v", inner.Result, err)
				}
				if a := f.attestation(t, hashes[0]); a.Result != "pass" {
					t.Fatalf("recovered attestation: %+v", a)
				}
			} else {
				if len(hashes) != 0 {
					t.Fatalf("a passing attestation appeared without a publication: %v", hashes)
				}
				if len(runs) != 1 || runs[0].(map[string]any)["manifest"] != f.proposal["manifest"] {
					t.Fatalf("the interrupted run must be discoverable: %v", status["interrupted_runs"])
				}
				// Retrying the same operation id reports the interruption.
				_, err := restarted.Handle(ctx, req)
				if !apierr.HasCode(err, "run_interrupted") {
					t.Fatalf("retry: %v", err)
				}
				if d := apierr.As(err).Details; d["run_id"] != runs[0].(map[string]any)["run_id"] || d["commit"] != f.proposal["commit"] {
					t.Fatalf("details %v", d)
				}
				// A new operation id starts a fresh run.
				fresh := call(t, restarted, request("evidence.run", "", f.input(nil)))
				if fresh["result"] != "pass" || fresh["run_id"] == runs[0].(map[string]any)["run_id"] {
					t.Fatalf("fresh run: %v", fresh)
				}
				if got := f.attestations(t); len(got) != 1 || got[0] != fresh["attestation"] {
					t.Fatalf("attestations %v", got)
				}
			}
			f.verifyDirEmpty(t)
			// Every verifier session is over, whether the run finished or not.
			active, err := restarted.Store.Index.SubjectsIn(ctx, "session", "active")
			if err != nil {
				t.Fatal(err)
			}
			if len(active) != 0 {
				t.Fatalf("sessions still active after restart: %v", active)
			}
		})
	}
}

func TestEvidenceVerifierKeyLoss(t *testing.T) {
	s := fixture(t)
	f := newRunFix(t, s, "true", nil)
	ctx := context.Background()
	s.evidenceHooks = &evidence.Hooks{Fault: func(stage string) error {
		if stage == evidence.StageExecuted {
			// The verifier's key disappears after the command completed.
			s.mu.Lock()
			for id := range s.signers {
				if id != f.sid {
					delete(s.signers, id)
				}
			}
			s.mu.Unlock()
		}
		return nil
	}}
	req := request("evidence.run", "", f.input(nil))
	_, err := s.Handle(ctx, req)
	if !apierr.HasCode(err, "run_interrupted") {
		t.Fatalf("lost key: %v", err)
	}
	runID, _ := apierr.As(err).Details["run_id"].(string)
	if apierr.As(err).Details["reason"] != "verifier_key_lost" || runID == "" {
		t.Fatalf("details %v", apierr.As(err).Details)
	}
	if got := f.attestations(t); len(got) != 0 {
		t.Fatalf("evidence fabricated without a key: %v", got)
	}
	status := call(t, s, request("system.status", "", map[string]any{}))
	runs, _ := status["interrupted_runs"].([]any)
	if len(runs) != 1 || runs[0].(map[string]any)["run_id"] != runID || runs[0].(map[string]any)["reason"] != "verifier_key_lost" {
		t.Fatalf("status %v", status["interrupted_runs"])
	}
	if _, err := s.Handle(ctx, req); !apierr.HasCode(err, "run_interrupted") {
		t.Fatalf("retry with the same id: %v", err)
	}
	s.evidenceHooks = nil
	fresh := call(t, s, request("evidence.run", "", f.input(nil)))
	if fresh["result"] != "pass" || fresh["run_id"] == runID {
		t.Fatalf("fresh run: %v", fresh)
	}
	if got := f.attestations(t); len(got) != 1 || got[0] != fresh["attestation"] {
		t.Fatalf("attestations %v", got)
	}
	// After a restart the orphaned verifier session is ended too.
	restarted, err := New(ctx, s.Store, s.Keys)
	if err != nil {
		t.Fatal(err)
	}
	active, _ := restarted.Store.Index.SubjectsIn(ctx, "session", "active")
	for _, a := range active {
		if a.ID != f.sid {
			t.Fatalf("verifier session %s survived the restart", a.ID)
		}
	}
}

func TestEvidenceRunDoesNotBlockOtherRequests(t *testing.T) {
	// The service lock is not held while the child runs.
	s := fixture(t)
	started := filepath.Join(t.TempDir(), "started")
	f := newRunFix(t, s, "touch "+started+"; sleep 3", nil)
	done := make(chan error, 1)
	go func() {
		_, err := s.Handle(context.Background(), request("evidence.run", "", f.input(nil)))
		done <- err
	}()
	for range 400 {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	begin := time.Now()
	call(t, s, request("session.create", "", map[string]any{"agent_name": "concurrent"}))
	if took := time.Since(begin); took > 2*time.Second {
		t.Fatalf("a mutation waited %s for the running check", took)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
