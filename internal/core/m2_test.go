package core

import (
	"context"
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
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/verify"
)

func m2Workspace(t *testing.T, s *Service, session map[string]any) map[string]any {
	t.Helper()
	intent := call(t, s, request("intent.create", "", map[string]any{"description": "M2 scope enforcement", "criteria": []any{map[string]any{"id": "review", "statement": "review", "verification": map[string]any{"kind": "judgment", "reviewer": "human"}}}}))
	return call(t, s, request("workspace.fork", session["session_id"].(string), map[string]any{"intent_id": intent["intent_id"], "capability_id": session["capability_id"]}))
}
func expectCode(t *testing.T, s *Service, op, sid string, in any, code string) {
	t.Helper()
	_, err := s.Handle(context.Background(), request(op, sid, in))
	if err == nil || apierr.As(err).Code != code {
		t.Fatalf("%s: expected %s, got %v", op, code, err)
	}
}
func TestM2ScopeObservationAndExactProposal(t *testing.T) {
	// AC-153–154, AC-158–159, AC-206: stale cache never authorizes a proposal.
	s := fixture(t)
	session := call(t, s, request("session.create", "", map[string]any{"allow": []string{"src/**"}, "deny": []string{"src/private/**"}, "may_add_dependencies": false}))
	sid := session["session_id"].(string)
	w := m2Workspace(t, s, session)
	wid, dir := w["workspace_id"].(string), w["path"].(string)
	if err := os.MkdirAll(filepath.Join(dir, "src", "private"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "ok.ts"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	call(t, s, request("workspace.status", sid, map[string]any{"workspace_id": wid}))
	for _, p := range []string{"a.ts", "src/private/no.ts", "src/package.json"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	expectCode(t, s, "workspace.propose", sid, map[string]any{"workspace_id": wid}, "scope_violation")
	queue := call(t, s, request("coordination.notifications", sid, map[string]any{"session_id": sid}))
	n := queue["notifications"].([]any)
	if len(n) != 3 {
		t.Fatalf("warnings: %v", n)
	}
	for _, p := range []string{"src/private/no.ts", "src/package.json"} {
		if err := os.Remove(filepath.Join(dir, p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("export const x=1;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	call(t, s, request("workspace.propose", sid, map[string]any{"workspace_id": wid}))
}
func TestM2QueueRecovery(t *testing.T) {
	// QueueRecovery: ordinary restart preserves delivery and ack, reindex reports
	// the gap and regenerates current warnings with the original recipient.
	ctx := context.Background()
	s := fixture(t)
	session := call(t, s, request("session.create", "", map[string]any{"allow": []string{"src/**"}}))
	sid := session["session_id"].(string)
	w := m2Workspace(t, s, session)
	if err := os.WriteFile(filepath.Join(w["path"].(string), "a.ts"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	q := call(t, s, request("coordination.notifications", sid, map[string]any{"session_id": sid}))
	n := q["notifications"].([]any)
	if len(n) != 1 {
		t.Fatal(n)
	}
	id := n[0].(map[string]any)["id"]

	// A cursor is inclusive: entries created in the same timestamp bucket
	// cannot disappear between successive polls. Clients deduplicate by id.
	cursor := n[0].(map[string]any)["created_at"]
	sameTime := call(t, s, request("coordination.notifications", sid, map[string]any{"session_id": sid, "since": cursor}))
	if len(sameTime["notifications"].([]any)) != 1 {
		t.Fatal("timestamp cursor skipped matching entry")
	}
	for range 2 {
		call(t, s, request("coordination.ack", sid, map[string]any{"session_id": sid, "notification_id": id}))
	}
	other := call(t, s, request("session.create", "", map[string]any{}))["session_id"].(string)
	expectCode(t, s, "coordination.notifications", other, map[string]any{"session_id": sid}, "unauthorized")
	restarted, err := New(ctx, s.Store, s.Keys)
	if err != nil {
		t.Fatal(err)
	}
	q = call(t, restarted, request("coordination.notifications", "", map[string]any{"session_id": sid}))
	n = q["notifications"].([]any)
	if len(n) != 1 || n[0].(map[string]any)["acknowledged_at"] == nil {
		t.Fatal("queue lost on restart", n)
	}
	report, err := s.Store.Reindex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.DiscardedRuntime["notifications"] != 1 {
		t.Fatal("missing delivery gap", report)
	}
	if err := restarted.Observe(ctx); err != nil {
		t.Fatal(err)
	}
	q = call(t, restarted, request("coordination.notifications", "", map[string]any{"session_id": sid}))
	if len(q["notifications"].([]any)) != 1 {
		t.Fatal("warning not regenerated")
	}
}
func TestM2DelegationAndNarrowRecovery(t *testing.T) {
	// AC-151, AC-163–165, AC-105: delegation and replacement authority.
	s := fixture(t)
	parent := call(t, s, request("session.create", "", map[string]any{"allow": []string{"src/**"}, "deny": []string{"src/private/**"}, "may_add_dependencies": false}))
	child := call(t, s, request("session.create", "", map[string]any{}))
	in := map[string]any{"session_id": child["session_id"], "parent_capability_id": parent["capability_id"], "allow": []string{"src/**"}, "deny": []string{}, "may_add_dependencies": false, "expires_at": schema.Timestamp(time.Now().Add(time.Hour))}
	sid := parent["session_id"].(string)
	delegated := call(t, s, request("capability.delegate", sid, in))
	child["capability_id"] = delegated["capability_id"]
	w := m2Workspace(t, s, child)
	dir := w["path"].(string)
	if err := os.MkdirAll(filepath.Join(dir, "src", "private"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "private", "x"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	expectCode(t, s, "workspace.propose", child["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"]}, "scope_violation")
	for _, change := range []map[string]any{{"allow": []string{"**"}}, {"may_add_dependencies": true}, {"expires_at": "2099-01-01T00:00:00Z"}} {
		original := map[string]any{}
		for k, v := range in {
			original[k] = v
		}
		for k, v := range change {
			original[k] = v
		}
		expectCode(t, s, "capability.delegate", sid, original, "capability_widening")
	}
	recovered := call(t, s, request("session.recover", "", map[string]any{"workspace_id": w["workspace_id"], "allow": []string{"src/public/**"}, "may_add_dependencies": false}))
	expectCode(t, s, "workspace.propose", recovered["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"]}, "scope_violation")
}
func TestM2SymlinksUseFrozenTrees(t *testing.T) {
	// AC-161, AC-244: links, chained links, external targets, and deletions.
	for _, target := range []string{"../a.ts", "../../outside", "/tmp/outside", "private/no.ts", "chain"} {
		t.Run(target, func(t *testing.T) {
			s := fixture(t)
			session := call(t, s, request("session.create", "", map[string]any{"allow": []string{"src/**"}, "deny": []string{"src/private/**"}}))
			w := m2Workspace(t, s, session)
			dir := w["path"].(string)
			if err := os.Mkdir(filepath.Join(dir, "src"), 0700); err != nil {
				t.Fatal(err)
			}
			if target == "chain" {
				if err := os.Symlink("../../outside", filepath.Join(dir, "src", "chain")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(dir, "src", "link")); err != nil {
				t.Fatal(err)
			}
			expectCode(t, s, "workspace.propose", session["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"]}, "scope_violation")
		})
	}
}

func TestM2HistoricalSignatureChainAndTrust(t *testing.T) {
	// AC-098, AC-165, AC-236: verification needs no index, and distinguishes trust.
	s := fixture(t)
	parent := call(t, s, request("session.create", "", map[string]any{"allow": []string{"src/**"}}))
	child := call(t, s, request("session.create", "", map[string]any{}))
	delegated := call(t, s, request("capability.delegate", parent["session_id"].(string), map[string]any{"session_id": child["session_id"], "parent_capability_id": parent["capability_id"], "allow": []string{"src/**"}, "deny": []string{}, "may_add_dependencies": true, "expires_at": schema.Timestamp(time.Now().Add(time.Hour))}))
	child["capability_id"] = delegated["capability_id"]
	w := m2Workspace(t, s, child)
	dir := w["path"].(string)
	if err := os.Mkdir(filepath.Join(dir, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "ok"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	proposal := call(t, s, request("workspace.propose", child["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"]}))
	call(t, s, request("session.recover", "", map[string]any{"workspace_id": w["workspace_id"], "allow": []string{"other/**"}}))
	files, err := filepath.Glob(filepath.Join(s.Store.Dir, "objects", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	var records []envelope.Object
	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		o, err := envelope.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, o)
	}
	root, err := s.Keys.Root()
	if err != nil {
		t.Fatal(err)
	}
	hash := proposal["manifest"].(string)
	for _, trusted := range []bool{true, false} {
		roots := []string{}
		if trusted {
			roots = append(roots, root.ID())
		}
		result := verify.Manifest(records, hash, roots, true)
		if result.Integrity != "verified" || (result.Trust == "trusted") != trusted {
			t.Fatalf("verify: %+v", result)
		}
	}
	for i, o := range records {
		if o.Hash == hash {
			tampered := append([]envelope.Object{}, records...)
			tampered[i].Bytes = append([]byte{}, o.Bytes...)
			tampered[i].Bytes[len(o.Bytes)-3] ^= 1
			if verify.Manifest(tampered, hash, []string{root.ID()}, true).Integrity != "invalid" {
				t.Fatal("tamper accepted")
			}
		}
	}

	for i, o := range records {
		if o.Envelope.Kind == schema.KindSession {
			var original schema.Session
			if err := o.Decode(&original); err != nil {
				t.Fatal(err)
			}
			if original.ID != child["session_id"] {
				continue
			}
			replacement, err := identity.NewSession(original.ID)
			if err != nil {
				t.Fatal(err)
			}
			original.PublicKey = identity.EncodePublicKey(replacement.PublicKey())
			forged, err := envelope.Seal(schema.KindSession, original, root)
			if err != nil {
				t.Fatal(err)
			}
			substituted := append([]envelope.Object{}, records...)
			substituted[i] = forged
			if verify.Manifest(substituted, hash, []string{root.ID()}, true).Integrity != "invalid" {
				t.Fatal("substituted session key accepted")
			}
		}
	}

	// Root-authenticated revocation before the recorded proposal must fail;
	// an end after it must preserve the same historical signature.
	for _, o := range records {
		if o.Envelope.Kind != schema.KindSession {
			continue
		}
		var session schema.Session
		if err := o.Decode(&session); err != nil {
			t.Fatal(err)
		}
		if session.ID != child["session_id"] {
			continue
		}
		started, err := schema.ParseTimestamp(session.StartedAt)
		if err != nil {
			t.Fatal(err)
		}
		for _, before := range []bool{true, false} {
			endedAt := time.Now().Add(time.Hour)
			if before {
				endedAt = started.Add(time.Millisecond)
			}
			reason := "test_revocation"
			event, err := envelope.Seal(schema.KindLifecycleEvent, schema.LifecycleEvent{SubjectKind: "session", SubjectID: session.ID, Creation: o.Hash, FromState: "active", ToState: "ended", Actor: root.ID(), At: schema.Timestamp(endedAt), Reason: &reason}, root)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := envelope.Seal(schema.KindOperationReceipt, schema.OperationReceipt{OperationID: schema.NewID(schema.PrefixOperation), Operation: "session.end", RequestDigest: canon.Hash(nil), Members: []string{event.Hash}, Result: []byte("{}"), At: schema.Timestamp(endedAt)}, root)
			if err != nil {
				t.Fatal(err)
			}
			revoked := append(append([]envelope.Object{}, records...), event, receipt)
			result := verify.Manifest(revoked, hash, []string{root.ID()}, true)
			if before && (result.Integrity != "invalid" || result.Error == nil || result.Error.Code != "session_inactive") {
				t.Fatalf("revoked session verified: %+v", result)
			}
			if !before && result.Integrity != "verified" {
				t.Fatalf("later session end invalidated history: %+v", result)
			}
		}
	}
	missing := []envelope.Object{}
	for _, o := range records {
		if o.Envelope.Kind != schema.KindCapability {
			missing = append(missing, o)
		}
	}
	if verify.Manifest(missing, hash, nil, true).Integrity != "unavailable" {
		t.Fatal("missing chain not unavailable")
	}
	if _, err := s.Store.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestM2RecoveryPredecessorConflictAndTerminal(t *testing.T) {
	s := fixture(t)
	session := call(t, s, request("session.create", "", map[string]any{}))
	w := m2Workspace(t, s, session)
	wid := w["workspace_id"].(string)
	sub, err := s.Store.Index.Subject(context.Background(), wid)
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"workspace_id": wid, "previous_binding": sub.BindingHead, "allow": []string{"a.ts"}}
	first := call(t, s, request("session.recover", "", in))
	expectCode(t, s, "session.recover", "", in, "stale_predecessor")
	call(t, s, request("workspace.close", first["session_id"].(string), map[string]any{"workspace_id": wid, "reason": "abandoned"}))
	expectCode(t, s, "session.recover", "", map[string]any{"workspace_id": wid}, "invalid_state")
}

func TestM2DependencyDiffKinds(t *testing.T) {
	// AC-109, AC-108: inspect the base-relative diff, including both rename
	// sides, deleted manifests, agent commits and mode-only changes.
	for _, kind := range []string{"add", "modify", "delete", "rename-from", "rename-to", "mode", "agent-commit"} {
		t.Run(kind, func(t *testing.T) {
			s := fixture(t)
			ctx := context.Background()
			repo := s.Store.Repo.MainDir
			if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			env := []string{"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@invalid"}
			for _, args := range [][]string{{"add", "package.json"}, {"commit", "-qm", "manifest"}} {
				if _, err := gitx.Run(ctx, repo, nil, env, args...); err != nil {
					t.Fatal(err)
				}
			}
			session := call(t, s, request("session.create", "", map[string]any{"may_add_dependencies": false}))
			w := m2Workspace(t, s, session)
			dir := w["path"].(string)
			manifest := filepath.Join(dir, "package.json")
			switch kind {
			case "add":
				if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("foo"), 0600); err != nil {
					t.Fatal(err)
				}
			case "modify", "agent-commit":
				if err := os.WriteFile(manifest, []byte("{\"new\":true}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := os.Remove(manifest); err != nil {
					t.Fatal(err)
				}
			case "rename-from":
				if err := os.Rename(manifest, filepath.Join(dir, "ordinary.txt")); err != nil {
					t.Fatal(err)
				}
			case "rename-to":
				if err := os.Rename(filepath.Join(dir, "a.ts"), filepath.Join(dir, "requirements-dev.txt")); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(manifest, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "agent-commit" {
				for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "agent"}} {
					if _, err := gitx.Run(ctx, dir, nil, env, args...); err != nil {
						t.Fatal(err)
					}
				}
			}
			expectCode(t, s, "workspace.propose", session["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"]}, "scope_violation")
			q := call(t, s, request("coordination.notifications", session["session_id"].(string), map[string]any{"session_id": session["session_id"]}))
			if len(q["notifications"].([]any)) == 0 {
				t.Fatal("no dependency warning")
			}
		})
	}
}

func TestM2QueueFaultBoundaries(t *testing.T) {
	for _, stage := range []string{"enqueue_before", "enqueue_after", "ack_before", "ack_after"} {
		t.Run(stage, func(t *testing.T) {
			s := fixture(t)
			sid := call(t, s, request("session.create", "", map[string]any{}))["session_id"].(string)
			ctx := context.Background()
			n := index.Notification{ID: schema.NewID("ntf_"), Recipient: sid, Kind: "scope_violation", Subject: index.NotificationSubject{Path: "a.ts"}, Confidence: "file_level", CreatedAt: now()}
			injected := errors.New("simulated response loss")
			s.Store.Index.NotificationFault = func(at string) error {
				if at == stage {
					return injected
				}
				return nil
			}
			if strings.HasPrefix(stage, "enqueue") {
				if err := s.Store.Index.Enqueue(ctx, n); !errors.Is(err, injected) {
					t.Fatalf("missing fault %v", err)
				}
			} else {
				if err := s.Store.Index.Enqueue(ctx, n); err != nil {
					t.Fatal(err)
				}
				if err := s.Store.Index.Acknowledge(ctx, sid, n.ID, now()); !errors.Is(err, injected) {
					t.Fatalf("missing fault %v", err)
				}
			}
			s.Store.Index.NotificationFault = nil
			restarted, err := New(ctx, s.Store, s.Keys)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Store.Index.Enqueue(ctx, n); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := restarted.Store.Index.Acknowledge(ctx, sid, n.ID, now()); err != nil {
					t.Fatal(err)
				}
			}
			queue, err := restarted.Store.Index.Notifications(ctx, sid, "")
			if err != nil || len(queue) != 1 || queue[0].AcknowledgedAt == nil {
				t.Fatalf("queue recovery: %v %v", queue, err)
			}
		})
	}
}

func TestM2ConcurrentRecoveryOnePredecessor(t *testing.T) {
	s := fixture(t)
	session := call(t, s, request("session.create", "", map[string]any{}))
	w := m2Workspace(t, s, session)
	sub, err := s.Store.Index.Subject(context.Background(), w["workspace_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"workspace_id": w["workspace_id"], "previous_binding": sub.BindingHead}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { _, errs[i] = s.Handle(context.Background(), request("session.recover", "", in)) })
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !apierr.HasCode(err, "stale_predecessor") {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d accepted concurrent bindings", wins)
	}
}

func TestM2RootIssuanceAndPatternErrors(t *testing.T) {
	s := fixture(t)
	a := call(t, s, request("session.create", "", map[string]any{}))
	b := call(t, s, request("session.create", "", map[string]any{"allow": []string{"a.ts"}}))
	in := map[string]any{"session_id": b["session_id"], "allow": []string{"src/**"}, "deny": []string{}, "may_add_dependencies": false, "expires_at": schema.Timestamp(time.Now().Add(time.Hour))}
	call(t, s, request("capability.issue", "", in))
	expectCode(t, s, "capability.issue", a["session_id"].(string), in, "unauthorized")
	expectCode(t, s, "session.create", "", map[string]any{"allow": []string{"src/*"}}, "invalid_pattern")
}
