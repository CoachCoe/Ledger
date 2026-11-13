package verify_test

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/verify"
)

// pub publishes through a site's RecordStore, as the CLI does.
type pub struct{ s *site }

func (p pub) Publish(ctx context.Context, objs []envelope.Object) (verify.PublishResult, error) {
	r, err := p.s.store.ImportRecords(ctx, objs)
	if err != nil {
		return verify.PublishResult{}, err
	}
	return verify.PublishResult{Written: r.Imported, Indexed: r.Indexed, Detail: r}, nil
}

type noPublish struct{ t *testing.T }

func (p noPublish) Publish(context.Context, []envelope.Object) (verify.PublishResult, error) {
	p.t.Helper()
	p.t.Fatal("nothing may be published")
	return verify.PublishResult{}, nil
}

func (s *site) importBundle(path string, opts verify.ImportOptions) (*verify.ImportReport, error) {
	s.t.Helper()
	return verify.Import(context.Background(), path, pub{s}, s.repo, opts)
}

func (s *site) mustImport(path string) *verify.ImportReport {
	s.t.Helper()
	rep, err := s.importBundle(path, verify.ImportOptions{})
	if err != nil {
		s.t.Fatal(err)
	}
	return rep
}

func (s *site) digest() string {
	s.t.Helper()
	d, err := s.store.Index.Digest(context.Background())
	if err != nil {
		s.t.Fatal(err)
	}
	return d
}

func (s *site) refs() string {
	s.t.Helper()
	out, err := gitx.Run(context.Background(), s.dir, nil, gitEnv, "for-each-ref", "--format=%(refname) %(objectname)", "refs/ledger/proposals", "refs/heads")
	if err != nil {
		s.t.Fatal(err)
	}
	return string(out)
}

