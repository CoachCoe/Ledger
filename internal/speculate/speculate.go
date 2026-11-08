// Package speculate holds the pure rules of speculation runs: pinned run
// inputs, scorer results, the tiebreak order and attempt-record shape. It
// imports only schema-level packages so that publication, core and bundle
// verification share one definition (ADR 0023).
package speculate

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// TieRule is recorded with every speculation selection.
const TieRule = "highest score wins; equal scores resolve by ascending manifest hash"

// MaxAttempts bounds the attempts of one run.
const MaxAttempts = 64

// Attempt outcomes.
const (
	OutcomeFailed      = "failed_criteria"
	OutcomeTiebreaker  = "lost_tiebreaker"
	OutcomeTimeout     = "timeout"
	OutcomeCancelled   = "cancelled"
	OutcomeInterrupted = "interrupted"
	OutcomeNotProposed = "not_proposed"
)

// Scoring is the scoring input a speculation selection records: the pinned
// scorer, the score of every eligible candidate, and the tie rule.
type Scoring struct {
	Scorer   string           `json:"scorer"`
	Eligible []string         `json:"eligible"`
	Scores   map[string]int64 `json:"scores"`
	Rule     string           `json:"rule"`
}

// ScorerHash is the hash a run records for its scorer definition.
func ScorerHash(d schema.ScorerDefinition) (string, error) {
	raw, err := canon.Marshal(d)
	if err != nil {
		return "", err
	}
	return canon.Hash(raw), nil
}

