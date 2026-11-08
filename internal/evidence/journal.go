package evidence

import (
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Run journal states. A run ends in published, interrupted or aborted.
const (
	StatePreparing   = "preparing"
	StateReady       = "ready"
	StateRunning     = "running"
	StatePublished   = "published"
	StateInterrupted = "interrupted"
	StateAborted     = "aborted"
)

var runIDPattern = regexp.MustCompile(`^run_[0-9a-v]{26}$`)

// Journal is the local, never-exported record of one evidence run. It lets
// startup find and clean up a verification worktree a crash left behind.
type Journal struct {
	RunID           string `json:"run_id"`
	OperationID     string `json:"operation_id,omitempty"`
	State           string `json:"state"`
	Manifest        string `json:"manifest"`
	Commit          string `json:"commit"`
	Tree            string `json:"tree"`
	CheckDefinition string `json:"check_definition,omitempty"`
	WorktreePath    string `json:"worktree_path"`
	VerifierSession string `json:"verifier_session,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// Terminal reports whether the run needs no further reconciliation.
func (j Journal) Terminal() bool {
	return j.State == StatePublished || j.State == StateInterrupted || j.State == StateAborted
}

func journalDir(runtimeDir string) string { return filepath.Join(runtimeDir, "evidence") }

func journalPath(runtimeDir, runID string) string {
	return filepath.Join(journalDir(runtimeDir), runID+".json")
}

// VerifyPath is the verification worktree location for a run.
func VerifyPath(runtimeDir, runID string) string {
	return filepath.Join(runtimeDir, "verify", runID)
}

// WriteJournal validates and atomically replaces the journal for j.RunID.
func WriteJournal(runtimeDir string, j Journal) error {
	if !runIDPattern.MatchString(j.RunID) {
		return apierr.New("invalid_run_id", "%q is not a run id", j.RunID)
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := schema.ValidateRuntime("evidence_run", raw); err != nil {
		return err
	}
	if err := os.MkdirAll(journalDir(runtimeDir), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(journalPath(runtimeDir, j.RunID), raw, 0o600)
}

// ReadJournal loads one run journal, failing with "unknown_run" if absent.
func ReadJournal(runtimeDir, runID string) (Journal, error) {
	if !runIDPattern.MatchString(runID) {
		return Journal{}, apierr.New("invalid_run_id", "%q is not a run id", runID)
	}
	raw, err := os.ReadFile(journalPath(runtimeDir, runID))
	if errors.Is(err, os.ErrNotExist) {
		return Journal{}, apierr.New("unknown_run", "no evidence run %s", runID)
	}
	if err != nil {
		return Journal{}, err
	}
	if err := schema.ValidateRuntime("evidence_run", raw); err != nil {
		return Journal{}, err
	}
	var j Journal
	if err := json.Unmarshal(raw, &j); err != nil {
		return Journal{}, err
	}
	return j, nil
}

// ListJournals returns every readable journal, ordered by run id.
func ListJournals(runtimeDir string) ([]Journal, error) {
	entries, err := os.ReadDir(journalDir(runtimeDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Journal
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !runIDPattern.MatchString(id) {
			continue
		}
		j, err := ReadJournal(runtimeDir, id)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	slices.SortFunc(out, func(a, b Journal) int { return strings.Compare(a.RunID, b.RunID) })
	return out, nil
}