func (s *site) objectCount() int {
	s.t.Helper()
	n := 0
	_ = filepath.WalkDir(filepath.Join(s.store.Dir, "objects"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func (s *site) stagingEmpty() {
	s.t.Helper()
	entries, _ := os.ReadDir(filepath.Join(s.store.Dir, "runtime", "import"))
	if len(entries) != 0 {
		s.t.Fatalf("staging was not removed: %v", entries)
	}
}

func TestAC115_ForeignRootImport(t *testing.T) {
	// Decision 15: replay accepts groups signed by a root that is not the
	// importing repository's own, without letting that root sign locally.
	c := newChain(t, "true")
	path := c.mustExport()
	dst := newSite(t)
	if dst.root.ID() == c.root.ID() {
		t.Fatal("the destination must have its own root")
	}
	before := dst.refs()
	rep := dst.mustImport(path)
	if rep.AlreadyPresent || rep.Publish.Written == 0 || rep.Verification.Integrity != verify.Verified {
		t.Fatalf("import: %+v", rep)
	}
	ctx := context.Background()
	for _, id := range []string{c.sid, c.wid, c.intent} {
		if sub, err := dst.store.Index.Subject(ctx, id); err != nil || sub == nil {
			t.Fatalf("%s was not indexed: %v", id, err)
		}
	}
	if sha, _ := dst.repo.ResolveRef(ctx, verify.ProposalRefPrefix+c.manifest); sha != c.commit {
		t.Fatalf("retention ref: %q", sha)
	}
	if dst.refs() == before {
		t.Fatal("the retention ref was not created")
	}
	// Reindex replays the same imported history.
	d := dst.digest()
	if _, err := dst.store.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	if dst.digest() != d {
		t.Fatal("reindex changed the imported state")
	}
	// The foreign root is history only: the local root still signs locally.
	root, err := dst.keys.Root()
	if err != nil || root.ID() != dst.root.ID() {
		t.Fatalf("local root changed: %v", err)
	}
	ses := dst.call("session.create", "", map[string]any{"agent_name": "local"})
	o, err := dst.store.Read(ses["_receipt"].(string))
	if err != nil || o.Envelope.Signer != dst.root.ID() {
		t.Fatalf("a local operation must be signed by the local root: %v", err)
	}
	// The repository verifies, with both roots untrusted until the user says so.
	src, err := verify.OpenRepoSource(ctx, dst.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	list := identity.NewTrustList()
	_ = list.Merge([]string{dst.root.ID()})
	r, err := verify.Verify(ctx, src, verify.Options{Trusted: list})
	if err != nil || r.Integrity != verify.Verified || r.Trust != verify.Untrusted || len(r.Roots) != 2 {
		t.Fatalf("verify: %v %+v", err, r)
	}
}

func TestAC115_NeverImportsPrivateOrTrust(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	c := newChain(t, "true")
	path := c.mustExport()
	dst := newSite(t)
	keyFiles := func() map[string]string {
		out := map[string]string{}
		_ = filepath.WalkDir(dst.keys.Dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				b, _ := os.ReadFile(p)
				out[p] = string(b)
			}
			return nil
		})
		return out
	}
	keysBefore, heads := keyFiles(), dst.refs()
	dst.mustImport(path)
	if got := keyFiles(); len(got) != len(keysBefore) {
		t.Fatalf("key files changed: %v", got)
	} else {
		for p, v := range keysBefore {
			if got[p] != v {
				t.Fatalf("%s changed", p)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(cfg, "ledger", "trusted-roots.json")); err == nil {
		t.Fatal("import granted trust")
	}
	if _, err := os.Stat(filepath.Join(dst.store.Dir, "config.json")); err == nil {
		t.Fatal("import wrote configuration")
	}
	after := dst.refs()
	for _, line := range strings.Split(heads, "\n") {
		if strings.Contains(line, "refs/heads") && !strings.Contains(after, line) {
			t.Fatalf("a code branch changed: %s", line)
		}
	}
	for _, line := range strings.Split(after, "\n") {
		if strings.Contains(line, "refs/heads") && !strings.Contains(heads, line) {
			t.Fatalf("a code branch appeared: %s", line)
		}
	}
	// Imported sessions are history: nothing can act or sign as one.
	raw, _ := canon.Marshal(map[string]any{"description": "as imported session"})
	_, err := dst.service().Handle(context.Background(), api.Request{Version: 1, Operation: "intent.create", SessionID: c.sid, OperationID: schema.NewID(schema.PrefixOperation), Input: raw})
	if err == nil {
		t.Fatal("an imported session acted in the destination")
	}
	if sub, _ := dst.store.Index.Subject(context.Background(), c.sid); sub == nil {
		t.Fatal("the imported session remains visible as history")
	}
}

func TestAC115_IdempotentImport(t *testing.T) {
	c := newChain(t, "true")
	path := c.mustExport()
	dst := newSite(t)
	first := dst.mustImport(path)
	d1, r1, n1 := dst.digest(), dst.refs(), dst.objectCount()
	second := dst.mustImport(path)
	if first.AlreadyPresent || !second.AlreadyPresent || second.Publish.Written != 0 || second.Publish.Indexed != 0 {
		t.Fatalf("first=%+v second=%+v", first.Publish, second.Publish)
	}
	for _, r := range second.Refs {
		if r.Status != "present" {
			t.Fatalf("ref %+v", r)
		}
	}
	if dst.digest() != d1 || dst.refs() != r1 || dst.objectCount() != n1 {
		t.Fatal("a second import changed state")
	}
	dst.stagingEmpty()
}

// forkBundle adds a root-signed session-end group to the exported chain: a
// valid lifecycle event, in a group whose operation id is shared with
// competing bundles.
func (c *chain) forkBundle(t *testing.T, base string, opID string, after time.Duration) string {
	t.Helper()
	p := readParts(t, base)
	var sessionHash string
	for _, h := range p.byKind(t, schema.KindSession) {
		if p.payload(t, h)["id"] == c.sid {
			sessionHash = h
		}
	}
	at := schema.Timestamp(time.Now().Add(after))
	ev, err := envelope.Seal(schema.KindLifecycleEvent, schema.LifecycleEvent{SubjectKind: "session", SubjectID: c.sid, Creation: sessionHash, FromState: "active", ToState: "ended", Actor: c.root.ID(), At: at}, c.root)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := envelope.Seal(schema.KindOperationReceipt, schema.OperationReceipt{OperationID: opID, Operation: "session.end", RequestDigest: canon.Hash([]byte(at)), Result: []byte("{}"), Members: []string{ev.Hash}, At: at}, c.root)
	if err != nil {
		t.Fatal(err)
	}
	p.Records[ev.Hash], p.Records[receipt.Hash] = ev.Bytes, receipt.Bytes
	return p.write(t, true)
}

func TestAC115_ConflictsPreserved(t *testing.T) {
	c := newChain(t, "true")
	base := c.mustExport()
	op := schema.NewID(schema.PrefixOperation)
	a, b := c.forkBundle(t, base, op, time.Hour), c.forkBundle(t, base, op, 2*time.Hour)
	// Each bundle alone is valid and conflict free.
	for _, p := range []string{a, b} {
		if rep := verifyBundle(t, p); rep.Integrity != verify.Verified || len(rep.Conflicts) != 0 {
			t.Fatalf("one fork alone: %s %v", rep.Integrity, rep.Conflicts)
		}
	}
	ctx := context.Background()
	dst := newSite(t)
	dst.mustImport(a)
	rep := dst.mustImport(b)
	if rep.Verification.Integrity != verify.Verified {
		t.Fatalf("a fork is a conflict, not an integrity failure: %+v", rep.Verification)
	}
	sub, err := dst.store.Index.Subject(ctx, c.sid)
	if err != nil || sub == nil || sub.Conflict == "" {
		t.Fatalf("the diverged session must be a conflict: %+v %v", sub, err)
	}
	if sub.State == "ended" {
		t.Fatal("no side of a fork may be chosen")
	}
	ops, err := dst.store.Index.ConflictingOperations(ctx)
	if err != nil || len(ops[op]) != 2 {
		t.Fatalf("incompatible receipts must be preserved: %v %v", ops, err)
	}
	// Mutating the affected subject is blocked.
	raw, _ := canon.Marshal(map[string]any{"session_id": c.sid})
	if _, err := dst.service().Handle(ctx, api.Request{Version: 1, Operation: "session.end", OperationID: schema.NewID(schema.PrefixOperation), Input: raw}); err == nil {
		t.Fatal("a conflicted subject accepted a mutation")
	}
	// The repository's own report lists both preserved conflicts.
	src, err := verify.OpenRepoSource(ctx, dst.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	r, err := verify.Verify(ctx, src, verify.Options{})
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, cf := range r.Conflicts {
		kinds = append(kinds, cf.Kind)
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"lifecycle_fork", "operation_conflict"}) {
		t.Fatalf("conflicts: %v", r.Conflicts)
	}
	// Import order, and therefore time, decides nothing.
	other := newSite(t)
	other.mustImport(b)
	other.mustImport(a)
	osub, _ := other.store.Index.Subject(ctx, c.sid)
	if osub == nil || osub.Conflict != sub.Conflict || osub.State != sub.State {
		t.Fatalf("order changed the outcome: %+v vs %+v", osub, sub)
	}
}

func TestAC116_PortableVerificationAfterSourceLoss(t *testing.T) {
	c := newChain(t, "true")
	path := c.mustExport()
	// Lose the source: rename and lock down its repository and ledger.
	gone := c.dir + ".gone"
	if err := os.Rename(c.dir, gone); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gone, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gone, 0o700) })
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, "http://127.0.0.1:9")
	}
	t.Chdir(t.TempDir())
	v := run(t, "verify", "--bundle", path, "--trust", c.root.ID(), "--strict")
	res := v["result"].(map[string]any)
	if v["ok"] != true || res["integrity"] != "verified" || res["trust"] != "trusted" {
		t.Fatalf("portable verify: %v", v)
	}
	// The binary no longer writes a Phase 1 repository: importing into one
	// is refused untouched. Native import is TestNativeBundleRoundTrip's.
	dst := newSite(t)
	dst.store.Close()
	t.Chdir(dst.dir)
	imp := run(t, "import", path)
	if imp["ok"] != false || imp["error"].(map[string]any)["code"] != "migrate_required" {
		t.Fatalf("import into a Phase 1 repository: %v", imp)
	}
	if v := run(t, "verify", "--strict"); v["ok"] != true {
		t.Fatalf("verify of the untouched repository: %v", v)
	}
}

