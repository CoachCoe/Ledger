package core

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/cow"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/oplog"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/store"
	"github.com/CoachCoe/Ledger/internal/symbols"
	"github.com/CoachCoe/Ledger/internal/workspace"
)

// nativeFixture is a native repository whose initial tree holds a.ts.
func nativeFixture(t *testing.T) (*Service, string) {
	t.Helper()
	return nativeFixtureWith(t, nil)
}

// nativeFixtureWith adds files to the default workspace before its import.
func nativeFixtureWith(t *testing.T, files map[string]string) (*Service, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	files = maps.Clone(files)
	if files == nil {
		files = map[string]string{}
	}
	files["a.ts"] = "export const x=1;\n"
	files[".ledgerignore"] = "/" + record.DirName + "/\n" + files[".ledgerignore"]
	for p, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ledger := filepath.Join(dir, record.DirName)
	if err := os.Mkdir(ledger, 0700); err != nil {
		t.Fatal(err)
	}
	objects, err := store.Open(filepath.Join(ledger, "store", "objects"), true)
	if err != nil {
		t.Fatal(err)
	}
	m, err := cow.New(cow.Config{Store: objects, Runtime: filepath.Join(ledger, "runtime"), Logs: filepath.Join(ledger, "oplog"), CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := m.Import(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := identity.LocalKeys{Dir: filepath.Join(t.TempDir(), "keys")}
	root, _, err := keys.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	rs, err := record.OpenNative(ctx, dir, record.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	rk, err := envelope.SealVersion(2, schema.KindRootKey, schema.RootKey{PublicKey: identity.EncodePublicKey(root.PublicKey()), CreatedAt: "2026-11-05T12:00:00Z"}, root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := envelope.SealVersion(2, schema.KindRepository, schema.Repository{ID: schema.NewID(schema.PrefixRepository), HashAlgorithm: "sha256",
		CasePolicy: "sensitive", Analyzers: []string{}, Tree: tree, CreatedAt: "2026-11-05T12:00:00Z"}, root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rs.Do(ctx, record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "test.init", Digest: canon.Hash([]byte("init")), Signer: root},
		func(*index.Index) (any, []envelope.Object, error) {
			return map[string]any{}, []envelope.Object{rk, repo}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(ctx, rs, keys)
	if err != nil {
		t.Fatal(err)
	}
	return svc, tree
}

func requestV2(op, sid string, input any) api.Request {
	r := request(op, sid, input)
	r.Version = 2
	return r
}

func TestNativeForkClonesTheLineHead(t *testing.T) {
	s, tree := nativeFixture(t)
	ctx := context.Background()
	if _, err := s.Handle(ctx, request("intent.create", "", judgment("v1"))); !apierr.HasCode(err, "unsupported_api_version") {
		t.Fatalf("api_version 1 on a native repository: %v", err)
	}
	intent := call(t, s, requestV2("intent.create", "", judgment("native")))["intent_id"].(string)
	sess := call(t, s, requestV2("session.create", "", map[string]any{}))
	sid := sess["session_id"].(string)
	fork := call(t, s, requestV2("workspace.fork", sid, map[string]any{"intent_id": intent, "capability_id": sess["capability_id"]}))
	if fork["base_tree"] != tree {
		t.Fatalf("forked from %v, want %s", fork["base_tree"], tree)
	}
	got, err := os.ReadFile(filepath.Join(fork["path"].(string), "a.ts"))
	if err != nil || string(got) != "export const x=1;\n" {
		t.Fatalf("checkout %q, %v", got, err)
	}
	cr, err := s.Store.Index.Creation(ctx, fork["workspace_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var w schema.Workspace
	if err := cr.Decode(&w); err != nil || cr.Version != 2 || w.BaseTree != tree || w.BaseLineAdvance != nil || w.BaseCommit != "" {
		t.Fatalf("workspace record v%d %+v, %v", cr.Version, w, err)
	}
	for _, id := range []string{intent, sid, sess["capability_id"].(string)} {
		if cr, err := s.Store.Index.Creation(ctx, id); err != nil || cr.Version != 2 {
			t.Fatalf("%s written at version %d, %v", id, cr.Version, err)
		}
	}
	if _, err := s.Handle(ctx, requestV2("workspace.fork", sid, map[string]any{"intent_id": intent, "capability_id": sess["capability_id"], "base_tree": "e" + tree[1:]})); !apierr.HasCode(err, "base_unavailable") {
		t.Fatalf("fork from a missing tree: %v", err)
	}
}

// A native proposal pins the operation log entry that froze its tree and
// the symbol maps of that tree (P2-AC-013).
func TestNativeProposePinsTheOperationLog(t *testing.T) {
	s, base := nativeFixture(t)
	intent := call(t, s, requestV2("intent.create", "", judgment("native")))["intent_id"].(string)
	sess := call(t, s, requestV2("session.create", "", map[string]any{}))
	sid := sess["session_id"].(string)
	fork := call(t, s, requestV2("workspace.fork", sid, map[string]any{"intent_id": intent, "capability_id": sess["capability_id"]}))
	wid := fork["workspace_id"].(string)
	if err := os.WriteFile(filepath.Join(fork["path"].(string), "a.ts"), []byte("export function x() { return 2 }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prop := call(t, s, requestV2("workspace.propose", sid, map[string]any{"workspace_id": wid}))
	o, err := s.Store.Read(prop["manifest"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var m schema.ChangeManifest
	if err := o.Decode(&m); err != nil || o.Envelope.SchemaVersion != 2 {
		t.Fatalf("manifest v%d, %v", o.Envelope.SchemaVersion, err)
	}
	if m.BaseTree != base || m.Tree != prop["tree"] || m.Tree == base || m.OpHead != prop["op_head"] || len(m.Files) != 1 || m.Files[0] != "a.ts" {
		t.Fatalf("manifest %+v, proposal %v", m, prop)
	}
	entries, err := s.Provider.(workspace.NativeProvider).Workspaces.Log(wid)
	if err != nil {
		t.Fatal(err)
	}
	last := entries[len(entries)-1]
	if h, err := oplog.Hash(last); err != nil || h != m.OpHead || last.Kind != "propose" || last.Tree != m.Tree {
		t.Fatalf("op_head %s, last entry %+v (%s), %v", m.OpHead, last, h, err)
	}
	if len(m.SymbolsWritten) == 0 || m.SymbolsWritten[0].Revision == "" {
		t.Fatalf("written symbols %+v", m.SymbolsWritten)
	}
	id := m.SymbolMaps["a.ts"]
	if same, err := symbols.Recompute(s.Objects, id); err != nil || !same {
		t.Fatalf("symbol map %s: %v, %v", id, same, err)
	}
	status := call(t, s, requestV2("system.status", "", map[string]any{}))
	for _, sub := range status["subjects"].([]any) {
		if sub := sub.(map[string]any); sub["id"] == wid && sub["state"] != "proposed" {
			t.Fatalf("workspace %v", sub)
		}
	}
	why := call(t, s, requestV2("history.why", "", map[string]any{"symbol": map[string]any{"path": "a.ts", "language": "typescript", "name": "x", "kind": "function", "disambiguator": nil}}))
	if records := why["records"].([]any); len(records) != 1 || records[0].(map[string]any)["manifest"].(map[string]any)["hash"] != prop["manifest"] {
		t.Fatalf("history.why %v", why)
	}
}

// Closing removes a native checkout after recording its tree, and keeps a
// checkout that still holds ignored files.
func TestNativeCloseRecordsThenRemoves(t *testing.T) {
	s, _ := nativeFixture(t)
	ctx := context.Background()
	intent := call(t, s, requestV2("intent.create", "", judgment("close")))["intent_id"].(string)
	sess := call(t, s, requestV2("session.create", "", map[string]any{}))
	sid := sess["session_id"].(string)
	fork := func() (string, string) {
		f := call(t, s, requestV2("workspace.fork", sid, map[string]any{"intent_id": intent, "capability_id": sess["capability_id"]}))
		return f["workspace_id"].(string), f["path"].(string)
	}
	closeWS := func(id string) error {
		_, err := s.Handle(ctx, requestV2("workspace.close", sid, map[string]any{"workspace_id": id, "reason": "finished"}))
		return err
	}

	edited, dir := fork()
	if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("export const x=3;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := closeWS(edited); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("checkout kept: %v", err)
	}
	entries, err := s.Provider.(workspace.NativeProvider).Workspaces.Log(edited)
	if err != nil {
		t.Fatal(err)
	}
	last := entries[len(entries)-1]
	content, err := s.Objects.Files(last.Tree)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(content, func(f store.File) bool { return f.Path == "a.ts" })
	if i < 0 {
		t.Fatalf("closing tree %v has no a.ts", content)
	}
	var b bytes.Buffer
	if err := s.Objects.WriteFile(content[i].ID, &b); err != nil || b.String() != "export const x=3;\n" {
		t.Fatalf("closing tree lost the edit: %q, %v", b.String(), err)
	}

	kept, dir := fork()
	if err := os.WriteFile(filepath.Join(dir, ".ledgerignore"), []byte("/"+record.DirName+"/\nsecret.env\n"), 0600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret.env")
	if err := os.WriteFile(secret, []byte("preserve user data\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := closeWS(kept); !apierr.HasCode(err, "cleanup_pending") {
		t.Fatalf("close with an ignored file: %v", err)
	}
	if _, err := os.ReadFile(secret); err != nil {
		t.Fatal("ignored user data was deleted")
	}
	if st := call(t, s, requestV2("workspace.status", "", map[string]any{"workspace_id": kept})); st["state"] != "closed" {
		t.Fatal(st)
	}
}

// Status and API writes on a native workspace report against its base tree.
func TestNativeStatusAndWrite(t *testing.T) {
	s, base := nativeFixture(t)
	intent := call(t, s, requestV2("intent.create", "", judgment("write")))["intent_id"].(string)
	sess := call(t, s, requestV2("session.create", "", map[string]any{}))
	sid := sess["session_id"].(string)
	wid := call(t, s, requestV2("workspace.fork", sid, map[string]any{"intent_id": intent, "capability_id": sess["capability_id"]}))["workspace_id"].(string)
	wrote := call(t, s, requestV2("workspace.write", sid, map[string]any{"workspace_id": wid, "path": "src/b.ts", "content": "export const y=2;\n", "expected_hash": nil}))
	if wrote["content_hash"] == nil {
		t.Fatalf("write %v", wrote)
	}
	st := call(t, s, requestV2("workspace.status", sid, map[string]any{"workspace_id": wid}))
	if st["base_tree"] != base || st["state"] != "open" {
		t.Fatalf("status %v", st)
	}
	if ws := anyStrings(st["write_set"]); len(ws) != 1 || ws[0] != "src/b.ts" {
		t.Fatalf("write set %v", st["write_set"])
	}
	entries, err := s.Provider.(workspace.NativeProvider).Workspaces.Log(wid)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(e oplog.Entry) bool { return e.Kind == "write" }) {
		t.Fatalf("no write entry in %+v", entries)
	}
}

// A check on a native proposal runs in a clone of the manifest tree, and its
// attestation names that tree and no commit.
func TestNativeEvidenceRunsOnTheManifestTree(t *testing.T) {
	s, base := nativeFixture(t)
	check := unitCheck()
	check.Command = "test -f a.ts && ! test -e .git"
	writeChecks(t, s, []schema.CheckDefinition{check})
	intent := call(t, s, requestV2("intent.create", "", checkIntent()))
	sess := call(t, s, requestV2("session.create", "", map[string]any{}))
	sid := sess["session_id"].(string)
	fork := call(t, s, requestV2("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": sess["capability_id"]}))
	if err := os.WriteFile(filepath.Join(fork["path"].(string), "a.ts"), []byte("export const x=5;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prop := call(t, s, requestV2("workspace.propose", sid, map[string]any{"workspace_id": fork["workspace_id"]}))
	resolved := call(t, s, requestV2("evidence.resolve", "", map[string]any{"manifest": prop["manifest"]}))
	checks := resolved["checks"].([]any)
	def := checks[0].(map[string]any)["definition_hash"]
	run := call(t, s, requestV2("evidence.run", "", map[string]any{"manifest": prop["manifest"], "check_definition": def}))
	if run["result"] != "pass" {
		t.Fatalf("run %v", run)
	}
	o, err := s.Store.Read(run["attestation"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var att schema.EvidenceAttestation
	if err := o.Decode(&att); err != nil || o.Envelope.SchemaVersion != 2 || att.Tree != prop["tree"] || att.Commit != "" {
		t.Fatalf("attestation v%d %+v, %v", o.Envelope.SchemaVersion, att, err)
	}
	// P2-AC-022: an attestation naming any other tree is not evidence for
	// this manifest, even when everything else about it is genuine.
	forger := call(t, s, requestV2("session.create", "", map[string]any{}))["session_id"].(string)
	forged := att
	forged.VerifierSession = forger
	forged.Tree = base
	fo, err := s.seal(schema.KindEvidenceAttestation, forged, s.signers[forger])
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Store.Do(context.Background(), record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "test.forge", Digest: canon.Hash([]byte(fo.Hash)), Signer: s.signers[forger]},
		func(*index.Index) (any, []envelope.Object, error) {
			return map[string]any{}, []envelope.Object{fo}, nil
		})
	if !apierr.HasCode(err, "cross_revision_evidence") {
		t.Fatalf("attestation for another tree: %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(s.runtimeDir(), "verify")); len(left) != 0 {
		t.Fatalf("checkouts left behind: %v", left)
	}
	sel := call(t, s, requestV2("selection.record", "", map[string]any{"manifest": prop["manifest"], "evidence": []any{run["attestation"]}}))
	o, err = s.Store.Read(sel["selection"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var d schema.SelectionDecision
	if err := o.Decode(&d); err != nil || o.Envelope.SchemaVersion != 2 || d.Manifest != prop["manifest"] || d.Commit != "" {
		t.Fatalf("selection v%d %+v, %v", o.Envelope.SchemaVersion, d, err)
	}
	if !slices.ContainsFunc(d.Criteria, func(c schema.CriterionStatus) bool { return c.CriterionID == "tests" && c.Status == "satisfied" }) {
		t.Fatalf("criteria %+v", d.Criteria)
	}
}

// speculateNative runs five candidates against two checks that each take
// half a second, and returns the evaluation and how long it took.
func speculateNative(t *testing.T, workers int) (map[string]any, time.Duration) {
	t.Helper()
	_, out, _, took := speculateNativeRun(t, workers)
	return out, took
}

// speculateNativeRun submits the five candidates and evaluates the run,
// timing the evaluation.
func speculateNativeRun(t *testing.T, workers int) (*Service, map[string]any, string, time.Duration) {
	t.Helper()
	s, base := nativeFixture(t)
	s.evidenceWorkers = workers
	slow := func(name, cond string) schema.CheckDefinition {
		return schema.CheckDefinition{Name: name, Category: "mechanical", Command: cond + " && sleep 0.5", WorkingDirectory: ".",
			EnvAllowlist: []string{"PATH"}, TimeoutSeconds: 20, OutputLimitBytes: 1024}
	}
	writeChecks(t, s, []schema.CheckDefinition{slow("ok", "test -f ok.txt"), slow("short", "test $(wc -c < ok.txt) -lt 10")})
	intent := call(t, s, requestV2("intent.create", "", map[string]any{"description": "speculate", "criteria": []any{
		map[string]any{"id": "ok", "statement": "ok.txt exists", "verification": map[string]any{"kind": "check", "check": "ok"}},
		map[string]any{"id": "short", "statement": "ok.txt is short", "verification": map[string]any{"kind": "check", "check": "short"}},
	}}))["intent_id"].(string)
	strategies := []any{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		strategies = append(strategies, map[string]any{"name": name, "spec": "write ok.txt"})
	}
	start := call(t, s, requestV2("speculation.start", "", map[string]any{"intent_id": intent, "scorer": submissionScorer(),
		"deadline_seconds": 600, "strategies": strategies}))
	if start["base_tree"] != base {
		t.Fatalf("run base %v, want %s", start["base_tree"], base)
	}
	runID := start["run_id"].(string)
	for i, a := range start["attempts"].([]any) {
		a := a.(map[string]any)
		content := []string{"ok", "much too long to pass", "", "fine", "x"}[i]
		if content != "" {
			if err := os.WriteFile(filepath.Join(a["path"].(string), "ok.txt"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(filepath.Join(a["path"].(string), "other.txt"), []byte("no ok.txt"), 0600); err != nil {
			t.Fatal(err)
		}
		call(t, s, requestV2("speculation.submit", a["session_id"].(string), map[string]any{"run_id": runID, "workspace_id": a["workspace_id"]}))
	}
	began := time.Now()
	out := call(t, s, requestV2("speculation.evaluate", "", map[string]any{"run_id": runID}))
	return s, out, runID, time.Since(began)
}

// P2-AC-014: speculation with 5 candidates and 2 checks runs the checks in
// parallel, with the same results as running them one at a time.
func TestNativeSpeculationRunsChecksInParallel(t *testing.T) {
	parallel, fast := speculateNative(t, nativeEvidenceWorkers)
	serial, slow := speculateNative(t, 1)
	// Each eligible candidate, by strategy, with its criteria statuses.
	outcomes := func(res map[string]any) []string {
		var got []string
		for _, c := range res["candidates"].(map[string]any)["candidates"].([]any) {
			c := c.(map[string]any)
			var statuses []string
			for _, st := range c["criteria"].([]any) {
				st := st.(map[string]any)
				statuses = append(statuses, fmt.Sprint(st["criterion_id"], "=", st["status"]))
			}
			got = append(got, fmt.Sprint(c["strategy"], " ", statuses))
		}
		slices.Sort(got)
		return got
	}
	if p, q := outcomes(parallel), outcomes(serial); !slices.Equal(p, q) {
		t.Fatalf("parallel %v, serial %v", p, q)
	}
	if fast > slow*6/10 {
		t.Fatalf("parallel evaluation took %v, serial %v", fast, slow)
	}
	if got := outcomes(parallel); len(got) != 3 || !strings.HasPrefix(got[0], "a ") || !strings.HasPrefix(got[1], "d ") || !strings.HasPrefix(got[2], "e ") {
		t.Fatalf("eligible %v, want a, d and e", got)
	}
	t.Logf("parallel %v, serial %v", fast, slow)
}

// A native run resolves to a v2 selection without a commit, and removes its
// candidates' checkouts.
func TestNativeSpeculationResolves(t *testing.T) {
	s, evaluated, runID, _ := speculateNativeRun(t, nativeEvidenceWorkers)
	scores := map[string]any{}
	for i, id := range evaluated["eligible"].([]any) {
		scores[id.(string)] = i + 1
	}
	out := call(t, s, requestV2("speculation.score", "", map[string]any{"run_id": runID, "scores": scores}))
	if out["state"] != "resolved" {
		t.Fatalf("outcome %v", out)
	}
	winner := out["winner"].(map[string]any)
	o, err := s.Store.Read(out["selection"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var d schema.SelectionDecision
	if err := o.Decode(&d); err != nil || o.Envelope.SchemaVersion != 2 || d.Manifest != winner["manifest"] || d.Commit != "" {
		t.Fatalf("selection v%d %+v, %v", o.Envelope.SchemaVersion, d, err)
	}
	for _, a := range out["attempts"].([]any) {
		wid := a.(map[string]any)["workspace_id"].(string)
		if wid == winner["workspace_id"] {
			continue
		}
		if _, err := os.Stat(s.Provider.Path(wid)); !os.IsNotExist(err) {
			t.Fatalf("losing checkout %s kept: %v", wid, err)
		}
	}
	status := call(t, s, requestV2("speculation.status", "", map[string]any{"run_id": runID}))
	if status["state"] != "resolved" || status["base_tree"] == nil {
		t.Fatalf("status %v", status)
	}
}

// Detaching gives one workspace a private, writable dependency tree and
// leaves the shared one, and the other workspaces linking it, untouched.
func TestNativeDetachDeps(t *testing.T) {
	s, _ := nativeFixtureWith(t, map[string]string{
		".ledgerignore":                  "node_modules/\n",
		"package.json":                   `{"name":"app"}`,
		"node_modules/left-pad/index.js": "module.exports = s => s\n",
	})
	intent := call(t, s, requestV2("intent.create", "", judgment("deps")))["intent_id"].(string)
	sess := call(t, s, requestV2("session.create", "", map[string]any{}))
	sid := sess["session_id"].(string)
	fork := func() (string, string) {
		f := call(t, s, requestV2("workspace.fork", sid, map[string]any{"intent_id": intent, "capability_id": sess["capability_id"]}))
		return f["workspace_id"].(string), filepath.Join(f["path"].(string), "node_modules")
	}
	a, aDeps := fork()
	_, bDeps := fork()
	t.Cleanup(func() {
		deps := filepath.Join(s.Store.Dir, "runtime", "deps")
		_ = filepath.WalkDir(deps, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0700)
			}
			return nil
		})
	})
	shared := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, ".ledger-shared"))
		return err == nil
	}
	if !shared(aDeps) || !shared(bDeps) {
		t.Fatal("forks do not share the dependency tree")
	}
	got := call(t, s, requestV2("workspace.deps.detach", sid, map[string]any{"workspace_id": a, "path": "node_modules"}))
	if got["workspace_id"] != a || got["path"] != "node_modules" {
		t.Fatalf("detach %v", got)
	}
	if shared(aDeps) || !shared(bDeps) {
		t.Fatal("detach did not apply to exactly one workspace")
	}
	if err := os.WriteFile(filepath.Join(aDeps, "left-pad", "index.js"), []byte("a"), 0600); err != nil {
		t.Fatalf("detached tree is not writable: %v", err)
	}
	if body, _ := os.ReadFile(filepath.Join(bDeps, "left-pad", "index.js")); string(body) != "module.exports = s => s\n" {
		t.Fatalf("other workspace saw the install: %q", body)
	}
	call(t, s, requestV2("workspace.deps.detach", sid, map[string]any{"workspace_id": a}))
	if _, err := s.Handle(context.Background(), requestV2("workspace.deps.detach", "", map[string]any{"workspace_id": a})); !apierr.HasCode(err, "unauthorized") {
		t.Fatalf("detach without the session: %v", err)
	}
}
