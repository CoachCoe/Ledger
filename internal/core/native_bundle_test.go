package core

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/oplog"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/store"
)

// cloneNative copies s's repository as another machine would hold it:
// records, store and operation logs, but no index or runtime state.
func cloneNative(t *testing.T, s *Service) *Service {
	t.Helper()
	ctx := context.Background()
	src, dst := filepath.Dir(s.Store.Dir), t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		switch {
		case rel == filepath.Join(record.DirName, "runtime"):
			return filepath.SkipDir
		case strings.HasPrefix(rel, filepath.Join(record.DirName, "index.db")), rel == filepath.Join(record.DirName, "lock"):
			return nil
		case d.IsDir():
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), raw, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := record.OpenNative(ctx, dst, record.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	if _, err := rs.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := New(ctx, rs, s.Keys)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// nativeSelected proposes an edit in s, runs a check on it and records a
// selection. It returns the propose result.
func nativeSelected(t *testing.T, s *Service) map[string]any {
	t.Helper()
	writeChecks(t, s, []schema.CheckDefinition{unitCheck()})
	intent := call(t, s, requestV2("intent.create", "", checkIntent()))
	sess := call(t, s, requestV2("session.create", "", map[string]any{}))
	sid := sess["session_id"].(string)
	fork := call(t, s, requestV2("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": sess["capability_id"]}))
	if err := os.WriteFile(filepath.Join(fork["path"].(string), "a.ts"), []byte("export function x() { return 5 }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prop := call(t, s, requestV2("workspace.propose", sid, map[string]any{"workspace_id": fork["workspace_id"]}))
	resolved := call(t, s, requestV2("evidence.resolve", "", map[string]any{"manifest": prop["manifest"]}))
	def := resolved["checks"].([]any)[0].(map[string]any)["definition_hash"]
	run := call(t, s, requestV2("evidence.run", "", map[string]any{"manifest": prop["manifest"], "check_definition": def}))
	call(t, s, requestV2("selection.record", "", map[string]any{"manifest": prop["manifest"], "evidence": []any{run["attestation"]}}))
	return prop
}

func manifestReport(t *testing.T, rep map[string]any) map[string]any {
	t.Helper()
	ms := rep["manifests"].([]any)
	if len(ms) != 1 {
		t.Fatalf("%d manifests in %v", len(ms), rep)
	}
	return ms[0].(map[string]any)
}

// A native repository verifies, exports a v2 bundle that verifies on its
// own, and imports into a clone that then verifies the same manifest.
func TestNativeBundleRoundTrip(t *testing.T) {
	s, _ := nativeFixture(t)
	clone := cloneNative(t, s)
	prop := nativeSelected(t, s)

	rep := call(t, s, requestV2("bundle.verify", "", map[string]any{"trust": []string{}, "strict": true}))
	m := manifestReport(t, rep)
	if rep["integrity"] != "verified" || m["tree"] != prop["tree"] || m["op_head"] != prop["op_head"] || m["commit"] != nil {
		t.Fatalf("repository report %v", rep)
	}
	for _, c := range []string{"authority", "tree", "files", "oplog", "symbols", "evidence", "selections"} {
		if m["components"].(map[string]any)[c] != "verified" {
			t.Fatalf("component %s in %v", c, m["components"])
		}
	}

	out := filepath.Join(t.TempDir(), "native.ledger")
	exp := call(t, s, requestV2("bundle.export", "", map[string]any{"out": out, "manifests": []string{}, "intents": []string{}, "allow_partial": false}))
	if exp["complete"] != true || exp["size"].(map[string]any)["store_objects"].(float64) == 0 {
		t.Fatalf("export %v", exp)
	}
	rep = call(t, s, requestV2("bundle.verify", "", map[string]any{"bundle": out, "trust": []string{}, "strict": true}))
	if rep["integrity"] != "verified" || rep["bundle_version"].(float64) != schema.BundleVersionNative || manifestReport(t, rep)["tree"] != prop["tree"] {
		t.Fatalf("bundle report %v", rep)
	}

	imp := call(t, clone, requestV2("bundle.import", "", map[string]any{"bundle": out, "trust": []string{}}))["import"].(map[string]any)
	logs := imp["oplogs"].([]any)
	if imp["objects"].(float64) == 0 || len(logs) != 1 || logs[0].(map[string]any)["status"] != "created" {
		t.Fatalf("import %v", imp)
	}
	rep = call(t, clone, requestV2("bundle.verify", "", map[string]any{"trust": []string{}, "strict": true}))
	if rep["integrity"] != "verified" || manifestReport(t, rep)["hash"] != prop["manifest"] {
		t.Fatalf("clone report %v", rep)
	}
	again := call(t, clone, requestV2("bundle.import", "", map[string]any{"bundle": out, "trust": []string{}}))["import"].(map[string]any)
	if again["already_present"] != true {
		t.Fatalf("second import %v", again)
	}
}

// P2-AC-013: editing an operation log by hand breaks its chain, and the
// manifest that pinned it no longer verifies.
func TestNativeEditedOpLogFailsVerification(t *testing.T) {
	s, _ := nativeFixture(t)
	prop := nativeSelected(t, s)
	path := oplog.Path(filepath.Join(s.Store.Dir, "oplog"), prop["workspace_id"].(string))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := oplog.Decode(bytes.NewReader(raw))
	if err != nil || len(entries) < 2 {
		t.Fatalf("%d entries, %v", len(entries), err)
	}
	entries[0].Trigger = "edited"
	var edited []byte
	for _, e := range entries {
		line, err := canon.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		edited = append(append(edited, line...), '\n')
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	rep := call(t, s, requestV2("bundle.verify", "", map[string]any{"trust": []string{}, "strict": false}))
	m := manifestReport(t, rep)
	if rep["integrity"] != "invalid" || m["components"].(map[string]any)["oplog"] != "invalid" {
		t.Fatalf("report after the edit %v", rep)
	}
	if !slices.ContainsFunc(rep["invalid"].([]any), func(p any) bool {
		q := p.(map[string]any)
		return q["hash"] == prop["manifest"] && q["code"] == "oplog_broken"
	}) {
		t.Fatalf("no oplog_broken problem in %v", rep["invalid"])
	}
	_, err = s.Handle(context.Background(), requestV2("bundle.verify", "", map[string]any{"trust": []string{}, "strict": true}))
	if !apierr.HasCode(err, "verification_failed") {
		t.Fatalf("strict verify: %v", err)
	}
}

// A store object that is gone leaves the manifest unavailable, never
// invalid, and an export of it refuses to claim completeness.
func TestNativeMissingObjectIsUnavailable(t *testing.T) {
	s, _ := nativeFixture(t)
	prop := nativeSelected(t, s)
	files, err := s.Objects.Files(prop["tree"].(string))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(files, func(f store.File) bool { return f.Path == "a.ts" })
	id := files[i].ID
	if err := os.Remove(filepath.Join(s.Store.Dir, "store", "objects", id[:2], id[2:])); err != nil {
		t.Fatal(err)
	}
	rep := call(t, s, requestV2("bundle.verify", "", map[string]any{"trust": []string{}, "strict": false}))
	if c := manifestReport(t, rep)["components"].(map[string]any); rep["integrity"] != "unavailable" || c["tree"] != "unavailable" {
		t.Fatalf("report %v", rep)
	}
	out := filepath.Join(t.TempDir(), "partial.ledger")
	if _, err := s.Handle(context.Background(), requestV2("bundle.export", "", map[string]any{"out": out, "manifests": []string{}, "intents": []string{}, "allow_partial": false})); !apierr.HasCode(err, "export_incomplete") {
		t.Fatalf("export without the object: %v", err)
	}
}

// A bundle of one manifest carries its repository's record, so a clone
// imports it and another repository refuses it before writing anything.
func TestNativeImportRefusesAnotherRepository(t *testing.T) {
	s, _ := nativeFixture(t)
	clone := cloneNative(t, s)
	other, _ := nativeFixture(t)
	prop := nativeSelected(t, s)
	out := filepath.Join(t.TempDir(), "one.ledger")
	call(t, s, requestV2("bundle.export", "", map[string]any{"out": out, "manifests": []string{prop["manifest"].(string)}, "intents": []string{}, "allow_partial": false}))

	before := storeFiles(t, other)
	_, err := other.Handle(context.Background(), requestV2("bundle.import", "", map[string]any{"bundle": out, "trust": []string{}}))
	if !apierr.HasCode(err, "repository_mismatch") {
		t.Fatalf("import into another repository: %v", err)
	}
	if after := storeFiles(t, other); !slices.Equal(before, after) {
		t.Fatalf("refused import changed the store: %d -> %d objects", len(before), len(after))
	}
	imp := call(t, clone, requestV2("bundle.import", "", map[string]any{"bundle": out, "trust": []string{}}))["import"].(map[string]any)
	if imp["objects"].(float64) == 0 {
		t.Fatalf("clone import %v", imp)
	}
}

func storeFiles(t *testing.T, s *Service) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(s.Store.Dir, record.StoreDir), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
