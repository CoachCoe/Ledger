package core

import (
	"context"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/schema"
)

type queryTarget struct {
	Symbol *schema.Symbol `json:"symbol"`
	Path   string         `json:"path"`
}

func (s *Service) samePath(a, b string) bool {
	x, e := capability.Normalize(a, s.caseSensitive)
	y, f := capability.Normalize(b, s.caseSensitive)
	return e == nil && f == nil && x == y
}

func target(raw []byte) (queryTarget, error) {
	q, err := decode[queryTarget](raw)
	if err != nil {
		return q, err
	}
	p := q.Path
	if q.Symbol != nil {
		p = q.Symbol.Path
	}
	_, err = capability.Normalize(p, true)
	return q, err
}

func (s *Service) matchesSymbol(q queryTarget, v schema.Symbol) bool {
	if q.Symbol == nil {
		return s.samePath(q.Path, v.Path)
	}
	a := *q.Symbol
	return s.samePath(a.Path, v.Path) && a.Language == v.Language && a.Name == v.Name && a.Kind == v.Kind && ((a.Disambiguator == nil && v.Disambiguator == nil) || (a.Disambiguator != nil && v.Disambiguator != nil && *a.Disambiguator == *v.Disambiguator))
}

func (s *Service) matchesSymbols(q queryTarget, ss []schema.Symbol) bool {
	for _, v := range ss {
		if s.matchesSymbol(q, v) {
			return true
		}
	}
	return false
}
func (s *Service) matchesManifest(q queryTarget, m schema.ChangeManifest) bool {
	if q.Symbol != nil {
		return s.matchesSymbols(q, m.SymbolsWritten)
	}
	for _, p := range m.Files {
		if s.samePath(q.Path, p) {
			return true
		}
	}
	return false
}

func (s *Service) history(ctx context.Context, r api.Request) (any, error) {
	q, err := target(r.Input)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.Store.Index.History(ctx)
	if err != nil {
		return nil, err
	}
	manifests := map[string]schema.ChangeManifest{}
	selected := map[string][]index.HistoricalRecord{}
	intents := map[string][]index.HistoricalRecord{}
	for _, record := range records {
		switch record.Kind {
		case schema.KindChangeManifest:
			var m schema.ChangeManifest
			if err := record.Decode(&m); err != nil {
				return nil, err
			}
			manifests[record.Hash] = m
		case schema.KindSelectionDecision:
			var d schema.SelectionDecision
			if err := record.Decode(&d); err != nil {
				return nil, err
			}
			selected[d.Manifest] = append(selected[d.Manifest], record)
		case schema.KindIntent:
			var i schema.Intent
			if err := record.Decode(&i); err != nil {
				return nil, err
			}
			intents[i.ID] = append(intents[i.ID], record)
		}
	}
	out := []any{}
	for _, record := range records {
		switch r.Operation {
		case "history.why":
			m, ok := manifests[record.Hash]
			if ok && s.matchesManifest(q, m) {
				out = append(out, map[string]any{"manifest": record, "intents": intents[m.IntentID]})
			}
		case "history.attempts":
			if record.Kind != schema.KindAttemptRecord {
				continue
			}
			var a schema.AttemptRecord
			if err := record.Decode(&a); err != nil {
				return nil, err
			}
			match := s.matchesSymbols(q, a.TargetSymbols)
			if a.Manifest != nil {
				if m, ok := manifests[*a.Manifest]; ok && s.matchesManifest(q, m) {
					match = true
				}
			}
			if match {
				out = append(out, record)
			}
		case "history.dependents":
			m, ok := manifests[record.Hash]
			if ok && len(selected[record.Hash]) > 0 && s.matchesSymbols(q, m.SymbolsRead) {
				out = append(out, map[string]any{"manifest": record, "selections": selected[record.Hash], "symbols": m.SymbolsWritten, "confidence": "manifest_cooccurrence"})
			}
		}
	}
	// Coverage is deliberately narrower than independent bundle verification.
	// Unindexed or incomplete provenance must never look like a complete search.
	src, err := s.repositorySource()
	if err != nil {
		return nil, err
	}
	defer src.Close()
	subjects, err := s.Store.Index.Subjects(ctx)
	if err != nil {
		return nil, err
	}
	conflicts := []any{}
	for _, sub := range subjects {
		if sub.Conflict != "" {
			conflicts = append(conflicts, map[string]any{"id": sub.ID, "kind": sub.Kind, "reason": sub.Conflict, "creation": sub.Creation, "detail": sub.Detail})
		}
	}
	ops, err := s.Store.Index.ConflictingOperations(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"records": out, "coverage": map[string]any{"scope": "indexed_committed_records", "verification": "not_performed", "receipt_missing_members": src.Corpus.Missing, "pending_records": src.Corpus.Pending, "record_problems": src.Corpus.Problems}, "subject_conflicts": conflicts, "operation_conflicts": ops}, nil
}

func (s *Service) nearby(ctx context.Context, r api.Request) (any, error) {
	q, err := target(r.Input)
	if err != nil {
		return nil, err
	}
	if err := s.Observe(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.coordMu.Lock()
	defer s.coordMu.Unlock()
	if err := s.expireLeases(ctx); err != nil {
		return nil, err
	}
	views, err := s.activeCoordination(ctx)
	if err != nil {
		return nil, err
	}
	workspaces := []any{}
	for _, v := range views {
		match := s.matchesSymbols(q, v.Analysis.Written) || s.matchesSymbols(q, v.Analysis.Read)
		if q.Symbol == nil {
			status, _, err := s.Store.Index.Observation(ctx, v.Workspace.ID)
			if err != nil {
				return nil, err
			}
			for p := range status {
				if s.samePath(q.Path, p) {
					match = true
				}
			}
		}
		for _, f := range v.Analysis.Fallbacks {
			p := q.Path
			if q.Symbol != nil {
				p = q.Symbol.Path
			}
			if s.samePath(p, f.Path) {
				match = true
			}
		}
		if match {
			workspaces = append(workspaces, map[string]any{"workspace_id": v.Workspace.ID, "intent_id": v.Workspace.IntentID, "session_id": v.Subject.Owner, "state": v.Subject.State, "tree": v.Tree, "analysis": v.Analysis})
		}
	}
	leases, err := s.Store.Index.Leases(ctx)
	if err != nil {
		return nil, err
	}
	matching := []any{}
	for _, l := range leases {
		match := s.matchesSymbols(q, l.Symbols)
		p := q.Path
		if q.Symbol != nil {
			p = q.Symbol.Path
		}
		for _, lp := range l.Paths {
			if s.samePath(p, lp) {
				match = true
			}
		}
		if match {
			matching = append(matching, l)
		}
	}
	return runtimeResult(map[string]any{"unsigned": true, "workspaces": workspaces, "leases": matching})
}
