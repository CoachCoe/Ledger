package schema

import "encoding/json/jsontext"

// Payloads for schema_version 2 (ADR 0027). Kinds not listed here are
// unchanged from version 1. Fields that bind code name native store
// objects instead of Git objects.

type CapabilityV2 struct {
	ID                 string   `json:"id"`
	SessionID          string   `json:"session_id"`
	Issuer             Issuer   `json:"issuer"`
	Allow              []string `json:"allow"`
	Deny               []string `json:"deny"`
	MayAddDependencies bool     `json:"may_add_dependencies"`
	MayAdvance         []string `json:"may_advance"`
	ExpiresAt          string   `json:"expires_at"`
	CreatedAt          string   `json:"created_at"`
}

type WorkspaceV2 struct {
	ID              string  `json:"id"`
	IntentID        string  `json:"intent_id"`
	SessionID       string  `json:"session_id"`
	CapabilityID    string  `json:"capability_id"`
	ChangeID        string  `json:"change_id"`
	BaseTree        string  `json:"base_tree"`
	BaseLineAdvance *string `json:"base_line_advance"`
	CreatedAt       string  `json:"created_at"`
}

// SymbolV2 is a symbol at one revision: the hash of its declaration span,
// so a manifest says which version of the symbol it read or wrote.
type SymbolV2 struct {
	Path          string  `json:"path"`
	Language      string  `json:"language"`
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	Disambiguator *string `json:"disambiguator"`
	Revision      string  `json:"revision"`
}

type ChangeManifestV2 struct {
	ChangeID       string     `json:"change_id"`
	IntentID       string     `json:"intent_id"`
	WorkspaceID    string     `json:"workspace_id"`
	AuthorSession  string     `json:"author_session"`
	ContextRefs    []string   `json:"context_refs"`
	Plan           *string    `json:"plan"`
	BaseTree       string     `json:"base_tree"`
	Tree           string     `json:"tree"`
	Files          []string   `json:"files"`
	SymbolsRead    []SymbolV2 `json:"symbols_read"`
	SymbolsWritten []SymbolV2 `json:"symbols_written"`
	OpHead         string     `json:"op_head"`
	// SymbolMaps maps each analyzed path in Tree to its symbol_map object.
	SymbolMaps map[string]string `json:"symbol_maps"`
}

type EvidenceAttestationV2 struct {
	Manifest string `json:"manifest"`
	// Tree is the tree the check ran on; it must equal the manifest's.
	Tree            string      `json:"tree"`
	CheckDefinition string      `json:"check_definition"`
	Category        string      `json:"category"`
	Criteria        []string    `json:"criteria"`
	Command         string      `json:"command"`
	Setup           *Phase      `json:"setup"`
	Check           *Phase      `json:"check"`
	Result          string      `json:"result"`
	Reason          *string     `json:"reason"`
	EnvironmentHash string      `json:"environment_hash"`
	Environment     Environment `json:"environment"`
	VerifierSession string      `json:"verifier_session"`
	StartedAt       string      `json:"started_at"`
	EndedAt         string      `json:"ended_at"`
	OutputDigest    string      `json:"output_digest"`
	OutputBytes     int64       `json:"output_bytes"`
	OutputTruncated bool        `json:"output_truncated"`
	Isolation       string      `json:"isolation"`
}

type SelectionDecisionV2 struct {
	IntentID         string            `json:"intent_id"`
	Manifest         string            `json:"manifest"`
	Competitors      []string          `json:"competitors"`
	Evidence         []string          `json:"evidence"`
	Criteria         []CriterionStatus `json:"criteria"`
	ContraryEvidence []string          `json:"contrary_evidence"`
	Selector         Selector          `json:"selector"`
	Scoring          jsontext.Value    `json:"scoring"`
	At               string            `json:"at"`
}

type SpeculationRunV2 struct {
	ID               string             `json:"id"`
	IntentID         string             `json:"intent_id"`
	Strategies       []Strategy         `json:"strategies"`
	Attempts         []CandidateBinding `json:"attempts"`
	Checks           []string           `json:"checks"`
	Scorer           string             `json:"scorer"`
	ScorerDefinition ScorerDefinition   `json:"scorer_definition"`
	BaseTree         string             `json:"base_tree"`
	Deadline         string             `json:"deadline"`
	CreatedAt        string             `json:"created_at"`
}

// Repository is the root-signed creation record of a native repository.
type Repository struct {
	ID            string `json:"id"`
	HashAlgorithm string `json:"hash_algorithm"`
	// CasePolicy is "sensitive" or "insensitive"; path collisions are judged
	// under it for the repository's lifetime.
	CasePolicy string   `json:"case_policy"`
	Analyzers  []string `json:"analyzers"`
	// Tree is the content the repository was created with: the head of
	// every line until its first advance.
	Tree      string `json:"tree"`
	CreatedAt string `json:"created_at"`
}

// LineAdvance accepts a selected manifest onto a line. Previous is the
// line's head advance, or nil for the first.
type LineAdvance struct {
	Line      string  `json:"line"`
	Previous  *string `json:"previous"`
	Manifest  string  `json:"manifest"`
	Selection string  `json:"selection"`
	Actor     string  `json:"actor"`
	At        string  `json:"at"`
}
