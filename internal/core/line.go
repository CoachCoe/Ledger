package core

import (
	"cmp"
	"context"
	"slices"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/selection"
)

type lineInput struct {
	Line string `json:"line"`
}

// lineStatus returns where a line stands: its last advance, or null before
// the first, and the tree new workspaces fork from.
func (s *Service) lineStatus(ctx context.Context, r api.Request) (any, error) {
	in, err := decode[lineInput](r.Input)
	if err != nil {
		return nil, err
	}
	line := cmp.Or(in.Line, "main")
	head, err := s.Store.Index.LineHead(ctx, line)
	if err != nil {
		return nil, err
	}
	return map[string]any{"line": line, "advance": head.Advance, "tree": head.Tree}, nil
}

// advanceLine accepts a selected manifest onto a line (ADR 0027). The
// selection must still describe the workspace's current proposal, and every
// criterion must be satisfied by the selection's evidence when re-evaluated
// against all evidence known now. Contrary evidence the decision records was
// weighed by the selector; contrary evidence recorded after it blocks
// acceptance. The manifest's author cannot accept it, as it cannot select
// it. The validator rechecks may_advance, the line head and the base tree
// inside the RecordStore lock.
func (s *Service) advanceLine(ctx context.Context, c *invocation) (prepared, error) {
	in, err := decode[struct {
		Line      string `json:"line"`
		Selection string `json:"selection"`
	}](c.Input)
	if err != nil {
		return prepared{}, err
	}
	line := cmp.Or(in.Line, "main")
	d, err := s.Store.Index.Selection(ctx, in.Selection)
	if err != nil {
		return prepared{}, err
	}
	b, sb, err := s.criteriaBinding(ctx, d.Manifest)
	if err != nil {
		return prepared{}, err
	}
	if c.SessionID != "" && c.SessionID == b.AuthorSession {
		return prepared{}, apierr.New("unauthorized", "the author of a change cannot accept it")
	}
	if intent, err := s.subject(ctx, b.IntentID, "intent"); err != nil {
		return prepared{}, err
	} else if intent.State != "open" {
		return prepared{}, apierr.New("invalid_state", "intent %s is %s", intent.ID, intent.State)
	}
	ws, err := s.subject(ctx, b.WorkspaceID, "workspace")
	if err != nil {
		return prepared{}, err
	}
	if ws.State != "proposed" && ws.State != "selected" {
		return prepared{}, apierr.New("invalid_state", "workspace %s is %s, not proposed or selected", ws.ID, ws.State)
	}
	last, err := s.Store.Index.LastProposal(ctx, b.WorkspaceID)
	if err != nil {
		return prepared{}, err
	}
	if last.Manifest != d.Manifest {
		return prepared{}, apierr.New("stale_revision", "manifest %s is not the current proposal of %s", d.Manifest, ws.ID).With("current_manifest", last.Manifest)
	}
	set, err := s.explicitSet(ctx, sb, d.Evidence)
	if err != nil {
		return prepared{}, err
	}
	known, err := s.Store.Index.EvidenceFor(ctx, d.Manifest)
	if err != nil {
		return prepared{}, err
	}
	res := selection.Evaluate(sb, set, known)
	for _, cr := range res.Criteria {
		if cr.Status != selection.Satisfied {
			return prepared{}, apierr.New("criteria_unsatisfied", "criterion %s is %s for manifest %s", cr.CriterionID, cr.Status, d.Manifest).
				With("criteria", res.Criteria).With("contrary_evidence", res.Contrary)
		}
	}
	for _, h := range res.Contrary {
		if !slices.Contains(d.ContraryEvidence, h) {
			return prepared{}, apierr.New("criteria_unsatisfied", "evidence %s, recorded after selection %s, contradicts manifest %s", h, in.Selection, d.Manifest).
				With("criteria", res.Criteria).With("contrary_evidence", res.Contrary)
		}
	}
	head, err := s.Store.Index.LineHead(ctx, line)
	if err != nil {
		return prepared{}, err
	}
	a := schema.LineAdvance{Line: line, Previous: head.Advance, Manifest: d.Manifest, Selection: in.Selection, Actor: c.signer.ID(), At: now()}
	adv, err := s.seal(schema.KindLineAdvance, a, c.signer)
	if err != nil {
		return prepared{}, err
	}
	return prepared{records: []envelope.Object{adv}, result: map[string]any{
		"advance": adv.Hash, "line": line, "previous": head.Advance, "manifest": d.Manifest,
		"selection": in.Selection, "tree": b.Tree, "intent_id": b.IntentID, "workspace_id": b.WorkspaceID,
	}}, nil
}
