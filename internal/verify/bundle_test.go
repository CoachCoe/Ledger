package verify_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/cli"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/oplog"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/verify"
)

func code(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	return apierr.As(err).Code
}

func TestAC088_089_179_IntegrityAndTrustSeparate(t *testing.T) {
	c := newChain(t, "true")
	path := c.mustExport()

	// Integrity is verified whether or not the roots are trusted.
	rep := verifyBundle(t, path)
	if rep.Integrity != verify.Verified || rep.Trust != verify.Untrusted || !rep.Complete || len(rep.Invalid) != 0 || len(rep.Missing) != 0 {
		t.Fatalf("no trust list: %s/%s %+v", rep.Integrity, rep.Trust, rep)
	}
	if len(rep.Roots) != 1 || rep.Roots[0].Trusted {
		t.Fatalf("keys carried in the bundle must not be trusted: %+v", rep.Roots)
	}
	rep = verifyBundle(t, path, c.root.ID())
	if rep.Integrity != verify.Verified || rep.Trust != verify.Trusted || !rep.Roots[0].Trusted || rep.Manifests[0].Trust != verify.Trusted {
		t.Fatalf("trusted root: %s/%s", rep.Integrity, rep.Trust)
	}
	if rep = verifyBundle(t, path, strings.Repeat("a", 64)); rep.Trust != verify.Untrusted {
		t.Fatalf("an unrelated fingerprint trusts nothing: %s", rep.Trust)
	}

	// A flipped byte is invalid and trust is then unknown.
	p := readParts(t, path)
	h := p.byKind(t, schema.KindChangeManifest)[0]
	p.Records[h][len(p.Records[h])/2] ^= 1
	rep = verifyBundle(t, p.write(t, true), c.root.ID())
	if rep.Integrity != verify.Invalid || rep.Trust != verify.Unknown || len(rep.Invalid) == 0 {
		t.Fatalf("tampered: %s/%s", rep.Integrity, rep.Trust)
	}

	// A missing record is unavailable, trust unknown.
	p = readParts(t, path)
	delete(p.Records, p.byKind(t, schema.KindCheckDefinition)[0])
	rep = verifyBundle(t, p.write(t, true), c.root.ID())
	if rep.Integrity != verify.Unavailable || rep.Trust != verify.Unknown || len(rep.Invalid) != 0 || len(rep.Missing) == 0 {
		t.Fatalf("missing: %s/%s invalid=%v", rep.Integrity, rep.Trust, rep.Invalid)
	}

	// A second root that is only partially trusted makes the whole untrusted.
	other, _, err := (identity.LocalKeys{Dir: filepath.Join(t.TempDir(), "k")}).Ensure()
	if err != nil {
		t.Fatal(err)
	}
	o, err := envelope.SealVersion(2, schema.KindRootKey, schema.RootKey{PublicKey: identity.EncodePublicKey(other.PublicKey()), CreatedAt: "2026-11-05T12:00:00Z"}, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.Do(context.Background(), record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "test.rootkey", Digest: canon.Hash([]byte("root2")), Signer: other}, func(*index.Index) (any, []envelope.Object, error) {
		return map[string]any{}, []envelope.Object{o}, nil
	}); err != nil {
		t.Fatal(err)
	}
	two := c.mustExport()
	if rep = verifyBundle(t, two, c.root.ID()); rep.Integrity != verify.Verified || rep.Trust != verify.Untrusted || len(rep.Roots) != 2 {
		t.Fatalf("partial trust: %s/%s %+v", rep.Integrity, rep.Trust, rep.Roots)
	}
	if rep = verifyBundle(t, two, c.root.ID(), other.ID()); rep.Trust != verify.Trusted {
		t.Fatalf("both trusted: %s", rep.Trust)
	}
}

