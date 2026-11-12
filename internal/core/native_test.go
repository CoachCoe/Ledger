package core

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

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
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("export const x=1;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, record.DirName)
	if err := os.Mkdir(ledger, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ledgerignore"), []byte("/"+record.DirName+"/\n"), 0600); err != nil {
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
