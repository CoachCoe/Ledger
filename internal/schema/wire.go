package schema

import "fmt"

// Wire returns payload in the form it is written at version: the payload
// itself at version 1 and for kinds version 2 did not change, and that
// kind's version 2 struct otherwise. A version 1 field that binds a Git
// object has no version 2 form, so a payload that sets one is refused.
func Wire(version int, kind Kind, payload any) (any, error) {
	if version != 2 || !changedInV2[kind] {
		return payload, nil
	}
	git := func(field, value string) error {
		if value != "" {
			return fmt.Errorf("schema: a version 2 %s has no %s", kind, field)
		}
		return nil
	}
	switch p := deref(payload).(type) {
	case Capability:
		return CapabilityV2{ID: p.ID, SessionID: p.SessionID, Issuer: p.Issuer, Allow: p.Allow, Deny: p.Deny,
			MayAddDependencies: p.MayAddDependencies, MayAdvance: nonNil(p.MayAdvance), ExpiresAt: p.ExpiresAt, CreatedAt: p.CreatedAt}, nil
	case Workspace:
		return WorkspaceV2{ID: p.ID, IntentID: p.IntentID, SessionID: p.SessionID, CapabilityID: p.CapabilityID,
			ChangeID: p.ChangeID, BaseTree: p.BaseTree, BaseLineAdvance: p.BaseLineAdvance, CreatedAt: p.CreatedAt}, git("base_commit", p.BaseCommit)
	case ChangeManifest:
		symbols := func(in []Symbol) []SymbolV2 {
			out := make([]SymbolV2, len(in))
			for i, s := range in {
				out[i] = SymbolV2(s)
			}
			return out
		}
		return ChangeManifestV2{ChangeID: p.ChangeID, IntentID: p.IntentID, WorkspaceID: p.WorkspaceID,
			AuthorSession: p.AuthorSession, ContextRefs: p.ContextRefs, Plan: p.Plan, BaseTree: p.BaseTree, Tree: p.Tree,
			Files: p.Files, SymbolsRead: symbols(p.SymbolsRead), SymbolsWritten: symbols(p.SymbolsWritten),
			OpHead: p.OpHead, SymbolMaps: p.SymbolMaps}, git("base_commit", p.BaseCommit)
	case EvidenceAttestation:
		if err := git("commit", p.Commit); err != nil {
			return nil, err
		}
		return EvidenceAttestationV2{Manifest: p.Manifest, Tree: p.Tree, CheckDefinition: p.CheckDefinition,
			Category: p.Category, Criteria: p.Criteria, Command: p.Command, Setup: p.Setup, Check: p.Check,
			Result: p.Result, Reason: p.Reason, EnvironmentHash: p.EnvironmentHash, Environment: p.Environment,
			VerifierSession: p.VerifierSession, StartedAt: p.StartedAt, EndedAt: p.EndedAt, OutputDigest: p.OutputDigest,
			OutputBytes: p.OutputBytes, OutputTruncated: p.OutputTruncated, Isolation: p.Isolation}, git("executed_tree", p.ExecutedTree)
	case SelectionDecision:
		return SelectionDecisionV2{IntentID: p.IntentID, Manifest: p.Manifest, Competitors: p.Competitors,
			Evidence: p.Evidence, Criteria: p.Criteria, ContraryEvidence: p.ContraryEvidence, Selector: p.Selector,
			Scoring: p.Scoring, At: p.At}, git("commit", p.Commit)
	case SpeculationRun:
		return SpeculationRunV2{ID: p.ID, IntentID: p.IntentID, Strategies: p.Strategies, Attempts: p.Attempts,
			Checks: p.Checks, Scorer: p.Scorer, ScorerDefinition: p.ScorerDefinition, BaseTree: p.BaseTree,
			Deadline: p.Deadline, CreatedAt: p.CreatedAt}, git("base_commit", p.BaseCommit)
	}
	return payload, nil
}

func deref(v any) any {
	switch p := v.(type) {
	case *Capability:
		return *p
	case *Workspace:
		return *p
	case *ChangeManifest:
		return *p
	case *EvidenceAttestation:
		return *p
	case *SelectionDecision:
		return *p
	case *SpeculationRun:
		return *p
	}
	return v
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
