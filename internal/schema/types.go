package schema

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json/jsontext"
	"strings"
	"time"
)

// Kind names a record schema.
type Kind string

const (
	KindRootKey             Kind = "root_key"
	KindIntent              Kind = "intent"
	KindCheckDefinition     Kind = "check_definition"
	KindCapability          Kind = "capability"
	KindSession             Kind = "session"
	KindWorkspace           Kind = "workspace"
	KindLifecycleEvent      Kind = "lifecycle_event"
	KindWorkspaceBinding    Kind = "workspace_binding"
	KindChangeManifest      Kind = "change_manifest"
	KindEvidenceAttestation Kind = "evidence_attestation"
	KindSelectionDecision   Kind = "selection_decision"
	KindAttemptRecord       Kind = "attempt_record"
	KindSpeculationRun      Kind = "speculation_run"
	KindOperationReceipt    Kind = "operation_receipt"
	KindRepository          Kind = "repository"
	KindLineAdvance         Kind = "line_advance"
)

var kindsV1 = []Kind{
	KindRootKey, KindIntent, KindCheckDefinition, KindCapability, KindSession,
	KindWorkspace, KindLifecycleEvent, KindWorkspaceBinding, KindChangeManifest,
	KindEvidenceAttestation, KindSelectionDecision, KindAttemptRecord,
	KindSpeculationRun, KindOperationReceipt,
}

// KindsFor lists the record kinds of one schema_version, or nil for a
// version this build does not read.
func KindsFor(version int) []Kind {
	switch version {
	case 1:
		return kindsV1
	case 2:
		return append(kindsV1[:len(kindsV1):len(kindsV1)], KindRepository, KindLineAdvance)
	}
	return nil
}

// Envelope is the signed wrapper around every immutable record. Signature
// is empty while the signing input is computed.
type Envelope struct {
	SchemaVersion int            `json:"schema_version"`
	Kind          Kind           `json:"kind"`
	Payload       jsontext.Value `json:"payload"`
	Signer        string         `json:"signer"`
	Signature     string         `json:"signature,omitzero"`
}

// Entity id prefixes. Ids are the prefix plus 26 lowercase base32
// characters of 128 random bits.
const (
	PrefixIntent     = "int_"
	PrefixSession    = "ses_"
	PrefixCapability = "cap_"
	PrefixWorkspace  = "wsp_"
	PrefixChange     = "chg_"
	PrefixRun        = "run_"
	PrefixOperation  = "op_"
	PrefixRepository = "rep_"
)

var idEncoding = base32.NewEncoding("0123456789abcdefghijklmnopqrstuv").WithPadding(base32.NoPadding)

// NewID returns a fresh random entity id with the given prefix.
func NewID(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + idEncoding.EncodeToString(b[:])
}

// ShortID abbreviates an entity id to its prefix and first 8 random
// characters (40 bits) for display names such as proposal branches.
func ShortID(id string) string {
	_, random, ok := strings.Cut(id, "_")
	if !ok || len(random) <= 8 {
		return id
	}
	return id[:len(id)-len(random)+8]
}

