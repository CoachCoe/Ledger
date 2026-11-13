package core

import (
	"context"
	"encoding/json/jsontext"
	"slices"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/selection"
)

type selectionInput struct {
	bindingInput
	Evidence    []string       `json:"evidence"`
	Competitors []string       `json:"competitors"`
	Scoring     jsontext.Value `json:"scoring"`
}

// recordSelection signs a selection decision for the current proposal of a
// workspace. The caller supplies only the exact revision and the evidence
// hashes to rely on: criterion statuses, contrary evidence and outstanding
// criteria are recomputed here and never accepted from the caller. The
// decision records a choice. It is not acceptance: nothing is merged, no
// branch or ref moves, and the workspace is not transitioned (ADR 0020).
func (s *Service) recordSelection(ctx context.Context, c *invocation) (prepared, error) {
	in, err := decode[selectionInput](c.Input)
	if err != nil {
		return prepared{}, err
	}
	b, sb, err := s.criteriaBinding(ctx, in.Manifest)
	if err != nil {
		return prepared{}, err
	}
	if c.SessionID != "" && c.SessionID == b.AuthorSession {
		return prepared{}, apierr.New("unauthorized", "the author of a change cannot select it")
	}
	intent, err := s.subject(ctx, b.IntentID, "intent")
	if err != nil {
		return prepared{}, err
	}
	if intent.State != "open" {
		return prepared{}, apierr.New("invalid_state", "intent %s is %s", intent.ID, intent.State)
	}
	ws, err := s.subject(ctx, b.WorkspaceID, "workspace")
	if err != nil {
		return prepared{}, err
	}
	if err := s.notInRun(ctx, ws.ID); err != nil {
		return prepared{}, err
	}
	if ws.State != "proposed" {
		return prepared{}, apierr.New("invalid_state", "workspace %s is %s, not proposed", ws.ID, ws.State)
	}
	last, err := s.Store.Index.LastProposal(ctx, b.WorkspaceID)
	if err != nil {
		return prepared{}, err
	}
	if last.Manifest != b.Manifest {
		return prepared{}, apierr.New("stale_revision", "manifest %s is not the current proposal; select %s", b.Manifest, last.Manifest).With("current_manifest", last.Manifest)
	}
	for _, other := range in.Competitors {
		m, err := s.Store.Index.Manifest(ctx, other)
		if err != nil {
			return prepared{}, err
		}
		if other == b.Manifest || m.IntentID != b.IntentID {
			return prepared{}, apierr.New("invalid_competitor", "competitor %s is the selected manifest or belongs to another intent", other)
		}
	}
	set, err := s.explicitSet(ctx, sb, in.Evidence)
	if err != nil {
		return prepared{}, err
	}
	known, err := s.Store.Index.EvidenceFor(ctx, b.Manifest)
	if err != nil {
		return prepared{}, err
	}
	kind := "root"
	if c.SessionID != "" {
		kind = "session"
	}
	d, err := selection.Build(sb, b.IntentID, set, known, schema.Selector{Kind: kind, ID: c.signer.ID()}, in.Competitors, in.Scoring, now())
	if err != nil {
		return prepared{}, err
	}
	o, err := s.seal(schema.KindSelectionDecision, d, c.signer)
	if err != nil {
		return prepared{}, err
	}
	return prepared{records: []envelope.Object{o}, result: map[string]any{
		"selection": o.Hash, "intent_id": d.IntentID, "manifest": d.Manifest,
		"evidence": d.Evidence, "competitors": slices.Clone(d.Competitors), "criteria": d.Criteria,
		"outstanding": selection.OutstandingIDs(d.Criteria), "contrary_evidence": d.ContraryEvidence,
		"accepted": false, "selector": d.Selector,
	}}, nil
}
