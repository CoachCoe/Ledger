package evidence

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Failure reasons recorded in a failed attestation.
const (
	ReasonSetupFailed     = "setup_failed"
	ReasonSetupTimeout    = "setup_timeout"
	ReasonCheckFailed     = "check_failed"
	ReasonCheckTimeout    = "check_timeout"
	ReasonCancelled       = "cancelled"
	ReasonTrackedMutation = "tracked_mutation"
)

// KeepPolicy says whether the verification worktree outlives the run.
type KeepPolicy string

// Keep policies. The empty policy is KeepNever.
const (
	KeepNever     KeepPolicy = "never"
	KeepOnFailure KeepPolicy = "on_failure"
	KeepAlways    KeepPolicy = "always"
)

// Plan is everything Execute needs. The verifier session and operation id
// are only recorded in the run journal; signing never happens here.
type Plan struct {
	RunID           string
	OperationID     string
	VerifierSession string
	Binding         Binding
	Check           CheckRef
	Keep            KeepPolicy
	// SourceEnv is the environment allowlisted variables are taken from.
	SourceEnv []string
}

// Outcome is an executed run. Attestation lacks VerifierSession: the caller
// signs it under the verifier's key.
type Outcome struct {
	Attestation schema.EvidenceAttestation
	Output      []byte
	// Kept is the retained worktree path, or empty if it was removed.
	Kept string
}

// OutputPath is where a run's retained output is written (local only).
func OutputPath(runtimeDir, runID string) string {
	return filepath.Join(journalDir(runtimeDir), runID+".output")
}

// SetState rewrites a run journal's state and reason.
func SetState(runtimeDir, runID, state, reason string) error {
	j, err := ReadJournal(runtimeDir, runID)
	if err != nil {
		return err
	}
	j.State, j.Reason = state, reason
	return WriteJournal(runtimeDir, j)
}

