package core

import (
	"context"
	"testing"

	"github.com/CoachCoe/Ledger/internal/gitx"
)

func TestRewrittenCodeDropsRemovedDependencies(t *testing.T) {
	s := m4Service(t, map[string]string{"api.ts": "export function f() { return 1; }\n", "caller.ts": "import {f} from './api'; export function g() { return f(); }\n"})
	a := m4Session(t, s)
	w := m2Workspace(t, s, a)
	m4Edit(t, w, "caller.ts", "import {f} from './api'; export function g() { return 0; }\n")
	status := call(t, s, request("coordination.status", "", map[string]any{"workspace_id": w["workspace_id"]}))
	for _, raw := range status["analysis"].(map[string]any)["read"].([]any) {
		sym := raw.(map[string]any)
		if sym["path"] == "api.ts" && sym["name"] == "f" {
			t.Fatal("removed call remains in the current read set")
		}
	}
	proposal := call(t, s, request("workspace.propose", a["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"]}))
	manifest, err := s.Store.Index.Manifest(context.Background(), proposal["manifest"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, sym := range manifest.SymbolsRead {
		if sym.Path == "api.ts" && sym.Name == "f" {
			t.Fatal("removed call leaked into frozen provenance")
		}
	}
}

func TestUnchangedGitlinkDoesNotBlockSymbolAnalysis(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	sha, err := s.Store.Repo.ResolveRef(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@invalid"}
	for _, args := range [][]string{{"update-index", "--add", "--cacheinfo", "160000," + sha + ",vendor"}, {"commit", "-qm", "unchanged gitlink"}} {
		if _, err := gitx.Run(ctx, s.Store.Repo.Dir, nil, env, args...); err != nil {
			t.Fatal(err)
		}
	}
	a := m4Session(t, s)
	w := m2Workspace(t, s, a)
	m4Edit(t, w, "a.ts", "export const x=2;\n")
	call(t, s, request("coordination.status", "", map[string]any{"workspace_id": w["workspace_id"]}))
	call(t, s, request("workspace.propose", a["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"]}))
}

func TestBatchAcknowledgmentDoesNotPartiallyAcknowledgeAnotherQueue(t *testing.T) {
	s := fixture(t)
	a, b := m4Session(t, s), m4Session(t, s)
	wa, wb := m2Workspace(t, s, a), m2Workspace(t, s, b)
	m4Edit(t, wa, "a.ts", "export const x=2;\n")
	m4Edit(t, wb, "a.ts", "export const x=3;\n")
	if err := s.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	sidA, sidB := a["session_id"].(string), b["session_id"].(string)
	qa, qb := m4Warnings(t, s, sidA, "overlap"), m4Warnings(t, s, sidB, "overlap")
	if len(qa) != 1 || len(qb) != 1 {
		t.Fatal(qa, qb)
	}
	expectCode(t, s, "coordination.ack", sidA, map[string]any{"session_id": sidA, "notification_ids": []string{qa[0]["id"].(string), qb[0]["id"].(string)}}, "unknown_notification")
	if len(m4Warnings(t, s, sidA, "overlap")) != 1 || len(m4Warnings(t, s, sidB, "overlap")) != 1 {
		t.Fatal("rejected batch changed a recipient queue")
	}
	call(t, s, request("coordination.ack", sidA, map[string]any{"session_id": sidA, "notification_ids": []string{qa[0]["id"].(string)}}))
	if len(m4Warnings(t, s, sidA, "overlap")) != 0 {
		t.Fatal("pending queue returned acknowledged history")
	}
	stored, err := s.Store.Index.Notifications(context.Background(), sidA, "")
	if err != nil || len(stored) != 1 || stored[0].AcknowledgedAt == nil {
		t.Fatal("acknowledged history was not retained", stored, err)
	}
}
