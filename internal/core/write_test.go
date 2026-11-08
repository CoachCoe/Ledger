package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/CoachCoe/Ledger/internal/canon"
)

func TestWorkspaceWriteScopeConflictAndMetadata(t *testing.T) {
	s := fixture(t)
	a := call(t, s, request("session.create", "", map[string]any{"allow": []string{"src/**"}}))
	w := m2Workspace(t, s, a)
	sid := a["session_id"].(string)
	in := map[string]any{"workspace_id": w["workspace_id"], "path": "src/new.ts", "content": "export const n=1;\n", "expected_hash": nil}
	for range 2 {
		call(t, s, request("workspace.write", sid, in))
	}
	in["content"] = "changed"
	expectCode(t, s, "workspace.write", sid, in, "content_conflict")
	in["expected_hash"] = canon.Hash([]byte("export const n=1;\n"))
	call(t, s, request("workspace.write", sid, in))
	in["path"] = "a.ts"
	expectCode(t, s, "workspace.write", sid, in, "scope_violation")
	in["path"] = "src/../a.ts"
	expectCode(t, s, "workspace.write", sid, in, "invalid_path")
	in["path"] = "src/.git/config"
	expectCode(t, s, "workspace.write", sid, in, "invalid_path")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(w["path"].(string), "src", "link")); err != nil {
		t.Fatal(err)
	}
	in["path"] = "src/link/escape"
	expectCode(t, s, "workspace.write", sid, in, "invalid_path")
	if _, err := os.Stat(filepath.Join(outside, "escape")); !os.IsNotExist(err) {
		t.Fatal("write escaped workspace")
	}
	if err := syscall.Mkfifo(filepath.Join(w["path"].(string), "src", "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	in["path"] = "src/pipe"
	expectCode(t, s, "workspace.write", sid, in, "invalid_path")
	if err := os.Remove(filepath.Join(w["path"].(string), "src", "pipe")); err != nil {
		t.Fatal(err)
	}
	other := call(t, s, request("session.create", "", map[string]any{}))
	in["path"] = "src/other.ts"
	expectCode(t, s, "workspace.write", other["session_id"].(string), in, "unauthorized")
}

func TestWorkspaceWriteRestart(t *testing.T) {
	for _, stage := range []string{"prepared", "applied", "done"} {
		t.Run(stage, func(t *testing.T) {
			s := fixture(t)
			a := call(t, s, request("session.create", "", map[string]any{}))
			w := m2Workspace(t, s, a)
			r := request("workspace.write", a["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"], "path": "new.ts", "content": "export const n=1;\n", "expected_hash": nil})
			s.writeFault = func(at string) error {
				if at == stage {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := s.Handle(context.Background(), r); err == nil {
				t.Fatal("fault not injected")
			}
			restarted, err := New(context.Background(), s.Store, s.Keys)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "done" {
				call(t, restarted, r)
			} else {
				expectErr(t, restarted, r, "unauthorized")
			}
		})
	}
}

func TestWorkspaceWriteInterruptedRetry(t *testing.T) {
	for _, stage := range []string{"prepared", "applied", "done"} {
		t.Run(stage, func(t *testing.T) {
			s := fixture(t)
			a := call(t, s, request("session.create", "", map[string]any{}))
			w := m2Workspace(t, s, a)
			r := request("workspace.write", a["session_id"].(string), map[string]any{"workspace_id": w["workspace_id"], "path": "new.ts", "content": "export const n=1;\n", "expected_hash": nil})
			s.writeFault = func(at string) error {
				if at == stage {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := s.Handle(context.Background(), r); err == nil {
				t.Fatal("fault not injected")
			}
			s.writeFault = nil
			first := call(t, s, r)
			if err := os.WriteFile(filepath.Join(w["path"].(string), "new.ts"), []byte("later edit"), 0600); err != nil {
				t.Fatal(err)
			}
			after := call(t, s, r)
			b, _ := canon.Marshal(first)
			c, _ := canon.Marshal(after)
			if string(b) != string(c) {
				t.Fatal("retry changed observation")
			}
			content, err := os.ReadFile(filepath.Join(w["path"].(string), "new.ts"))
			if err != nil || string(content) != "later edit" {
				t.Fatal("retry overwrote later edit")
			}
			changed := r
			changed.Input = []byte(`{"workspace_id":"` + w["workspace_id"].(string) + `","path":"new.ts","content":"different","expected_hash":null}`)
			expectErr(t, s, changed, "operation_conflict")
		})
	}
}
