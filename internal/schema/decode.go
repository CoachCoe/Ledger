package schema

import (
	"encoding/json/v2"
	"fmt"
	"reflect"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

// Decode strictly decodes a kind's payload written at version into v, a
// pointer to that kind's Go type. The Go types are the version 1 structs;
// a version 2 payload is decoded through its own struct and its fields are
// carried in the untagged members, so code that reads records handles both
// versions through one type.
func Decode(version int, kind Kind, raw []byte, v any) error {
	strict := json.RejectUnknownMembers(true)
	bad := func(err error) error {
		return apierr.New("record_malformed", "%s payload: %v", kind, err)
	}
	if version == 1 || version == 2 && !changedInV2[kind] {
		if version == 1 && (kind == KindRepository || kind == KindLineAdvance) {
			return bad(fmt.Errorf("kind %s needs schema_version 2", kind))
		}
		if err := json.Unmarshal(raw, v, strict); err != nil {
			return bad(err)
		}
		return nil
	}
	if version != 2 {
		return apierr.New("unsupported_schema_version", "schema_version %d is not supported", version)
	}
	wire := PayloadType(2, kind)
	if reflect.TypeOf(v) == reflect.TypeOf(wire) {
		if err := json.Unmarshal(raw, v, strict); err != nil {
			return bad(err)
		}
		return nil
	}
	if err := json.Unmarshal(raw, wire, strict); err != nil {
		return bad(err)
	}
	ok := false
	switch w := wire.(type) {
	case *CapabilityV2:
		var out *Capability
		if out, ok = v.(*Capability); ok {
			*out = Capability{ID: w.ID, SessionID: w.SessionID, Issuer: w.Issuer, Allow: w.Allow, Deny: w.Deny,
				MayAddDependencies: w.MayAddDependencies, ExpiresAt: w.ExpiresAt, CreatedAt: w.CreatedAt, MayAdvance: w.MayAdvance}
		}
	case *WorkspaceV2:
		var out *Workspace
		if out, ok = v.(*Workspace); ok {
			*out = Workspace{ID: w.ID, IntentID: w.IntentID, SessionID: w.SessionID, CapabilityID: w.CapabilityID,
				ChangeID: w.ChangeID, CreatedAt: w.CreatedAt, BaseTree: w.BaseTree, BaseLineAdvance: w.BaseLineAdvance}
		}
	case *ChangeManifestV2:
		var out *ChangeManifest
		if out, ok = v.(*ChangeManifest); ok {
			*out = ChangeManifest{ChangeID: w.ChangeID, IntentID: w.IntentID, WorkspaceID: w.WorkspaceID,
				AuthorSession: w.AuthorSession, ContextRefs: w.ContextRefs, Plan: w.Plan, Tree: w.Tree, Files: w.Files,
				SymbolsRead: symbolsFromV2(w.SymbolsRead), SymbolsWritten: symbolsFromV2(w.SymbolsWritten),
				BaseTree: w.BaseTree, OpHead: w.OpHead, SymbolMaps: w.SymbolMaps}
		}
	case *EvidenceAttestationV2:
		var out *EvidenceAttestation
		if out, ok = v.(*EvidenceAttestation); ok {
			*out = EvidenceAttestation{Manifest: w.Manifest, CheckDefinition: w.CheckDefinition, Category: w.Category,
				Criteria: w.Criteria, Command: w.Command, Setup: w.Setup, Check: w.Check, Result: w.Result, Reason: w.Reason,
				EnvironmentHash: w.EnvironmentHash, Environment: w.Environment, VerifierSession: w.VerifierSession,
				StartedAt: w.StartedAt, EndedAt: w.EndedAt, OutputDigest: w.OutputDigest, OutputBytes: w.OutputBytes,
				OutputTruncated: w.OutputTruncated, Isolation: w.Isolation, Tree: w.Tree}
		}
	case *SelectionDecisionV2:
		var out *SelectionDecision
		if out, ok = v.(*SelectionDecision); ok {
			*out = SelectionDecision{IntentID: w.IntentID, Manifest: w.Manifest, Competitors: w.Competitors,
				Evidence: w.Evidence, Criteria: w.Criteria, ContraryEvidence: w.ContraryEvidence, Selector: w.Selector,
				Scoring: w.Scoring, At: w.At}
		}
	case *SpeculationRunV2:
		var out *SpeculationRun
		if out, ok = v.(*SpeculationRun); ok {
			*out = SpeculationRun{ID: w.ID, IntentID: w.IntentID, Strategies: w.Strategies, Attempts: w.Attempts,
				Checks: w.Checks, Scorer: w.Scorer, ScorerDefinition: w.ScorerDefinition, Deadline: w.Deadline,
				CreatedAt: w.CreatedAt, BaseTree: w.BaseTree}
		}
	case *Repository:
		var out *Repository
		if out, ok = v.(*Repository); ok {
			*out = *w
		}
	case *LineAdvance:
		var out *LineAdvance
		if out, ok = v.(*LineAdvance); ok {
			*out = *w
		}
	}
	if !ok {
		return fmt.Errorf("schema: cannot decode a version %d %s into %T", version, kind, v)
	}
	return nil
}

var changedInV2 = map[Kind]bool{
	KindCapability: true, KindWorkspace: true, KindChangeManifest: true, KindEvidenceAttestation: true,
	KindSelectionDecision: true, KindSpeculationRun: true, KindRepository: true, KindLineAdvance: true,
}

func symbolsFromV2(in []SymbolV2) []Symbol {
	out := make([]Symbol, len(in))
	for i, s := range in {
		out[i] = Symbol(s)
	}
	return out
}

// ExecutedOn is the tree the check ran on: ExecutedTree in version 1, Tree
// in version 2.
func (a EvidenceAttestation) ExecutedOn() string {
	if a.Tree != "" {
		return a.Tree
	}
	return a.ExecutedTree
}