// Execute runs the plan's check in a fresh verification worktree: setup,
// then check, each with the reduced environment and the definition's
// timeout, comparing tracked content with the manifest tree before anything
// runs and after each phase. Timeout, non-zero exit, cancellation and
// tracked mutation produce a failed outcome, not an error. A returned error
// means no outcome exists: Prepare refused the checkout, or Execute failed
// internally (journal aborted). A Hooks.Fault error is returned untouched,
// as a crash would leave things.
func Execute(ctx context.Context, checkouts Checkouts, runtimeDir string, p Plan, hooks *Hooks) (Outcome, error) {
	d := p.Check.Definition
	if err := Runnable(d); err != nil {
		return Outcome{}, apierr.New("not_runnable", "%v", err)
	}
	if err := ValidateDefinition(d); err != nil {
		return Outcome{}, apierr.New("invalid_checks", "%v", err)
	}
	template := Journal{RunID: p.RunID, OperationID: p.OperationID, CheckDefinition: p.Check.DefinitionHash, VerifierSession: p.VerifierSession}
	wt, err := Prepare(ctx, checkouts, runtimeDir, p.Binding, template, hooks)
	if err != nil {
		return Outcome{}, err
	}
	clean := context.WithoutCancel(ctx)
	fail := func(err error) (Outcome, error) {
		_ = wt.Remove(clean)
		_ = SetState(runtimeDir, p.RunID, StateAborted, apierr.As(err).Code)
		return Outcome{}, err
	}
	if err := SetState(runtimeDir, p.RunID, StateRunning, ""); err != nil {
		return fail(err)
	}
	if err := hooks.fault(StagePrepared); err != nil {
		return Outcome{}, err
	}
	root, err := filepath.EvalSymlinks(wt.Path)
	if err != nil {
		return fail(err)
	}
	dir, err := workDir(root, d.WorkingDirectory)
	if err != nil {
		return fail(err)
	}

	out := NewBoundedOutput(d.OutputLimitBytes)
	timeout := time.Duration(d.TimeoutSeconds) * time.Second
	child, _ := childEnvironment(d, p.SourceEnv)
	run := func(command string) PhaseResult {
		r := RunPhase(ctx, PhaseSpec{Command: command, Dir: dir, Env: child, Timeout: timeout}, out)
		if r.StartErr != nil {
			_, _ = out.Write([]byte("ledger: cannot start command: " + r.StartErr.Error() + "\n"))
		}
		return r
	}
	mutated := func() (bool, error) {
		tree, err := wt.TrackedTree(clean)
		return tree != p.Binding.Tree, err
	}

	started := time.Now()
	reason := ""
	var setup, check *schema.Phase
	if d.Setup != nil {
		r := run(*d.Setup)
		setup = phaseRecord(r)
		if hooks != nil && hooks.AfterSetup != nil {
			hooks.AfterSetup(wt.Path)
		}
		if err := hooks.fault(StageAfterSetup); err != nil {
			return Outcome{}, err
		}
		changed, err := mutated()
		if err != nil {
			return fail(err)
		}
		if reason = phaseReason(r, "setup"); reason == "" && changed {
			reason = ReasonTrackedMutation
		}
	}
	// The environment is described after setup, so dependencies and lockfiles
	// it produced are part of what the check ran against.
	env, _, err := BuildEnvironment(clean, root, dir, d, p.SourceEnv)
	if err != nil {
		return fail(err)
	}
	envHash, err := EnvironmentHash(env)
	if err != nil {
		return fail(err)
	}
	if reason == "" {
		r := run(d.Command)
		check = phaseRecord(r)
		if hooks != nil && hooks.AfterCheck != nil {
			hooks.AfterCheck(wt.Path)
		}
		if err := hooks.fault(StageAfterCheck); err != nil {
			return Outcome{}, err
		}
		changed, err := mutated()
		if err != nil {
			return fail(err)
		}
		if reason = phaseReason(r, "check"); reason == "" && changed {
			reason = ReasonTrackedMutation
		}
	}
	ended := time.Now()

	att := schema.EvidenceAttestation{
		Manifest:        p.Binding.Manifest,
		CheckDefinition: p.Check.DefinitionHash, Category: d.Category,
		Criteria: slices.Sorted(slices.Values(p.Check.Criteria)), Command: d.Command,
		Setup: setup, Check: check, Result: "pass",
		EnvironmentHash: envHash, Environment: env,
		StartedAt: schema.Timestamp(started), EndedAt: schema.Timestamp(ended),
		OutputDigest: out.Digest(), OutputBytes: out.Total(), OutputTruncated: out.Truncated(),
		Isolation: "none",
	}
	// A v2 manifest binds no commit; its attestation names only the tree.
	if p.Binding.Commit != "" {
		att.Commit, att.ExecutedTree = p.Binding.Commit, p.Binding.Tree
	} else {
		att.Tree = p.Binding.Tree
	}
	if reason != "" {
		att.Result, att.Reason = "fail", &reason
	}
	if err := os.MkdirAll(journalDir(runtimeDir), 0o700); err != nil {
		return fail(err)
	}
	if err := atomicfile.Write(OutputPath(runtimeDir, p.RunID), out.Bytes(), 0o600); err != nil {
		return fail(err)
	}

	res := Outcome{Attestation: att, Output: out.Bytes()}
	if p.Keep == KeepAlways || p.Keep == KeepOnFailure && reason != "" {
		res.Kept = wt.Path
	} else if err := wt.Remove(clean); err != nil {
		// The result stands; say honestly that the worktree is still there.
		res.Kept = wt.Path
	}
	if err := hooks.fault(StageExecuted); err != nil {
		return Outcome{}, err
	}
	return res, nil
}

func phaseRecord(r PhaseResult) *schema.Phase {
	return &schema.Phase{ExitStatus: r.ExitStatus, DurationMS: r.Duration.Milliseconds(), TimedOut: r.TimedOut}
}

// phaseReason names why a phase failed, or "" if it exited 0 in time.
func phaseReason(r PhaseResult, phase string) string {
	switch {
	case r.Cancelled:
		return ReasonCancelled
	case r.TimedOut:
		return phase + "_timeout"
	case !r.OK():
		return phase + "_failed"
	}
	return ""
}

// workDir resolves the check's working directory inside the checkout. A
// tracked symlink that leads outside the checkout is refused.
func workDir(root, rel string) (string, error) {
	dir := filepath.Join(root, filepath.FromSlash(rel))
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", apierr.New("invalid_working_directory", "working directory %q is not present in the checkout", rel)
	}
	if realDir != realRoot && !strings.HasPrefix(realDir, realRoot+string(filepath.Separator)) {
		return "", apierr.New("invalid_working_directory", "working directory %q leaves the checkout", rel)
	}
	if fi, err := os.Stat(realDir); err != nil || !fi.IsDir() {
		return "", apierr.New("invalid_working_directory", "working directory %q is not a directory", rel)
	}
	return realDir, nil
}
