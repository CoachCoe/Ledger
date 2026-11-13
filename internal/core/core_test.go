package core

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func fixture(t *testing.T) *Service { return fixtureWith(t, record.Options{}) }

func fixtureWith(t *testing.T, opts record.Options) *Service {
	t.Helper()
	s, _ := newFixture(t, nil, opts)
	return s
}

func request(op, sid string, input any) api.Request {
	raw, _ := canon.Marshal(input)
	return api.Request{Version: 2, Operation: op, SessionID: sid, OperationID: schema.NewID(schema.PrefixOperation), Input: raw}
}

func call(t *testing.T, s *Service, r api.Request) map[string]any {
	t.Helper()
	result, err := s.Handle(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := canon.Marshal(api.ReplyVersion(r.Version, result, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := api.ValidateResponseVersion(r.Version, r.Operation, wire); err != nil {
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

func TestInterruptedWorkspaceEffectsConverge(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	checkpoint := ""
	var interrupted record.Effect
	s.Store.SetEffectHandler(func(ctx context.Context, e record.Effect) error {
		if err := s.Provider.Apply(ctx, e); err != nil {
			return err
		}
		if e.Kind == checkpoint {
			checkpoint = ""
			interrupted = e
			return errors.New("interrupted after external effect")
		}
		return nil
	})
	session := call(t, s, request("session.create", "", map[string]any{}))
	sid := session["session_id"].(string)
	intent := call(t, s, request("intent.create", "", judgment("recover effects")))
	fork := request("workspace.fork", sid, map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]})
	checkpoint = record.EffectWorkspaceCreate
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
	closeRequest := request("workspace.close", sid, map[string]any{"workspace_id": ws["workspace_id"], "reason": "finished"})
	checkpoint = record.EffectWorkspaceRemove
	if _, err := s.Handle(ctx, closeRequest); !apierr.HasCode(err, "cleanup_pending") || checkpoint != "" {
		t.Fatalf("cleanup interruption missing: %v", err)
	}
	if _, err := s.Store.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	call(t, s, closeRequest)
	if _, err := os.Stat(ws["path"].(string)); !os.IsNotExist(err) {
		t.Fatal("checkout removal did not converge", err)
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
