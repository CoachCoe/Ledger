package evidence

import (
	"context"
	"encoding/json/v2"
	"slices"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// CheckRef is one pinned check definition and the criteria it serves.
type CheckRef struct {
	DefinitionHash string                 `json:"definition_hash"`
	Name           string                 `json:"name"`
	Category       string                 `json:"category"`
	Criteria       []string               `json:"criteria"`
	Definition     schema.CheckDefinition `json:"definition"`
}

// Binding is the exact revision evidence applies to: manifest, bound
// commit and the pinned definitions. Once returned it never changes; a
// reworked proposal is a different manifest and a different Binding.
type Binding struct {
	Manifest      string     `json:"manifest"`
	Commit        string     `json:"commit"`
	Tree          string     `json:"tree"`
	BaseCommit    string     `json:"base_commit"`
	ChangeID      string     `json:"change_id"`
	IntentID      string     `json:"intent_id"`
	WorkspaceID   string     `json:"workspace_id"`
	AuthorSession string     `json:"author_session"`
	Checks        []CheckRef `json:"checks"`
	Frozen        bool       `json:"frozen"`
}

// ResolveInput selects a binding by change id (convenience) or by exact
// manifest id with an optional commit that must agree.
type ResolveInput struct {
	ChangeID string
	Manifest string
	Commit   string
}

// Resolve returns the frozen binding. A change id resolves once to the
// workspace's current proposal; callers use the returned manifest, commit
// and definition hashes from then on.
func Resolve(ctx context.Context, idx *index.Index, in ResolveInput) (Binding, error) {
	manifest, commit := in.Manifest, in.Commit
	if in.ChangeID != "" {
		if manifest != "" || commit != "" {
			return Binding{}, apierr.New("invalid_binding", "change_id cannot be combined with manifest or commit")
		}
		wid, err := idx.ChangeWorkspace(ctx, in.ChangeID)
		if err != nil {
			return Binding{}, err
		}
		last, err := idx.LastProposal(ctx, wid)
		if err != nil {
			return Binding{}, err
		}
		if last.Manifest == "" || last.Commit == "" {
			return Binding{}, apierr.New("not_proposed", "change %s has no proposal", in.ChangeID)
		}
		manifest = last.Manifest
	}
	if manifest == "" {
		return Binding{}, apierr.New("invalid_binding", "a manifest or change id is required")
	}
	m, err := idx.Manifest(ctx, manifest)
	if err != nil {
		return Binding{}, err
	}
	bound, err := idx.ManifestCommit(ctx, manifest)
	if err != nil {
		return Binding{}, err
	}
	if bound == "" {
		return Binding{}, apierr.New("not_proposed", "manifest %s has no proposal commit", manifest)
	}
	if commit != "" && commit != bound {
		return Binding{}, apierr.New("binding_mismatch", "commit %s is not the commit bound to manifest %s", commit, manifest).With("bound_commit", bound)
	}
	checks, err := pinnedChecks(ctx, idx, m.IntentID)
	if err != nil {
		return Binding{}, err
	}
	return Binding{
		Manifest: manifest, Commit: bound, Tree: m.Tree, BaseCommit: m.BaseCommit,
		ChangeID: m.ChangeID, IntentID: m.IntentID, WorkspaceID: m.WorkspaceID, AuthorSession: m.AuthorSession,
		Checks: checks, Frozen: true,
	}, nil
}

// pinnedChecks groups the intent's check criteria by pinned definition hash.
func pinnedChecks(ctx context.Context, idx *index.Index, intentID string) ([]CheckRef, error) {
	kind, raw, err := idx.Creation(ctx, intentID)
	if err != nil {
		return nil, err
	}
	if kind != schema.KindIntent {
		return nil, apierr.New("unknown_entity", "no intent %s", intentID)
	}
	intent, err := decodeIntent(raw)
	if err != nil {
		return nil, err
	}
	checks := []CheckRef{}
	for _, c := range intent.Criteria {
		if c.Verification.Kind != "check" {
			continue
		}
		hash := c.Verification.DefinitionHash
		i := slices.IndexFunc(checks, func(r CheckRef) bool { return r.DefinitionHash == hash })
		if i < 0 {
			def, err := idx.CheckDefinition(ctx, hash)
			if err != nil {
				return nil, err
			}
			checks = append(checks, CheckRef{DefinitionHash: hash, Name: def.Name, Category: def.Category, Criteria: []string{}, Definition: def})
			i = len(checks) - 1
		}
		checks[i].Criteria = append(checks[i].Criteria, c.ID)
	}
	return checks, nil
}

func decodeIntent(raw []byte) (schema.Intent, error) {
	var intent schema.Intent
	err := json.Unmarshal(raw, &intent)
	return intent, err
}