func TestAC088_UnavailableNotInvalid(t *testing.T) {
	c := newChain(t, "true")
	path := c.mustExport()
	receiptOf := func(p *parts) string {
		for _, h := range p.byKind(t, schema.KindOperationReceipt) {
			var r struct{ Members []string }
			if err := json.Unmarshal(mustField(t, p.payload(t, h), "members"), &r.Members); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(r.Members, c.manifest) {
				return h
			}
		}
		t.Fatal("no receipt for the manifest")
		return ""
	}
	cases := map[string]func(*parts){
		"record":  func(p *parts) { delete(p.Records, p.byKind(t, schema.KindCheckDefinition)[0]) },
		"session": func(p *parts) { delete(p.Records, p.byKind(t, schema.KindSession)[0]) },
		"receipt": func(p *parts) { delete(p.Records, receiptOf(p)) },
		"store": func(p *parts) {
			for name := range p.Files {
				if strings.HasPrefix(name, "store/") {
					delete(p.Files, name)
				}
			}
		},
	}
	for name, mutate := range cases {
		for _, refresh := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				p := readParts(t, path)
				mutate(p)
				rep := verifyBundle(t, p.write(t, refresh))
				if rep.Integrity != verify.Unavailable || len(rep.Invalid) != 0 || rep.Trust != verify.Unknown {
					t.Fatalf("%s (refresh=%v): %s/%s invalid=%v missing=%v", name, refresh, rep.Integrity, rep.Trust, rep.Invalid, rep.Missing)
				}
				if len(rep.Manifests) != 1 {
					t.Fatalf("components are still reported: %+v", rep.Manifests)
				}
				if name == "store" {
					if got := rep.Manifests[0].Components["tree"]; got != verify.Unavailable {
						t.Fatalf("a missing tree is unavailable: %v", rep.Manifests[0].Components)
					}
					if rep.Manifests[0].Components["authority"] != verify.Verified {
						t.Fatalf("authority still verifies: %v", rep.Manifests[0].Components)
					}
				}
			})
		}
	}
}