// PinnedChecks returns the sorted distinct check-definition hashes pinned by
// the intent's check criteria.
func PinnedChecks(intent schema.Intent) []string {
	out := []string{}
	for _, c := range intent.Criteria {
		if c.Verification.Kind == "check" {
			out = append(out, c.Verification.DefinitionHash)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ValidateRun checks a run creation record against its intent: at least one
// check criterion, the exact pinned checks, the scorer hash, distinct
// strategies and attempts, and a deadline after creation.
func ValidateRun(r schema.SpeculationRun, intent schema.Intent) error {
	if r.IntentID != intent.ID {
		return apierr.New("invalid_run", "run names intent %s, not %s", r.IntentID, intent.ID)
	}
	checks := PinnedChecks(intent)
	if len(checks) == 0 {
		return apierr.New("no_check_criterion", "intent %s has no check-kind criterion; speculation needs something to score mechanically", intent.ID)
	}
	if !slices.Equal(checks, r.Checks) {
		return apierr.New("invalid_run", "run checks differ from the intent's pinned check definitions")
	}
	if h, err := ScorerHash(r.ScorerDefinition); err != nil {
		return err
	} else if h != r.Scorer {
		return apierr.New("invalid_run", "scorer %s is not the hash of the scorer definition", r.Scorer)
	}
	if err := ValidateScorer(r.ScorerDefinition); err != nil {
		return err
	}
	names := map[string]bool{}
	for _, s := range r.Strategies {
		if names[s.Name] {
			return apierr.New("invalid_run", "strategy %s appears twice", s.Name)
		}
		names[s.Name] = true
	}
	if len(r.Attempts) == 0 || len(r.Attempts) > MaxAttempts {
		return apierr.New("invalid_run", "a run has 1 to %d attempts", MaxAttempts)
	}
	seen := map[string]bool{}
	for _, a := range r.Attempts {
		if seen[a.WorkspaceID] {
			return apierr.New("invalid_run", "workspace %s is bound twice", a.WorkspaceID)
		}
		seen[a.WorkspaceID] = true
		if !names[a.Strategy] {
			return apierr.New("invalid_run", "attempt %s names unknown strategy %s", a.WorkspaceID, a.Strategy)
		}
	}
	created, err := schema.ParseTimestamp(r.CreatedAt)
	if err != nil {
		return err
	}
	deadline, err := schema.ParseTimestamp(r.Deadline)
	if err != nil {
		return err
	}
	if !deadline.After(created) {
		return apierr.New("invalid_run", "the deadline must follow creation")
	}
	return nil
}

// ValidateScorer checks the parts of a scorer definition the schema cannot.
func ValidateScorer(d schema.ScorerDefinition) error {
	switch d.Kind {
	case "command":
		if d.Command == nil || d.TimeoutSeconds == nil || *d.TimeoutSeconds < 1 {
			return apierr.New("invalid_scorer", "a command scorer needs a command and a timeout")
		}
	case "submission":
		if d.Command != nil || d.TimeoutSeconds != nil || len(d.EnvAllowlist) > 0 {
			return apierr.New("invalid_scorer", "a submission scorer has no command, timeout or environment")
		}
	default:
		return apierr.New("invalid_scorer", "unknown scorer kind %q", d.Kind)
	}
	return nil
}

// ParseScores validates a scorer result: one JSON object whose only member
// "scores" maps exactly the candidate ids to safe integers. Anything else is
// invalid_scores and must not produce a winner.
func ParseScores(raw []byte, ids []string) (map[string]int64, error) {
	if err := canon.Validate(raw); err != nil {
		return nil, apierr.New("invalid_scores", "scorer output is not valid JSON: %v", err)
	}
	var doc struct {
		Scores map[string]jsontext.Value `json:"scores"`
	}
	if err := json.Unmarshal(raw, &doc, json.RejectUnknownMembers(true)); err != nil {
		return nil, apierr.New("invalid_scores", "scorer output must be {\"scores\": {candidate: integer}}: %v", err)
	}
	if doc.Scores == nil {
		return nil, apierr.New("invalid_scores", "scorer output has no scores object")
	}
	out := map[string]int64{}
	for id, v := range doc.Scores {
		if !slices.Contains(ids, id) {
			return nil, apierr.New("invalid_scores", "score for %s, which is not an eligible candidate", id).With("candidate", id)
		}
		n, err := safeInteger(v)
		if err != nil {
			return nil, apierr.New("invalid_scores", "score for %s: %v", id, err).With("candidate", id)
		}
		out[id] = n
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, apierr.New("invalid_scores", "no score for candidate %s", id).With("candidate", id)
		}
	}
	return out, nil
}

func safeInteger(v jsontext.Value) (int64, error) {
	if v.Kind() != '0' {
		return 0, apierr.New("invalid_scores", "not a number")
	}
	for _, b := range v {
		if b == '.' || b == 'e' || b == 'E' {
			return 0, apierr.New("invalid_scores", "not an integer")
		}
	}
	var n int64
	if err := json.Unmarshal(v, &n); err != nil {
		return 0, err
	}
	const limit = 1<<53 - 1
	if n > limit || n < -limit {
		return 0, apierr.New("invalid_scores", "outside the safe integer range")
	}
	return n, nil
}

// Winner returns the highest-scoring candidate, ties broken by ascending
// manifest hash, or "" for no scores.
func Winner(scores map[string]int64) string {
	best := ""
	for id, n := range scores {
		if best == "" || n > scores[best] || (n == scores[best] && id < best) {
			best = id
		}
	}
	return best
}

// CheckScoring verifies a recorded scoring input against its run and winner.
func CheckScoring(raw jsontext.Value, scorer, winner string) (Scoring, error) {
	var s Scoring
	if err := json.Unmarshal(raw, &s, json.RejectUnknownMembers(true)); err != nil {
		return s, apierr.New("invalid_selection", "speculation scoring is malformed: %v", err)
	}
	if s.Scorer != scorer || s.Rule != TieRule {
		return s, apierr.New("invalid_selection", "speculation scoring does not name the run's scorer and tie rule")
	}
	if !slices.IsSorted(s.Eligible) || len(slices.Compact(slices.Clone(s.Eligible))) != len(s.Eligible) || len(s.Eligible) != len(s.Scores) {
		return s, apierr.New("invalid_selection", "eligible candidates must be sorted, unique and exactly the scored ones")
	}
	for _, id := range s.Eligible {
		if _, ok := s.Scores[id]; !ok {
			return s, apierr.New("invalid_selection", "eligible candidate %s has no score", id)
		}
	}
	if Winner(s.Scores) != winner {
		return s, apierr.New("invalid_selection", "the selected manifest is not the scoring winner")
	}
	return s, nil
}

// CheckAttempt verifies an attempt record's shape: manifests are the
// workspace's committed manifests (at most one for a run workspace),
// cancelled tells whether the run was cancelled, and evidence lists the
// attestations of that manifest.
func CheckAttempt(a schema.AttemptRecord, manifests []string, cancelled bool) error {
	bad := func(format string, args ...any) error { return apierr.New("invalid_attempt", format, args...) }
	switch {
	case len(manifests) > 1:
		return bad("workspace %s has more than one manifest", a.WorkspaceID)
	case a.Manifest == nil && len(manifests) > 0:
		return bad("attempt omits manifest %s", manifests[0])
	case a.Manifest != nil && (len(manifests) == 0 || manifests[0] != *a.Manifest):
		return bad("manifest %s is not the attempt's frozen candidate", *a.Manifest)
	}
	if cancelled != (a.Outcome == OutcomeCancelled) {
		return bad("outcome %s does not match how the run ended", a.Outcome)
	}
	switch a.Outcome {
	case OutcomeFailed, OutcomeTiebreaker:
		if a.Manifest == nil {
			return bad("outcome %s needs a submitted candidate", a.Outcome)
		}
	case OutcomeTimeout, OutcomeNotProposed:
		if a.Manifest != nil {
			return bad("outcome %s cannot have a submitted candidate", a.Outcome)
		}
	}
	if (a.Outcome == OutcomeFailed) != (len(a.FailedCriteria) > 0) {
		return bad("failed criteria are listed exactly for outcome %s", OutcomeFailed)
	}
	if (a.Outcome == OutcomeTiebreaker) != (a.Score != nil) {
		return bad("a score is recorded exactly for outcome %s", OutcomeTiebreaker)
	}
	if a.Manifest == nil && len(a.Evidence) > 0 {
		return bad("an attempt without a candidate has no evidence")
	}
	if !slices.IsSorted(a.Evidence) || !slices.IsSorted(a.FailedCriteria) {
		return bad("evidence and failed criteria must be sorted")
	}
	return nil
}

// DeadlinePassed reports whether at is at or after the run deadline.
func DeadlinePassed(r schema.SpeculationRun, at time.Time) bool {
	d, err := schema.ParseTimestamp(r.Deadline)
	return err == nil && !at.Before(d)
}