func TestAC251_TwoKindsOfState(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "ledger"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "ledger", "trusted-roots.json"), []byte(`{"version":1,"roots":[{"fingerprint":"`+strings.Repeat("e", 64)+`","label":"TRUST-MARKER"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newChain(t, "true")
	for rel, body := range map[string]string{"journal/stray.json": "JOURNAL-MARKER", "runtime/state.json": "RUNTIME-MARKER"} {
		if err := os.WriteFile(filepath.Join(c.store.Dir, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := c.mustExport()
	var secrets [][]byte
	_ = filepath.WalkDir(c.keys.Dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); len(bytes.TrimSpace(b)) > 8 {
				secrets = append(secrets, bytes.TrimSpace(b))
			}
		}
		return nil
	})
	if len(secrets) == 0 {
		t.Fatal("the root key file was not found")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(tr)
		if hdr.Name != verify.InventoryName && hdr.Name != verify.GitName && !strings.HasPrefix(hdr.Name, "records/") {
			t.Fatalf("unexpected entry %s", hdr.Name)
		}
		// Signed receipts legitimately echo a workspace path in their frozen
		// result, so "runtime/" is only forbidden as an entry name.
		for _, bad := range []string{"index.db", "runtime/", "journal/", "JOURNAL-MARKER", "RUNTIME-MARKER", "TRUST-MARKER"} {
			if bad == "runtime/" && !strings.Contains(hdr.Name, bad) {
				continue
			}
			if strings.Contains(hdr.Name, bad) || bytes.Contains(buf.Bytes(), []byte(bad)) {
				i := bytes.Index(buf.Bytes(), []byte(bad))
				t.Fatalf("%s contains %q: %s", hdr.Name, bad, buf.Bytes()[max(0, i-200):min(len(buf.Bytes()), i+100)])
			}
		}
		for _, s := range secrets {
			if bytes.Contains(buf.Bytes(), s) {
				t.Fatalf("%s contains private key material", hdr.Name)
			}
		}
	}
}

func TestAC172_213_236_ReportShape(t *testing.T) {
	c := newChain(t, "false")
	path := c.mustExport()
	rep := verifyBundle(t, path)
	// A failing check is evidence, not an integrity failure.
	if rep.Integrity != verify.Verified {
		t.Fatalf("a failed check broke integrity: %s %v", rep.Integrity, rep.Invalid)
	}
	var failed int
	for _, e := range rep.Manifests[0].Evidence {
		if e.Result == "fail" && e.Integrity == verify.Verified && e.Category == "mechanical" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("the failed run must appear as an evidence result: %+v", rep.Manifests[0].Evidence)
	}
	raw, err := canon.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidateAPI("bundle.verify.output", raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"integrity"`, `"trust"`, `"missing":[]`, `"conflicts":[]`, `"invalid":[]`} {
		if !bytes.Contains(raw, []byte(k)) {
			t.Fatalf("report lacks %s: %s", k, raw)
		}
	}
	// Missing records are listed as missing, never as invalid.
	p := readParts(t, path)
	delete(p.Records, p.byKind(t, schema.KindCheckDefinition)[0])
	un := verifyBundle(t, p.write(t, true))
	if un.Integrity != verify.Unavailable || len(un.Missing) == 0 || len(un.Invalid) != 0 {
		t.Fatalf("unavailable report: %+v", un)
	}
	if raw, _ := canon.Marshal(un); schema.ValidateAPI("bundle.verify.output", raw) != nil {
		t.Fatal("the unavailable report violates its schema")
	}
}

