package core

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/workspace"
)

func (s *Service) writeText(ctx context.Context, r api.Request) (any, error) {
	in, err := decode[struct {
		WorkspaceID  string  `json:"workspace_id"`
		Path         string  `json:"path"`
		Content      string  `json:"content"`
		ExpectedHash *string `json:"expected_hash"`
	}](r.Input)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	digest, err := r.Digest()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.runtimeDir(), "edits")
	file := filepath.Join(dir, r.OperationID+".json")
	var journal struct {
		Digest  string         `json:"digest"`
		Applied bool           `json:"applied"`
		Result  map[string]any `json:"result"`
	}
	if raw, e := os.ReadFile(file); e == nil {
		if err := json.Unmarshal(raw, &journal); err != nil {
			return nil, err
		}
		if journal.Digest != digest {
			return nil, apierr.New("operation_conflict", "edit operation id has different input")
		}
		if journal.Applied {
			return journal.Result, nil
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if r.SessionID == "" || s.signers[r.SessionID] == nil {
		return nil, apierr.New("unauthorized", "editing requires the bound local session")
	}
	sub, w, cid, err := s.boundWorkspace(ctx, in.WorkspaceID, r.SessionID)
	if err != nil {
		return nil, err
	}
	if sub.State != "open" {
		return nil, apierr.New("invalid_state", "only open workspaces can be edited")
	}
	if err := s.authority(ctx, r.SessionID, cid); err != nil {
		return nil, err
	}
	chain, err := s.chain(ctx, cid, time.Now())
	if err != nil {
		return nil, err
	}
	if err := capability.Authorize(chain, in.Path, s.caseSensitive, time.Now()); err != nil {
		return nil, err
	}
	m, _ := s.observing.LoadOrStore(w.ID, new(sync.Mutex))
	observation := m.(*sync.Mutex)
	observation.Lock()
	defer observation.Unlock()
	writer, ok := s.Provider.(workspace.TextWriter)
	if !ok {
		return nil, apierr.New("unsupported_operation", "workspace provider has no text writer")
	}
	// Runtime journals preserve retry identity across interrupted replacement.
	// They contain only request digests and unsigned observations, never keys or
	// source text, and are deliberately outside portable record history.
	if err := atomicfile.MakeDir(dir, 0700); err != nil {
		return nil, err
	}
	if journal.Digest == "" {
		journal.Digest = digest
		b, err := canon.Marshal(journal)
		if err != nil {
			return nil, err
		}
		if err := atomicfile.Write(file, b, 0600); err != nil {
			return nil, err
		}
	}
	fault := func(stage string) error {
		if s.writeFault != nil {
			return s.writeFault(stage)
		}
		return nil
	}
	if err := fault("prepared"); err != nil {
		return nil, err
	}
	if _, err := writer.WriteText(ctx, w, in.Path, in.Content, in.ExpectedHash); err != nil {
		return nil, err
	}
	if err := fault("applied"); err != nil {
		return nil, err
	}
	result, err := runtimeResult(map[string]any{"unsigned": true, "workspace_id": w.ID, "path": in.Path, "content_hash": canon.Hash([]byte(in.Content))})
	if err != nil {
		return nil, err
	}
	journal.Result = result.(map[string]any)
	journal.Applied = true
	b, err := canon.Marshal(journal)
	if err != nil {
		return nil, err
	}
	if err := atomicfile.Write(file, b, 0600); err != nil {
		return nil, err
	}
	if err := fault("done"); err != nil {
		return nil, err
	}
	return journal.Result, nil
}

// detachDeps gives a native workspace private, writable copies of its shared
// dependency trees, so installing into them changes no other workspace
// (ADR 0026). Repeating it changes nothing.
func (s *Service) detachDeps(ctx context.Context, r api.Request) (any, error) {
	in, err := decode[struct {
		WorkspaceID string  `json:"workspace_id"`
		Path        *string `json:"path"`
	}](r.Input)
	if err != nil {
		return nil, err
	}
	native, ok := s.Provider.(workspace.NativeProvider)
	if !ok {
		return nil, apierr.New("unsupported_operation", "only native workspaces share dependency trees")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.SessionID == "" || s.signers[r.SessionID] == nil {
		return nil, apierr.New("unauthorized", "detaching dependencies requires the bound local session")
	}
	sub, w, cid, err := s.boundWorkspace(ctx, in.WorkspaceID, r.SessionID)
	if err != nil {
		return nil, err
	}
	if sub.State != "open" {
		return nil, apierr.New("invalid_state", "only open workspaces can be changed")
	}
	if err := s.authority(ctx, r.SessionID, cid); err != nil {
		return nil, err
	}
	rel := ""
	if in.Path != nil {
		rel = *in.Path
	}
	if err := native.Workspaces.Detach(w.ID, rel); err != nil {
		return nil, err
	}
	return runtimeResult(map[string]any{"unsigned": true, "workspace_id": w.ID, "path": in.Path})
}