func mustField(t *testing.T, m map[string]any, k string) []byte {
	t.Helper()
	b, err := json.Marshal(m[k])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rewrittenGit returns a Git bundle whose retained commit for manifest is
// replaced by a commit that breaks exactly one binding condition.
func (r *v1repo) rewrittenGit(t *testing.T, manifest, what string) (bundle []byte, sha, tree string) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "b.git")
	run := func(stdin string, args ...string) string {
		out, err := gitx.Run(ctx, bare, strings.NewReader(stdin), gitEnv, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if _, err := gitx.Run(ctx, tmp, nil, gitEnv, "init", "--bare", "-q", bare); err != nil {
		t.Fatal(err)
	}
	ref := verify.ProposalRefPrefix + manifest
	commit := r.commit(t, manifest)
	run("", "fetch", "-q", "--no-tags", r.repo.Dir, "+"+ref+":"+ref)
	orig, err := (&gitx.Repo{Dir: bare, CommonDir: bare, MainDir: bare}).ReadCommit(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	tree, parent, message := orig.Tree, orig.Parents[0], orig.Message
	switch what {
	case "trailer":
		message = "rewritten\n"
	case "tree":
		tree = run("", "mktree")
	case "parent":
		parent = commit
	}
	sha = run(message, "commit-tree", tree, "-p", parent, "-F", "-")
	run("", "update-ref", ref, sha)
	file := filepath.Join(tmp, "o.bundle")
	run("", "bundle", "create", "-q", file, ref)
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return raw, sha, tree
}

func TestAC177_TamperDetected(t *testing.T) {
	c := newChain(t, "true")
	path := c.mustExport()
	kinds := map[schema.Kind]string{}
	base := readParts(t, path)
	for h := range base.Records {
		o, err := envelope.Parse(base.Records[h])
		if err != nil {
			t.Fatal(err)
		}
		kinds[o.Envelope.Kind] = h
	}
	for _, k := range []schema.Kind{schema.KindRootKey, schema.KindSession, schema.KindCapability, schema.KindIntent, schema.KindCheckDefinition, schema.KindWorkspace, schema.KindLifecycleEvent, schema.KindChangeManifest, schema.KindEvidenceAttestation, schema.KindSelectionDecision, schema.KindOperationReceipt} {
		if kinds[k] == "" {
			t.Fatalf("the chain has no %s record", k)
		}
	}
	t.Run("flipped byte in every kind", func(t *testing.T) {
		for k, h := range kinds {
			p := readParts(t, path)
			p.Records[h][len(p.Records[h])/2] ^= 1
			if rep := verifyBundle(t, p.write(t, true)); rep.Integrity != verify.Invalid {
				t.Fatalf("%s: %s", k, rep.Integrity)
			}
		}
	})
	t.Run("swapped signer", func(t *testing.T) {
		p := readParts(t, path)
		p.forge(t, kinds[schema.KindEvidenceAttestation], func(env map[string]any) { env["signer"] = c.sid })
		if rep := verifyBundle(t, p.write(t, true)); rep.Integrity != verify.Invalid {
			t.Fatalf("%s", rep.Integrity)
		}
	})
	t.Run("changed manifest tree", func(t *testing.T) {
		p := readParts(t, path)
		p.forge(t, c.manifest, func(env map[string]any) {
			env["payload"].(map[string]any)["tree"] = strings.Repeat("1", 64)
		})
		if rep := verifyBundle(t, p.write(t, true)); rep.Integrity != verify.Invalid {
			t.Fatalf("%s", rep.Integrity)
		}
	})
}

// The v1 path still reports each commit binding condition separately.
func TestPhase1CommitBindingTamper(t *testing.T) {
	r := newV1Repo(t)
	path := r.mustExport(t)
	manifest := r.manifests[1]
	for _, what := range []string{"trailer", "tree", "parent"} {
		t.Run("commit "+what, func(t *testing.T) {
			raw, sha, tree := r.rewrittenGit(t, manifest, what)
			p := readParts(t, path)
			p.Files[verify.GitName] = raw
			for _, cm := range p.Inv["git"].(map[string]any)["commits"].([]any) {
				if cm := cm.(map[string]any); cm["manifest"] == manifest {
					cm["commit"], cm["tree"] = sha, tree
				}
			}
			rep := verifyBundle(t, p.write(t, true))
			i := slices.IndexFunc(rep.Manifests, func(m verify.ManifestReport) bool { return m.Hash == manifest })
			if rep.Integrity != verify.Invalid || i < 0 {
				t.Fatalf("%s: %s", what, rep.Integrity)
			}
			b := *rep.Manifests[i].Binding
			got := map[string]string{"trailer": b.Trailer, "tree": b.Tree, "parent": b.Parent}
			for cond, v := range got {
				want := verify.BindingOK
				if cond == what {
					want = verify.BindingMismatch
				}
				if v != want {
					t.Fatalf("%s rewrite: condition %s is %s, want %s (%+v)", what, cond, v, want, b)
				}
			}
			if rep.Manifests[i].Components["authority"] != verify.Verified {
				t.Fatalf("authority is independent of the binding: %v", rep.Manifests[i].Components)
			}
		})
	}
}

func TestAC078_099_114_BundleInventoryAndClosure(t *testing.T) {
	c := newChain(t, "true")
	path := c.mustExport()
	p := readParts(t, path)
	inv := p.Inv
	if inv["bundle_version"].(float64) != 2 || inv["complete"] != true {
		t.Fatalf("inventory: %v", inv)
	}
	ids := inv["ids"].(map[string]any)
	for _, id := range []string{c.sid, c.cap, c.wid, c.intent} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("inventory ids lack %s", id)
		}
	}
	var haveRoot, haveSession bool
	for _, k := range inv["keys"].([]any) {
		key := k.(map[string]any)
		haveRoot = haveRoot || (key["role"] == "root" && key["fingerprint"] == c.root.ID())
		haveSession = haveSession || (key["role"] == "session" && key["session_id"] == c.sid)
	}
	if !haveRoot || !haveSession || len(inv["roots"].([]any)) != 1 {
		t.Fatalf("public keys: %v", inv["keys"])
	}
	for _, k := range []schema.Kind{schema.KindRepository, schema.KindOperationReceipt, schema.KindLifecycleEvent, schema.KindEvidenceAttestation, schema.KindSelectionDecision, schema.KindCheckDefinition} {
		if len(p.byKind(t, k)) == 0 {
			t.Fatalf("no %s in the bundle", k)
		}
	}
	// The store part carries the trees, and the workspace's operation log
	// travels up to the head the manifest pinned.
	if len(inv["store"].([]any)) == 0 {
		t.Fatalf("no store objects: %v", inv["store"])
	}
	logs := inv["oplogs"].([]any)
	if len(logs) != 1 || logs[0].(map[string]any)["workspace"] != c.wid || p.Files["oplog/"+c.wid+".log"] == nil {
		t.Fatalf("operation logs: %v", logs)
	}
	// Unknown versions fail explicitly instead of being reinterpreted.
	q := readParts(t, path)
	q.Inv["bundle_version"] = 3
	if _, err := tryVerify(q.write(t, false)); code(t, err) != "unsupported_bundle_version" {
		t.Fatalf("bundle version: %v", err)
	}
	q = readParts(t, path)
	q.forge(t, p.byKind(t, schema.KindIntent)[0], func(env map[string]any) { env["schema_version"] = 3 })
	if _, err := tryVerify(q.write(t, true)); code(t, err) != "unsupported_schema_version" {
		t.Fatalf("schema version: %v", err)
	}
	// A closure export of one manifest is complete and verifies.
	closure, err := c.export(verify.ExportOptions{Manifests: []string{c.manifest}})
	if err != nil || !closure.Complete {
		t.Fatalf("closure: %v %+v", err, closure)
	}
	if rep := verifyBundle(t, closure.Out); rep.Integrity != verify.Verified || len(rep.Manifests[0].Evidence) != 2 || len(rep.Manifests[0].Selections) != 1 {
		t.Fatalf("closure report: %+v", rep)
	}
	byIntent, err := c.export(verify.ExportOptions{Intents: []string{c.intent}})
	if err != nil || len(byIntent.Manifests) != 1 {
		t.Fatalf("intent closure: %v", err)
	}
	if _, err := c.export(verify.ExportOptions{Manifests: []string{strings.Repeat("0", 64)}}); code(t, err) != "not_found" {
		t.Fatal("an unknown manifest must not export")
	}
	// A missing dependency is never silently partial.
	if err := os.Remove(oplog.Path(filepath.Join(c.dir, record.DirName, "oplog"), c.wid)); err != nil {
		t.Fatal(err)
	}
	_, err = c.export(verify.ExportOptions{})
	if code(t, err) != "export_incomplete" {
		t.Fatalf("missing operation log: %v", err)
	}
	partial, err := c.export(verify.ExportOptions{AllowPartial: true})
	if err != nil || partial.Complete || len(partial.Missing) == 0 {
		t.Fatalf("allow-partial: %v %+v", err, partial)
	}
	if rep := verifyBundle(t, partial.Out); rep.Integrity != verify.Unavailable || rep.Complete {
		t.Fatalf("a partial bundle never verifies complete: %s %v", rep.Integrity, rep.Complete)
	}
}

