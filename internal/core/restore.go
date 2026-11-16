package core

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/oplog"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/workspace"
)

// restore writes the tree of an earlier operation log entry back into an
// open workspace as a new restore entry, so nothing in the log is lost
// (ADR 0026). Ledger writes these files on the session's behalf, so every
// path the restore changes must be within the workspace's capability, as
// for workspace.write. Like a write it is unsigned runtime state, retried
// by operation id.
func (s *Service) restore(ctx context.Context, r api.Request) (any, error) {
	in, err := decode[struct {
		WorkspaceID string `json:"workspace_id"`
		Entry       string `json:"entry"`
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
		return nil, apierr.New("unauthorized", "restoring requires the bound local session")
	}
	sub, w, cid, err := s.boundWorkspace(ctx, in.WorkspaceID, r.SessionID)
	if err != nil {
		return nil, err
	}
	if sub.State != "open" {
		return nil, apierr.New("invalid_state", "only open workspaces can be restored; rework a proposal first")
	}
	if err := s.authority(ctx, r.SessionID, cid); err != nil {
		return nil, err
	}
	m, _ := s.observing.LoadOrStore(w.ID, new(sync.Mutex))
	observation := m.(*sync.Mutex)
	observation.Lock()
	defer observation.Unlock()
	ws := s.Provider.Workspaces
	entries, err := ws.Log(w.ID)
	if err != nil {
		return nil, err
	}
	hashes, err := oplog.Chain(entries)
	if err != nil {
		return nil, err
	}
	i := slices.Index(hashes, in.Entry)
	if i < 0 {
		return nil, apierr.New("operation_unknown", "workspace %s has no entry %s", w.ID, in.Entry)
	}
	target := entries[i]
	if target.Base != w.BaseTree {
		return nil, apierr.New("stale_base", "entry %s is on base %s, not the workspace's base %s", in.Entry, target.Base, w.BaseTree)
	}
	session := optional(r.SessionID)
	head, _, _, err := ws.Snapshot(w.ID, "restore", session)
	if err != nil {
		return nil, err
	}
	changes, err := s.Objects.Diff(head.Tree, target.Tree)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(changes))
	deps := false
	for _, c := range changes {
		paths = append(paths, c.Path)
		deps = deps || capability.Dependency(c.Path)
	}
	at := time.Now()
	chain, err := s.chain(ctx, cid, at)
	if err != nil {
		return nil, err
	}
	bad, err := workspace.Violations(ctx, s.Provider.Trees(), head.Tree, workspace.Snapshot{Tree: target.Tree, Files: paths}, chain, s.caseSensitive, at)
	if err != nil {
		return nil, err
	}
	if len(bad) > 0 {
		denied := slices.Sorted(maps.Keys(bad))
		return nil, apierr.New("scope_violation", "restoring %s would change paths outside the workspace's capability", in.Entry).With("paths", denied)
	}
	if err := journal.begin(); err != nil {
		return nil, err
	}
	if deps {
		if err := ws.Detach(w.ID, ""); err != nil {
			return nil, err
		}
	}
	e, h, err := ws.Restore(w.ID, in.Entry, session)
	if err != nil {
		return nil, err
	}
	result, err := runtimeResult(map[string]any{"unsigned": true, "workspace_id": w.ID, "entry": h, "seq": e.Seq, "source": in.Entry, "tree": e.Tree, "files": paths})
	if err != nil {
		return nil, err
	}
	if err := journal.finish(result.(map[string]any)); err != nil {
		return nil, err
	}
	return journal.Result, nil
}

// logLimit bounds one workspace.log reply; symbol deltas are computed for
// every entry returned.
const logLimit = 500

// workspaceLog returns the latest entries of a workspace's operation log,
// each with the files it changed from the entry before it (the base, for
// the first) and the symbols those changes wrote. Symbol deltas are
// advisory, like all symbol analysis; the entries and trees are exact.
func (s *Service) workspaceLog(ctx context.Context, r api.Request) (any, error) {
	in, err := decode[struct {
		WorkspaceID string `json:"workspace_id"`
		Limit       int    `json:"limit"`
	}](r.Input)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := s.boundWorkspace(ctx, in.WorkspaceID, ""); err != nil {
		return nil, err
	}
	entries, err := s.Provider.Workspaces.Log(in.WorkspaceID)
	if err != nil {
		return nil, err
	}
	hashes, err := oplog.Chain(entries)
	if err != nil {
		return nil, err
	}
	from := max(0, len(entries)-min(cmp.Or(in.Limit, 50), logLimit))
	out := make([]map[string]any, 0, len(entries)-from)
	for i := from; i < len(entries); i++ {
		e := entries[i]
		prev := e.Base
		if i > 0 {
			prev = entries[i-1].Tree
		}
		changes, err := s.Objects.Diff(prev, e.Tree)
		if err != nil {
			return nil, err
		}
		files, paths := make([]map[string]any, 0, len(changes)), make([]string, 0, len(changes))
		for _, c := range changes {
			files = append(files, map[string]any{"path": c.Path, "status": c.Status})
			paths = append(paths, c.Path)
		}
		written := []schema.SymbolV2{}
		if len(paths) > 0 {
			a, err := s.symbolEngine.Compare(ctx, s.symbolSource(), prev, e.Tree, paths, nil)
			if err != nil {
				return nil, err
			}
			for _, sym := range a.Written {
				written = append(written, schema.SymbolV2(sym))
			}
		}
		out = append(out, map[string]any{
			"hash": hashes[i], "seq": e.Seq, "previous": e.Previous, "kind": e.Kind, "tree": e.Tree, "base": e.Base,
			"source": e.Source, "at": e.At, "trigger": e.Trigger, "session": e.Session,
			"files": files, "symbols_written": written,
		})
	}
	return runtimeResult(map[string]any{"unsigned": true, "advisory": true, "workspace_id": in.WorkspaceID, "total": len(entries), "entries": out})
}
