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
)

// editJournal is the retry identity of one runtime edit (workspace.write or
// workspace.restore) across an interrupted application. It holds only the
// request digest and the unsigned result, never keys or source text, and is
// deliberately outside portable record history.
type editJournal struct {
	file    string
	Digest  string         `json:"digest"`
	Applied bool           `json:"applied"`
	Result  map[string]any `json:"result"`
}

// openEdit loads the journal of r's operation id. A completed edit is
// returned with Applied set; reusing the id for other input is refused.
// Callers hold mu.
func (s *Service) openEdit(r api.Request) (*editJournal, error) {
	digest, err := r.Digest()
	if err != nil {
		return nil, err
	}
	j := &editJournal{file: filepath.Join(s.runtimeDir(), "edits", r.OperationID+".json")}
	raw, err := os.ReadFile(j.file)
	if os.IsNotExist(err) {
		j.Digest = digest
		return j, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, j); err != nil {
		return nil, err
	}
	if j.Digest != digest {
		return nil, apierr.New("operation_conflict", "edit operation id has different input")
	}
	return j, nil
}

// begin records that the edit may have started.
func (j *editJournal) begin() error {
	if err := atomicfile.MakeDir(filepath.Dir(j.file), 0700); err != nil {
		return err
	}
	return j.save()
}

// finish records the edit's result for retries.
func (j *editJournal) finish(result map[string]any) error {
	j.Result, j.Applied = result, true
	return j.save()
}

func (j *editJournal) save() error {
	b, err := canon.Marshal(j)
	if err != nil {
		return err
	}
	return atomicfile.Write(j.file, b, 0600)
}

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
	journal, err := s.openEdit(r)
	if err != nil {
		return nil, err
	}
	if journal.Applied {
		return journal.Result, nil
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
	writer := s.Provider
	if err := journal.begin(); err != nil {
		return nil, err
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
	if err := journal.finish(result.(map[string]any)); err != nil {
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
	if err := s.Provider.Workspaces.Detach(w.ID, rel); err != nil {
		return nil, err
	}
	return runtimeResult(map[string]any{"unsigned": true, "workspace_id": w.ID, "path": in.Path})
}