// A v1 bundle's Git part carries the retained proposal commits and the
// records ref, and needs nothing from the source repository.
func TestPhase1BundleGitPart(t *testing.T) {
	r := newV1Repo(t)
	p := readParts(t, r.mustExport(t))
	if p.Inv["bundle_version"].(float64) != 1 {
		t.Fatalf("inventory: %v", p.Inv)
	}
	git := p.Inv["git"].(map[string]any)
	refs := git["refs"].([]any)
	for _, m := range r.manifests {
		if !slices.Contains(refs, any(verify.ProposalRefPrefix+m)) {
			t.Fatalf("git refs lack %s: %v", m, refs)
		}
	}
	if !slices.Contains(refs, any(verify.RecordsRef)) {
		t.Fatalf("git refs: %v", refs)
	}
	for _, cm := range git["commits"].([]any) {
		cm := cm.(map[string]any)
		if cm["commit"] != r.commit(t, cm["manifest"].(string)) {
			t.Fatalf("commits: %v", cm)
		}
	}
	ctx := context.Background()
	empty := t.TempDir()
	if _, err := gitx.Run(ctx, empty, nil, gitEnv, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "objects.bundle")
	if err := os.WriteFile(file, p.Files[verify.GitName], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, empty, nil, gitEnv, "bundle", "verify", file); err != nil {
		t.Fatalf("the git part has prerequisites: %v", err)
	}
}