// Timestamp formats t as a record timestamp: UTC, millisecond precision.
func Timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ParseTimestamp parses a record timestamp.
func ParseTimestamp(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

type RootKey struct {
	PublicKey string `json:"public_key"`
	CreatedAt string `json:"created_at"`
}

type Intent struct {
	ID          string      `json:"id"`
	ParentID    *string     `json:"parent_id"`
	Description string      `json:"description"`
	Criteria    []Criterion `json:"criteria"`
	Source      Source      `json:"source"`
	Owner       string      `json:"owner"`
	CreatedAt   string      `json:"created_at"`
}

type Criterion struct {
	ID           string       `json:"id"`
	Statement    string       `json:"statement"`
	Verification Verification `json:"verification"`
}

// Verification is either a check (Check and DefinitionHash set) or a
// judgment (Reviewer set), selected by Kind.
type Verification struct {
	Kind           string `json:"kind"`
	Check          string `json:"check,omitzero"`
	DefinitionHash string `json:"definition_hash,omitzero"`
	Reviewer       string `json:"reviewer,omitzero"`
}

type Source struct {
	Kind      string  `json:"kind"`
	Reference *string `json:"reference"`
}

type CheckDefinition struct {
	Name             string   `json:"name"`
	Category         string   `json:"category"`
	Setup            *string  `json:"setup"`
	Command          string   `json:"command"`
	WorkingDirectory string   `json:"working_directory"`
	EnvAllowlist     []string `json:"env_allowlist"`
	TimeoutSeconds   int64    `json:"timeout_seconds"`
	OutputLimitBytes int64    `json:"output_limit_bytes"`
}

type Capability struct {
	ID                 string   `json:"id"`
	SessionID          string   `json:"session_id"`
	Issuer             Issuer   `json:"issuer"`
	Allow              []string `json:"allow"`
	Deny               []string `json:"deny"`
	MayAddDependencies bool     `json:"may_add_dependencies"`
	ExpiresAt          string   `json:"expires_at"`
	CreatedAt          string   `json:"created_at"`
	// MayAdvance is set only by schema_version 2 capabilities.
	MayAdvance []string `json:"-"`
}

// Issuer is the root (Fingerprint set) or a parent capability
// (CapabilityID set), selected by Kind.
type Issuer struct {
	Kind         string `json:"kind"`
	Fingerprint  string `json:"fingerprint,omitzero"`
	CapabilityID string `json:"capability_id,omitzero"`
}

type Session struct {
	ID              string  `json:"id"`
	CapabilityID    string  `json:"capability_id"`
	PublicKey       string  `json:"public_key"`
	ParticipantKind string  `json:"participant_kind"`
	AgentName       *string `json:"agent_name"`
	Model           *string `json:"model"`
	StartedAt       string  `json:"started_at"`
}

type Workspace struct {
	ID           string `json:"id"`
	IntentID     string `json:"intent_id"`
	SessionID    string `json:"session_id"`
	CapabilityID string `json:"capability_id"`
	ChangeID     string `json:"change_id"`
	BaseCommit   string `json:"base_commit"`
	CreatedAt    string `json:"created_at"`
	// BaseTree and BaseLineAdvance replace BaseCommit in schema_version 2.
	BaseTree        string  `json:"-"`
	BaseLineAdvance *string `json:"-"`
}

type LifecycleEvent struct {
	SubjectKind   string  `json:"subject_kind"`
	SubjectID     string  `json:"subject_id"`
	Creation      string  `json:"creation"`
	Previous      *string `json:"previous"`
	FromState     string  `json:"from_state"`
	ToState       string  `json:"to_state"`
	Reason        *string `json:"reason"`
	ReplacementID *string `json:"replacement_id"`
	Actor         string  `json:"actor"`
	At            string  `json:"at"`
}

type WorkspaceBinding struct {
	WorkspaceID  string `json:"workspace_id"`
	Previous     string `json:"previous"`
	SessionID    string `json:"session_id"`
	CapabilityID string `json:"capability_id"`
	Actor        string `json:"actor"`
	Reason       string `json:"reason"`
	At           string `json:"at"`
}

type Symbol struct {
	Path          string  `json:"path"`
	Language      string  `json:"language"`
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	Disambiguator *string `json:"disambiguator"`
	// Revision, the declaration span hash, is recorded from schema_version 2.
	Revision string `json:"-"`
}

type ChangeManifest struct {
	ChangeID       string   `json:"change_id"`
	IntentID       string   `json:"intent_id"`
	WorkspaceID    string   `json:"workspace_id"`
	AuthorSession  string   `json:"author_session"`
	ContextRefs    []string `json:"context_refs"`
	Plan           *string  `json:"plan"`
	BaseCommit     string   `json:"base_commit"`
	Tree           string   `json:"tree"`
	Files          []string `json:"files"`
	SymbolsRead    []Symbol `json:"symbols_read"`
	SymbolsWritten []Symbol `json:"symbols_written"`
	// Schema_version 2 replaces BaseCommit with BaseTree, names a store
	// tree in Tree, and adds OpHead and SymbolMaps.
	BaseTree   string            `json:"-"`
	OpHead     string            `json:"-"`
	SymbolMaps map[string]string `json:"-"`
}

type Phase struct {
	ExitStatus *int64 `json:"exit_status"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
}

type Environment struct {
	OS         string            `json:"os"`
	Arch       string            `json:"arch"`
	Toolchains map[string]string `json:"toolchains"`
	Variables  map[string]string `json:"variables"`
	Lockfiles  map[string]string `json:"lockfiles"`
}

type EvidenceAttestation struct {
	Manifest        string      `json:"manifest"`
	Commit          string      `json:"commit"`
	ExecutedTree    string      `json:"executed_tree"`
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
	// Tree replaces Commit and ExecutedTree in schema_version 2.
	Tree string `json:"-"`
}

type CriterionStatus struct {
	CriterionID string   `json:"criterion_id"`
	Status      string   `json:"status"`
	Deciding    []string `json:"deciding"`
	Conflicting []string `json:"conflicting"`
}

type Selector struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type SelectionDecision struct {
	IntentID         string            `json:"intent_id"`
	Manifest         string            `json:"manifest"`
	Commit           string            `json:"commit"`
	Competitors      []string          `json:"competitors"`
	Evidence         []string          `json:"evidence"`
	Criteria         []CriterionStatus `json:"criteria"`
	ContraryEvidence []string          `json:"contrary_evidence"`
	Selector         Selector          `json:"selector"`
	Scoring          jsontext.Value    `json:"scoring"`
	At               string            `json:"at"`
}

type AttemptRecord struct {
	RunID          string   `json:"run_id"`
	IntentID       string   `json:"intent_id"`
	WorkspaceID    string   `json:"workspace_id"`
	Manifest       *string  `json:"manifest"`
	Approach       *string  `json:"approach"`
	Evidence       []string `json:"evidence"`
	Outcome        string   `json:"outcome"`
	FailedCriteria []string `json:"failed_criteria"`
	Score          *int64   `json:"score"`
	TargetSymbols  []Symbol `json:"target_symbols"`
}

type Strategy struct {
	Name string `json:"name"`
	Spec string `json:"spec"`
}

// CandidateBinding names one attempt workspace of a speculation run.
type CandidateBinding struct {
	WorkspaceID string `json:"workspace_id"`
	Strategy    string `json:"strategy"`
}

// ScorerDefinition is a run's pinned tiebreaker. A command scorer runs
// Command; a submission scorer waits for scores through the API.
type ScorerDefinition struct {
	Kind           string   `json:"kind"`
	Command        *string  `json:"command"`
	EnvAllowlist   []string `json:"env_allowlist"`
	TimeoutSeconds *int64   `json:"timeout_seconds"`
}

type SpeculationRun struct {
	ID               string             `json:"id"`
	IntentID         string             `json:"intent_id"`
	Strategies       []Strategy         `json:"strategies"`
	Attempts         []CandidateBinding `json:"attempts"`
	Checks           []string           `json:"checks"`
	Scorer           string             `json:"scorer"`
	ScorerDefinition ScorerDefinition   `json:"scorer_definition"`
	BaseCommit       string             `json:"base_commit"`
	Deadline         string             `json:"deadline"`
	CreatedAt        string             `json:"created_at"`
	// BaseTree replaces BaseCommit in schema_version 2.
	BaseTree string `json:"-"`
}

type OperationReceipt struct {
	OperationID   string         `json:"operation_id"`
	Operation     string         `json:"operation"`
	RequestDigest string         `json:"request_digest"`
	Result        jsontext.Value `json:"result"`
	Members       []string       `json:"members"`
	At            string         `json:"at"`
}

// PayloadType returns a pointer to a new zero payload for kind at
// version, or nil if this build does not read that pair.
func PayloadType(version int, kind Kind) any {
	if version == 2 {
		switch kind {
		case KindCapability:
			return new(CapabilityV2)
		case KindWorkspace:
			return new(WorkspaceV2)
		case KindChangeManifest:
			return new(ChangeManifestV2)
		case KindEvidenceAttestation:
			return new(EvidenceAttestationV2)
		case KindSelectionDecision:
			return new(SelectionDecisionV2)
		case KindSpeculationRun:
			return new(SpeculationRunV2)
		case KindRepository:
			return new(Repository)
		case KindLineAdvance:
			return new(LineAdvance)
		}
	} else if version != 1 {
		return nil
	}
	switch kind {
	case KindRootKey:
		return new(RootKey)
	case KindIntent:
		return new(Intent)
	case KindCheckDefinition:
		return new(CheckDefinition)
	case KindCapability:
		return new(Capability)
	case KindSession:
		return new(Session)
	case KindWorkspace:
		return new(Workspace)
	case KindLifecycleEvent:
		return new(LifecycleEvent)
	case KindWorkspaceBinding:
		return new(WorkspaceBinding)
	case KindChangeManifest:
		return new(ChangeManifest)
	case KindEvidenceAttestation:
		return new(EvidenceAttestation)
	case KindSelectionDecision:
		return new(SelectionDecision)
	case KindAttemptRecord:
		return new(AttemptRecord)
	case KindSpeculationRun:
		return new(SpeculationRun)
	case KindOperationReceipt:
		return new(OperationReceipt)
	}
	return nil
}