func TestAC115_ImportStagingRejects(t *testing.T) {
	c := newChain(t, "true")
	good := c.mustExport()
	gp := readParts(t, good)
	inv, err := canon.Marshal(gp.Inv)
	if err != nil {
		t.Fatal(err)
	}
	invEntry := tarEntry{Name: verify.InventoryName, Data: inv}
	truncated := func() string {
		raw, _ := os.ReadFile(good)
		p := filepath.Join(t.TempDir(), "t.ledger")
		if err := os.WriteFile(p, raw[:len(raw)/2+7], 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	garbage := func() string {
		p := filepath.Join(t.TempDir(), "g.ledger")
		if err := os.WriteFile(p, bytes.Repeat([]byte("not a tar file "), 100), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mismatched := func() string {
		p := readParts(t, good)
		h := p.byKind(t, schema.KindIntent)[0]
		p.Records[strings.Repeat("0", 64)] = p.Records[h]
		delete(p.Records, h)
		return p.write(t, true)
	}
	badSignature := func() string {
		p := readParts(t, good)
		p.forge(t, p.byKind(t, schema.KindEvidenceAttestation)[0], func(env map[string]any) { env["signature"] = strings.Repeat("A", 86) })
		return p.write(t, true)
	}
	badSchema := func() string {
		p := readParts(t, good)
		p.forge(t, p.byKind(t, schema.KindIntent)[0], func(env map[string]any) { env["schema_version"] = 9 })
		return p.write(t, true)
	}
	noClosure := func() string {
		p := readParts(t, good)
		delete(p.Records, p.byKind(t, schema.KindSession)[0])
		return p.write(t, true)
	}
	oneRecord := tarEntry{Name: "records/ab/" + strings.Repeat("c", 62) + ".json", Data: []byte("{}")}
	cases := []struct {
		name, want string
		path       func() string
	}{
		{"traversal", "bundle_unsafe", func() string { return writeTar(t, []tarEntry{invEntry, {Name: "../evil", Data: []byte("x")}}) }},
		{"absolute", "bundle_unsafe", func() string { return writeTar(t, []tarEntry{invEntry, {Name: "/etc/passwd", Data: []byte("x")}}) }},
		{"backslash", "bundle_unsafe", func() string {
			return writeTar(t, []tarEntry{invEntry, {Name: "records\\ab\\x.json", Data: []byte("x")}})
		}},
		{"symlink", "bundle_unsafe", func() string {
			return writeTar(t, []tarEntry{invEntry, {Name: verify.GitName, Type: tar.TypeSymlink, Linkname: "/etc/passwd"}})
		}},
		{"hardlink", "bundle_unsafe", func() string {
			return writeTar(t, []tarEntry{invEntry, {Name: verify.GitName, Type: tar.TypeLink, Linkname: "bundle.json"}})
		}},
		{"duplicate", "bundle_unsafe", func() string { return writeTar(t, []tarEntry{invEntry, invEntry}) }},
		{"case alias", "bundle_unsafe", func() string {
			return writeTar(t, []tarEntry{invEntry, {Name: "bundle.JSON", Data: inv}})
		}},
		{"inventory not first", "bundle_unsafe", func() string { return writeTar(t, []tarEntry{oneRecord, invEntry}) }},
		{"unknown entry", "bundle_unsafe", func() string {
			return writeTar(t, []tarEntry{invEntry, {Name: "hooks/post-checkout", Data: []byte("#!/bin/sh")}})
		}},
		{"declared size beyond data", "bundle_corrupt", func() string {
			return writeTar(t, []tarEntry{invEntry, {Name: oneRecord.Name, Data: []byte("{}"), Size: 100}})
		}},
		{"truncated", "bundle_corrupt", truncated},
		{"truncated inside padding", "bundle_corrupt", func() string {
			// Cut just after the inventory's data, inside its block padding:
			// archive/tar alone reports a clean end there.
			raw := writeTar(t, []tarEntry{invEntry, oneRecord})
			b, _ := os.ReadFile(raw)
			cut := 512 + len(inv)
			if len(inv)%512 == 0 {
				t.Skip("inventory fills its blocks exactly")
			}
			p := filepath.Join(t.TempDir(), "pad.ledger")
			if err := os.WriteFile(p, b[:cut+1], 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"not a tar", "bundle_corrupt", garbage},
		{"hash differs from name", "bundle_invalid", mismatched},
		{"unsupported schema", "unsupported_schema_version", badSchema},
		{"tampered signature", "bundle_invalid", badSignature},
		{"missing reference closure", "bundle_incomplete", noClosure},
		{"partial export", "bundle_partial", func() string {
			p := filepath.Join(t.TempDir(), "x.ledger.partial")
			raw, _ := os.ReadFile(good)
			if err := os.WriteFile(p, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	}
	dst := newSite(t)
	objects, refs := dst.objectCount(), dst.refs()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verify.Import(context.Background(), tc.path(), noPublish{t}, dst.repo, verify.ImportOptions{})
			if got := code(t, err); got != tc.want {
				t.Fatalf("code %s, want %s: %v", got, tc.want, err)
			}
			dst.stagingEmpty()
			if dst.objectCount() != objects || dst.refs() != refs {
				t.Fatal("a rejected bundle changed the repository")
			}
		})
	}
	// Declared sizes over the limits are refused before reading the data.
	small := verify.DefaultLimits()
	small.MaxRecordBytes = 16
	dir := t.TempDir()
	if _, err := verify.Stage(context.Background(), good, dir, small); code(t, err) != "bundle_too_large" {
		t.Fatalf("record limit: %v", err)
	}
	small = verify.DefaultLimits()
	small.MaxEntries = 3
	if _, err := verify.Stage(context.Background(), good, t.TempDir(), small); code(t, err) != "bundle_too_large" {
		t.Fatalf("entry limit: %v", err)
	}
}

func FuzzBundleStage(f *testing.F) {
	inv := []byte(`{"bundle_version":1}`)
	seed := func(entries ...tarEntry) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, e := range entries {
			_ = tw.WriteHeader(&tar.Header{Name: e.Name, Mode: 0o600, Size: int64(len(e.Data)), Typeflag: tar.TypeReg})
			_, _ = tw.Write(e.Data)
		}
		_ = tw.Close()
		return buf.Bytes()
	}
	f.Add(seed(tarEntry{Name: verify.InventoryName, Data: inv}))
	f.Add(seed(tarEntry{Name: verify.InventoryName, Data: inv}, tarEntry{Name: "../x", Data: []byte("x")}))
	f.Add(seed(tarEntry{Name: verify.InventoryName, Data: inv}, tarEntry{Name: "records/ab/" + strings.Repeat("c", 62) + ".json", Data: []byte("{}")}))
	f.Add([]byte("not a tar"))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		file := filepath.Join(t.TempDir(), "f.ledger")
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		lim := verify.Limits{MaxEntries: 8, MaxRecordBytes: 1 << 16, MaxInventoryBytes: 1 << 16, MaxTotalBytes: 1 << 18, MaxGitBytes: 1 << 16}
		_, _ = verify.Stage(context.Background(), file, dir, lim)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != "objects.bundle" && e.Name() != "repo.git" {
				t.Fatalf("staging wrote %s", e.Name())
			}
		}
	})
}