func run(t *testing.T, args ...string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	status := cli.Main(args, &out)
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if (status == 0) != (v["ok"] == true) {
		t.Fatalf("status %d for %s", status, out.String())
	}
	return v
}

func TestAC033_178_235_VerifyWithoutSQLite(t *testing.T) {
	c := newChain(t, "true")
	c.store.Close()
	for _, f := range []string{"index.db", "index.db-wal", "index.db-shm"} {
		_ = os.Remove(filepath.Join(c.store.Dir, f))
	}
	t.Chdir(c.dir)
	db := filepath.Join(c.store.Dir, "index.db")
	if v := run(t, "verify"); v["ok"] != true || v["result"].(map[string]any)["integrity"] != "verified" {
		t.Fatalf("verify: %v", v)
	}
	out := filepath.Join(t.TempDir(), "b.ledger")
	if v := run(t, "export", "--out", out); v["ok"] != true {
		t.Fatalf("export: %v", v)
	}
	if _, err := os.Stat(db); err == nil {
		t.Fatal("verify and export created or opened the SQLite index")
	}
	// --strict turns a non-verified outcome into an error, with the report.
	broken := readParts(t, out)
	delete(broken.Records, broken.byKind(t, schema.KindCheckDefinition)[0])
	bad := broken.write(t, true)
	if v := run(t, "verify", "--bundle", bad); v["ok"] != true || v["result"].(map[string]any)["integrity"] != "unavailable" {
		t.Fatalf("non-strict verify reports and succeeds: %v", v)
	}
	if v := run(t, "verify", "--bundle", bad, "--strict"); v["ok"] != false || v["error"].(map[string]any)["code"] != "verification_failed" {
		t.Fatalf("strict: %v", v)
	}
	// Reindex needs no daemon either.
	if v := run(t, "reindex"); v["ok"] != true {
		t.Fatalf("reindex: %v", v)
	}
}

func TestAC107_ProposalRefsPreserved(t *testing.T) {
	// Phase 1 retained every revision at its own ref, so both revisions of
	// the pinned repository survive garbage collection and verify.
	r := newV1Repo(t)
	if _, err := gitx.Run(context.Background(), r.dir, nil, gitEnv, "gc", "-q", "--prune=now"); err != nil {
		t.Fatal(err)
	}
	path := r.mustExport(t)
	refs := readParts(t, path).Inv["git"].(map[string]any)["refs"].([]any)
	for _, m := range r.manifests {
		if !slices.Contains(refs, any(verify.ProposalRefPrefix+m)) {
			t.Fatalf("revision %s is not retained: %v", m, refs)
		}
	}
	rep := verifyBundle(t, path)
	if rep.Integrity != verify.Verified || len(rep.Manifests) != 2 {
		t.Fatalf("both revisions verify after gc: %s %d", rep.Integrity, len(rep.Manifests))
	}
	// A native rework keeps both revisions too: each tree stays in the store.
	c := newChain(t, "true")
	first := c.manifest
	c.call("workspace.rework", c.sid, map[string]any{"workspace_id": c.wid})
	c.propose("export const x=3;\n")
	if rep := verifyBundle(t, c.mustExport()); rep.Integrity != verify.Verified || len(rep.Manifests) != 2 || c.manifest == first {
		t.Fatalf("native revisions: %s %d", rep.Integrity, len(rep.Manifests))
	}
	doc, err := os.ReadFile("../../docs/workflow.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "refs/ledger/proposals/*:refs/ledger/proposals/*") || !strings.Contains(string(doc), "refs/ledger/records") {
		t.Fatal("docs/workflow.md must document the fetch and push refspecs for the retention refs and the records ref")
	}
}
