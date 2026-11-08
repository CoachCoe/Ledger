package core

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func fixture(t *testing.T) *Service { return fixtureWith(t, record.Options{}) }

func fixtureWith(t *testing.T, opts record.Options) *Service {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	env := []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@invalid"}
	if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("export const x=1;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "a.ts"}, {"commit", "-qm", "base"}} {
		if _, err := gitx.Run(ctx, dir, nil, env, args...); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := gitx.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".ledger"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.CommonDir, "info", "exclude"), []byte("/.ledger/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	keys := identity.LocalKeys{Dir: filepath.Join(t.TempDir(), "keys")}
	root, _, err := keys.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	store, err := record.Open(ctx, repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	o, err := envelope.Seal(schema.KindRootKey, schema.RootKey{PublicKey: identity.EncodePublicKey(root.PublicKey()), CreatedAt: "2026-11-05T12:00:00Z"}, root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Do(ctx, record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "test.init", Digest: canon.Hash([]byte("init")), Signer: root}, func(*index.Index) (any, []envelope.Object, error) { return map[string]any{}, []envelope.Object{o}, nil })
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(ctx, store, keys)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func request(op, sid string, input any) api.Request {
	raw, _ := canon.Marshal(input)
	return api.Request{Version: 1, Operation: op, SessionID: sid, OperationID: schema.NewID(schema.PrefixOperation), Input: raw}
}

func call(t *testing.T, s *Service, r api.Request) map[string]any {
	t.Helper()
	result, err := s.Handle(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := canon.Marshal(api.Reply(result, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := api.ValidateResponse(r.Operation, wire); err != nil {
		t.Fatal(err)
	}
	raw, err := canon.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if v, ok := value["result"].(map[string]any); ok {
		return v
	}
	return value
}

func TestRevisionAndRestartRecovery(t *testing.T) {
	s := fixture(t)
	session := call(t, s, request("session.create", "", map[string]any{"agent_name": "test"}))
	sid := session["session_id"].(string)
	intent := call(t, s, request("intent.create", "", map[string]any{"description": "change x", "criteria": []any{map[string]any{"id": "review", "statement": "looks good", "verification": map[string]any{"kind": "judgment", "reviewer": "human"}}}}))
	r := request("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]})
	workspace := call(t, s, r)
	again := call(t, s, r)
	if workspace["workspace_id"] != again["workspace_id"] {
		t.Fatal("fork retry duplicated workspace")
	}
	wid, path := workspace["workspace_id"].(string), workspace["path"].(string)
	if err := os.WriteFile(filepath.Join(path, "a.ts"), []byte("export const x=2;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first := call(t, s, request("workspace.propose", sid, map[string]any{"workspace_id": wid}))
	call(t, s, request("workspace.rework", sid, map[string]any{"workspace_id": wid}))
	if err := os.WriteFile(filepath.Join(path, "a.ts"), []byte("export const x=3;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second := call(t, s, request("workspace.propose", sid, map[string]any{"workspace_id": wid}))
	if first["manifest"] == second["manifest"] {
		t.Fatal("rework did not make a new revision")
	}
	for _, proposal := range []map[string]any{first, second} {
		out, err := gitx.Run(context.Background(), path, nil, nil, "rev-list", "--parents", "-n1", proposal["commit"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if len(strings.Fields(string(out))) != 2 {
			t.Fatalf("not a sole parent: %s", out)
		}
	}
	restarted, err := New(context.Background(), s.Store, s.Keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Handle(context.Background(), request("workspace.rework", sid, map[string]any{"workspace_id": wid})); err == nil {
		t.Fatal("lost session can still sign")
	}
	recovered := call(t, restarted, request("session.recover", "", map[string]any{"workspace_id": wid}))
	if recovered["session_id"] == sid {
		t.Fatal("silently rotated old session")
	}
	if _, err := os.ReadFile(filepath.Join(path, "a.ts")); err != nil {
		t.Fatal("restart lost edits")
	}
}

func TestPinnedChecksLeafRulesAndUnauthorizedActor(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	definitions := []schema.CheckDefinition{{Name: "unit", Category: "mechanical", Command: "true", WorkingDirectory: ".", EnvAllowlist: []string{}, TimeoutSeconds: 10, OutputLimitBytes: 1024}}
	raw, err := canon.Marshal(definitions)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Store.Dir, "checks.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	criteria := []any{map[string]any{"id": "test", "statement": "passes", "verification": map[string]any{"kind": "check", "check": "unit"}}}
	req := request("intent.create", "", map[string]any{"description": "pin unit", "criteria": criteria})
	created := call(t, s, req)
	id := created["intent_id"].(string)
	before, err := creation[schema.Intent](ctx, s.Store.Index, id, schema.KindIntent)
	if err != nil {
		t.Fatal(err)
	}
	definitions[0].Command = "false"
	raw, _ = canon.Marshal(definitions)
	os.WriteFile(filepath.Join(s.Store.Dir, "checks.json"), raw, 0600)
	replayed := call(t, s, req)
	if replayed["intent_id"] != id {
		t.Fatal("retry changed intent")
	}
	after, err := creation[schema.Intent](ctx, s.Store.Index, id, schema.KindIntent)
	if err != nil {
		t.Fatal(err)
	}
	if before.Criteria[0].Verification.DefinitionHash != after.Criteria[0].Verification.DefinitionHash {
		t.Fatal("criterion retargeted")
	}
	a := call(t, s, request("session.create", "", map[string]any{}))
	b := call(t, s, request("session.create", "", map[string]any{}))
	sid := a["session_id"].(string)
	ws := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": id, "capability_id": a["capability_id"]}))
	wid := ws["workspace_id"].(string)
	if _, err := s.Handle(ctx, request("workspace.propose", b["session_id"].(string), map[string]any{"workspace_id": wid})); err == nil {
		t.Fatal("foreign actor proposed")
	}
	call(t, s, request("workspace.propose", sid, map[string]any{"workspace_id": wid}))
	if _, err := s.Handle(ctx, request("intent.create", "", map[string]any{"description": "child", "criteria": criteria, "parent_id": id})); err == nil {
		t.Fatal("used intent acquired a child")
	}
	// Submitting a different request under an existing operation id is denied.
	req.Input = []byte(`{"description":"different","criteria":[{"id":"x","statement":"x","verification":{"kind":"judgment","reviewer":"any"}}]}`)
	if _, err := s.Handle(ctx, req); !apierr.HasCode(err, "operation_mismatch") {
		t.Fatal(err)
	}
}

func TestRecoveryPendingWithoutRootPreservesFiles(t *testing.T) {
	s := fixture(t)
	session := call(t, s, request("session.create", "", map[string]any{}))
	sid := session["session_id"].(string)
	restarted, err := New(context.Background(), s.Store, unavailableKeys{})
	if err != nil {
		t.Fatal(err)
	}
	status := call(t, restarted, request("system.status", "", map[string]any{}))
	if status["recovery_pending"] != true {
		t.Fatal(status)
	}
	if _, err := restarted.Handle(context.Background(), request("intent.create", sid, map[string]any{"description": "x", "criteria": []any{map[string]any{"id": "x", "statement": "x", "verification": map[string]any{"kind": "judgment", "reviewer": "any"}}}})); err == nil {
		t.Fatal("lost session signed without root")
	}
}

type unavailableKeys struct{}

func (unavailableKeys) Root() (identity.Signer, error) {
	return nil, apierr.New("root_unavailable", "test root unavailable")
}

func TestClosePreservesIgnoredUserFiles(t *testing.T) {
	s := fixture(t)
	exclude := filepath.Join(s.Store.Repo.CommonDir, "info", "exclude")
	if err := os.WriteFile(exclude, []byte("/.ledger/\nsecret.env\n"), 0600); err != nil {
		t.Fatal(err)
	}
	session := call(t, s, request("session.create", "", map[string]any{}))
	sid := session["session_id"].(string)
	intent := call(t, s, request("intent.create", "", map[string]any{"description": "ignored data", "criteria": []any{map[string]any{"id": "review", "statement": "review", "verification": map[string]any{"kind": "judgment", "reviewer": "any"}}}}))
	ws := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]}))
	file := filepath.Join(ws["path"].(string), "secret.env")
	if err := os.WriteFile(file, []byte("preserve user data\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Handle(context.Background(), request("workspace.close", sid, map[string]any{"workspace_id": ws["workspace_id"], "reason": "finished"}))
	if !apierr.HasCode(err, "cleanup_pending") {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(file); err != nil {
		t.Fatal("ignored user data was deleted")
	}
	status := call(t, s, request("workspace.status", "", map[string]any{"workspace_id": ws["workspace_id"]}))
	if status["state"] != "closed" {
		t.Fatal(status)
	}
}

func TestCloseRemovesAProposedWorkspaceOnlyWhileItMatchesTheProposal(t *testing.T) {
	for name, edit := range map[string]string{"unchanged": "", "edited after proposal": "later edit\n"} {
		t.Run(name, func(t *testing.T) {
			s := fixture(t)
			session := call(t, s, request("session.create", "", map[string]any{}))
			sid := session["session_id"].(string)
			intent := call(t, s, request("intent.create", "", map[string]any{"description": "close", "criteria": []any{map[string]any{"id": "review", "statement": "review", "verification": map[string]any{"kind": "judgment", "reviewer": "any"}}}}))
			ws := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]}))
			path := ws["path"].(string)
			if err := os.WriteFile(filepath.Join(path, "refund.txt"), []byte("refund\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			call(t, s, request("workspace.propose", sid, map[string]any{"workspace_id": ws["workspace_id"]}))
			call(t, s, request("workspace.rework", sid, map[string]any{"workspace_id": ws["workspace_id"]}))
			if edit != "" {
				if err := os.WriteFile(filepath.Join(path, "refund.txt"), []byte(edit), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := s.Handle(context.Background(), request("workspace.close", sid, map[string]any{"workspace_id": ws["workspace_id"], "reason": "finished"}))
			if edit == "" {
				if err != nil {
					t.Fatalf("the proposal retains every file, so close removes the worktree: %v", err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("worktree still present: %v", err)
				}
				return
			}
			if !apierr.HasCode(err, "cleanup_pending") || apierr.As(err).Details["committed"] != true {
				t.Fatalf("an edit after the proposal is preserved: %v", err)
			}
			if raw, err := os.ReadFile(filepath.Join(path, "refund.txt")); err != nil || string(raw) != edit {
				t.Fatalf("edit lost: %q %v", raw, err)
			}
		})
	}
}

func TestMovedWorkspaceReconstructsWithoutRuntimeIndex(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	session := call(t, s, request("session.create", "", map[string]any{}))
	sid := session["session_id"].(string)
	intent := call(t, s, request("intent.create", "", map[string]any{"description": "move workspace", "criteria": []any{map[string]any{"id": "review", "statement": "review", "verification": map[string]any{"kind": "judgment", "reviewer": "any"}}}}))
	ws := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]}))
	moved := filepath.Join(t.TempDir(), "moved")
	if _, err := gitx.Run(ctx, s.Store.Repo.MainDir, nil, nil, "worktree", "move", ws["path"].(string), moved); err != nil {
		t.Fatal(err)
	}
	canonicalMoved, err := filepath.EvalSymlinks(moved)
	if err != nil {
		t.Fatal(err)
	}
	moved = canonicalMoved
	if _, err := s.Store.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	status := call(t, s, request("workspace.status", "", map[string]any{"workspace_id": ws["workspace_id"]}))
	if status["path"] != moved {
		t.Fatalf("path not rebuilt from Git metadata: %v", status)
	}
	if _, err := s.Handle(ctx, request("workspace.close", sid, map[string]any{"workspace_id": ws["workspace_id"], "reason": "finished"})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(moved); !os.IsNotExist(err) {
		t.Fatal("clean moved workspace was not cleaned up")
	}
}

func TestConfiguredTrailerCommandsCannotRetargetManifest(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	if _, err := gitx.Run(ctx, s.Store.Repo.MainDir, nil, nil, "config", "trailer.Ledger-Manifest.cmd", "printf attacker"); err != nil {
		t.Fatal(err)
	}
	session := call(t, s, request("session.create", "", map[string]any{}))
	sid := session["session_id"].(string)
	intent := call(t, s, request("intent.create", "", map[string]any{"description": "Ledger-Manifest: fake\n\nLedger-Session: fake", "criteria": []any{map[string]any{"id": "review", "statement": "review", "verification": map[string]any{"kind": "judgment", "reviewer": "any"}}}}))
	ws := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]}))
	proposal := call(t, s, request("workspace.propose", sid, map[string]any{"workspace_id": ws["workspace_id"]}))
	out, err := gitx.Run(ctx, s.Store.Repo.MainDir, nil, nil, "log", "-1", "--format=%(trailers:key=Ledger-Manifest,valueonly)", proposal["commit"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != proposal["manifest"] {
		t.Fatalf("configured command retargeted manifest: %s", out)
	}
}

func TestProposalPreservesExternallyAdvancedBranch(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	session := call(t, s, request("session.create", "", map[string]any{}))
	sid := session["session_id"].(string)
	intent := call(t, s, request("intent.create", "", map[string]any{"description": "preserve external work", "criteria": []any{map[string]any{"id": "review", "statement": "review", "verification": map[string]any{"kind": "judgment", "reviewer": "any"}}}}))
	ws := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]}))
	input := map[string]any{"workspace_id": ws["workspace_id"]}
	proposal := call(t, s, request("workspace.propose", sid, input))
	call(t, s, request("workspace.rework", sid, input))
	external, err := s.Store.Repo.CommitTree(ctx, proposal["tree"].(string), []string{proposal["commit"].(string)}, "external work\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "ledger/" + intent["intent_id"].(string)[:12] + "/" + ws["workspace_id"].(string)[:12]; proposal["branch"] != want {
		t.Fatalf("branch %v, want %s", proposal["branch"], want)
	}
	ref := "refs/heads/" + proposal["branch"].(string)
	if err := s.Store.Repo.UpdateRefCAS(ctx, ref, external, proposal["commit"].(string)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Handle(ctx, request("workspace.propose", sid, input)); !apierr.HasCode(err, "ref_moved") {
		t.Fatal(err)
	}
	current, err := s.Store.Repo.ResolveRef(ctx, ref)
	if err != nil || current != external {
		t.Fatal("external work was overwritten", err)
	}
}

func TestInterruptedWorkspaceEffectsConverge(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	checkpoint := ""
	var interrupted record.Effect
	s.Store.SetEffectHandler(func(ctx context.Context, e record.Effect) error {
		if err := s.Provider.Apply(ctx, e); err != nil {
			return err
		}
		match := e.Kind == checkpoint || checkpoint == "retention" && strings.HasPrefix(e.Ref, "refs/ledger/proposals/") || checkpoint == "branch" && strings.HasPrefix(e.Ref, "refs/heads/ledger/")
		if match {
			checkpoint = ""
			interrupted = e
			return errors.New("interrupted after external effect")
		}
		return nil
	})
	session := call(t, s, request("session.create", "", map[string]any{}))
	sid := session["session_id"].(string)
	intent := call(t, s, request("intent.create", "", map[string]any{"description": "recover effects", "criteria": []any{map[string]any{"id": "review", "statement": "review", "verification": map[string]any{"kind": "judgment", "reviewer": "any"}}}}))
	fork := request("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]})
	checkpoint = "worktree_add"
	if _, err := s.Handle(ctx, fork); err == nil || checkpoint != "" {
		t.Fatalf("fork interruption missing: %v", err)
	}
	file := filepath.Join(interrupted.Path, "a.ts")
	if err := os.WriteFile(file, []byte("edit after interrupted fork\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	ws := call(t, s, fork)
	if got, err := os.ReadFile(file); err != nil || string(got) != "edit after interrupted fork\n" {
		t.Fatal("fork recovery discarded edits", err)
	}
	input := map[string]any{"workspace_id": ws["workspace_id"]}
	var firstCommit string
	for _, stage := range []string{"retention", "identical", "branch"} {
		if stage != "retention" {
			call(t, s, request("workspace.rework", sid, input))
		}
		if stage == "branch" {
			if err := os.WriteFile(file, []byte("new revision\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		proposal := request("workspace.propose", sid, input)
		checkpoint = stage
		if stage == "identical" {
			checkpoint = "branch"
		}
		if _, err := s.Handle(ctx, proposal); err == nil || checkpoint != "" {
			t.Fatalf("%s interruption missing: %v", stage, err)
		}
		if _, err := s.Store.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		result := call(t, s, proposal)
		if firstCommit == "" {
			firstCommit = result["commit"].(string)
		} else if stage == "identical" && result["commit"] != firstCommit {
			t.Fatal("identical manifest lost its original commit binding")
		}
		retained, err := s.Store.Repo.ResolveRef(ctx, "refs/ledger/proposals/"+result["manifest"].(string))
		if err != nil || retained != result["commit"] {
			t.Fatalf("revision lost during %s recovery: %v", stage, err)
		}
	}
	if err := os.WriteFile(file, []byte("export const x=1;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	closeRequest := request("workspace.close", sid, map[string]any{"workspace_id": ws["workspace_id"], "reason": "finished"})
	checkpoint = "worktree_remove"
	if _, err := s.Handle(ctx, closeRequest); !apierr.HasCode(err, "cleanup_pending") || checkpoint != "" {
		t.Fatalf("cleanup interruption missing: %v", err)
	}
	if _, err := s.Store.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	call(t, s, closeRequest)
	if _, err := os.Stat(ws["path"].(string)); !os.IsNotExist(err) {
		t.Fatal("clean worktree cleanup did not converge", err)
	}
}

func judgment(description string) map[string]any {
	return map[string]any{"description": description, "criteria": []any{map[string]any{"id": "review", "statement": "review", "verification": map[string]any{"kind": "judgment", "reviewer": "any"}}}}
}

func TestIntentTreeAndSessionLifecycle(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	parent := call(t, s, request("intent.create", "", judgment("parent")))["intent_id"].(string)
	child := call(t, s, request("intent.create", "", map[string]any{"description": "child", "parent_id": parent, "criteria": judgment("")["criteria"]}))["intent_id"].(string)
	a := call(t, s, request("session.create", "", map[string]any{"participant_kind": "human"}))
	b := call(t, s, request("session.create", "", map[string]any{}))
	sid := a["session_id"].(string)
	fork := func(intent string) error {
		_, err := s.Handle(ctx, request("workspace.fork", sid, map[string]any{"intent_id": intent, "capability_id": a["capability_id"]}))
		return err
	}
	if err := fork(parent); !apierr.HasCode(err, "intent_not_leaf") {
		t.Fatalf("forked a parent intent: %v", err)
	}
	if _, err := s.Handle(ctx, request("workspace.fork", sid, map[string]any{"intent_id": child, "capability_id": b["capability_id"]})); !apierr.HasCode(err, "unauthorized") {
		t.Fatalf("forked with another session's capability: %v", err)
	}
	if _, err := s.Handle(ctx, request("intent.close", "", map[string]any{"intent_id": child, "reason": "superseded"})); err == nil {
		t.Fatal("superseded without a replacement")
	}
	call(t, s, request("intent.close", "", map[string]any{"intent_id": child, "reason": "superseded", "replacement_id": parent}))
	if err := fork(child); !apierr.HasCode(err, "invalid_state") {
		t.Fatalf("forked a closed intent: %v", err)
	}
	if _, err := s.Handle(ctx, request("session.end", b["session_id"].(string), map[string]any{"session_id": sid})); !apierr.HasCode(err, "unauthorized") {
		t.Fatalf("one session ended another: %v", err)
	}
	call(t, s, request("session.end", sid, map[string]any{"session_id": sid}))
	if err := fork(parent); !apierr.HasCode(err, "session_key_unavailable") {
		t.Fatalf("ended session still signs: %v", err)
	}
	status := call(t, s, request("system.status", "", map[string]any{}))
	for _, sub := range status["subjects"].([]any) {
		if sub := sub.(map[string]any); sub["id"] == sid && sub["state"] != "ended" {
			t.Fatalf("session not ended: %v", sub)
		}
	}
	session, err := creation[schema.Session](ctx, s.Store.Index, sid, schema.KindSession)
	if err != nil || session.ParticipantKind != "human" {
		t.Fatalf("participant kind not recorded: %+v %v", session, err)
	}
}

// Observation runs outside the mutation lock; this exercises it alongside
// status reads, session-triggered refreshes and proposals under -race.
func TestObservationRunsConcurrentlyWithRequests(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	session := call(t, s, request("session.create", "", map[string]any{}))
	sid := session["session_id"].(string)
	var workspaces []string
	for range 3 {
		intent := call(t, s, request("intent.create", "", judgment("concurrent")))
		ws := call(t, s, request("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]}))
		workspaces = append(workspaces, ws["workspace_id"].(string))
		if err := os.WriteFile(filepath.Join(ws["path"].(string), "new.ts"), []byte("export const y=1;\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 4 {
		wg.Go(func() {
			if err := s.Observe(ctx); err != nil {
				errs <- err
			}
		})
	}
	for _, id := range workspaces {
		wg.Go(func() {
			if _, err := s.Handle(ctx, request("workspace.status", "", map[string]any{"workspace_id": id})); err != nil {
				errs <- err
			}
		})
	}
	wg.Go(func() {
		if _, err := s.Handle(ctx, request("workspace.propose", sid, map[string]any{"workspace_id": workspaces[0]})); err != nil {
			errs <- err
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, id := range workspaces {
		status := call(t, s, request("workspace.status", "", map[string]any{"workspace_id": id}))
		if !slices.Equal(anyStrings(status["write_set"]), []string{"new.ts"}) {
			t.Fatalf("write set %v", status["write_set"])
		}
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}
