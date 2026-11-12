package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func checkDef(command string) schema.CheckDefinition {
	return schema.CheckDefinition{Name: "unit", Category: "mechanical", Command: command, WorkingDirectory: ".", EnvAllowlist: []string{}, TimeoutSeconds: 20, OutputLimitBytes: 4096}
}

func (f *wtFixture) plan(d schema.CheckDefinition) Plan {
	return Plan{
		RunID: runID(), OperationID: schema.NewID(schema.PrefixOperation), VerifierSession: schema.NewID(schema.PrefixSession),
		Binding: f.b, SourceEnv: []string{},
		Check: CheckRef{DefinitionHash: strings.Repeat("b", 64), Name: d.Name, Category: d.Category, Criteria: []string{"tests", "lint"}, Definition: d},
	}
}

// run executes plan and fails the test on an unexpected error.
func (f *wtFixture) run(t *testing.T, p Plan) Outcome {
	t.Helper()
	out, err := Execute(context.Background(), GitCheckouts{Repo: f.repo}, f.runtimeDir, p, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

func (f *wtFixture) gone(t *testing.T, id string) {
	t.Helper()
	path := VerifyPath(f.runtimeDir, id)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || f.worktreeListed(t, path) {
		t.Fatalf("verification worktree %s remains", path)
	}
}

func reason(a schema.EvidenceAttestation) string {
	if a.Reason == nil {
		return ""
	}
	return *a.Reason
}

func exit(p *schema.Phase) int64 {
	if p == nil || p.ExitStatus == nil {
		return -999
	}
	return *p.ExitStatus
}

func TestAC167_168_058_AttestationFields(t *testing.T) {
	f := newWT(t)
	d := checkDef("echo checked")
	d.Category = "behavioral"
	p := f.plan(d)
	out := f.run(t, p)
	a := out.Attestation
	if a.Manifest != f.b.Manifest || a.Commit != f.b.Commit || a.ExecutedTree != f.b.Tree || a.CheckDefinition != p.Check.DefinitionHash {
		t.Fatalf("binding fields: %+v", a)
	}
	if a.Category != "behavioral" || a.Command != "echo checked" || len(a.Criteria) != 2 || a.Criteria[0] != "lint" || a.Criteria[1] != "tests" {
		t.Fatalf("definition fields: %+v", a)
	}
	if a.Result != "pass" || a.Reason != nil || a.Setup != nil || a.Check == nil || exit(a.Check) != 0 || a.Check.TimedOut {
		t.Fatalf("result fields: %+v", a)
	}
	if a.Isolation != "none" || a.EnvironmentHash == "" || a.OutputBytes != int64(len("checked\n")) || a.OutputTruncated {
		t.Fatalf("run fields: %+v", a)
	}
	sum := sha256.Sum256([]byte("checked\n"))
	if a.OutputDigest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %s", a.OutputDigest)
	}
	started, err1 := schema.ParseTimestamp(a.StartedAt)
	ended, err2 := schema.ParseTimestamp(a.EndedAt)
	if err1 != nil || err2 != nil || ended.Before(started) {
		t.Fatalf("timestamps %s %s", a.StartedAt, a.EndedAt)
	}
	// The attestation is a valid, signable record once a verifier is set.
	a.VerifierSession = p.VerifierSession
	key, err := identity.NewSession(p.VerifierSession)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := envelope.Seal(schema.KindEvidenceAttestation, a, key); err != nil {
		t.Fatalf("sealing: %v", err)
	}
	if raw, err := os.ReadFile(OutputPath(f.runtimeDir, p.RunID)); err != nil || string(raw) != "checked\n" {
		t.Fatalf("retained output %q %v", raw, err)
	}
	if string(out.Output) != "checked\n" {
		t.Fatalf("outcome output %q", out.Output)
	}
}

func TestAC094_SetupSeparateStatus(t *testing.T) {
	f := newWT(t)
	// Setup passes, check fails: both recorded, separately.
	d := checkDef("test -f marker && exit 3")
	setup := "echo prepared > marker"
	d.Setup = &setup
	a := f.run(t, f.plan(d)).Attestation
	if a.Result != "fail" || reason(a) != ReasonCheckFailed || exit(a.Setup) != 0 || exit(a.Check) != 3 {
		t.Fatalf("setup ok, check fail: %+v setup=%+v check=%+v", a, a.Setup, a.Check)
	}
	if a.Setup.DurationMS < 0 || a.Check.DurationMS < 0 {
		t.Fatal("durations")
	}
	// Setup ran in the same worktree: the check saw its output and passes.
	d = checkDef("test -f marker")
	d.Setup = &setup
	if a := f.run(t, f.plan(d)).Attestation; a.Result != "pass" || exit(a.Setup) != 0 || exit(a.Check) != 0 {
		t.Fatalf("setup should feed the check: %+v", a)
	}
	// Setup fails: a failed attestation, not a missing one; the check never runs.
	d = checkDef("echo check-ran")
	bad := "echo setup-output; exit 2"
	d.Setup = &bad
	out := f.run(t, f.plan(d))
	a = out.Attestation
	if a.Result != "fail" || reason(a) != ReasonSetupFailed || exit(a.Setup) != 2 || a.Check != nil {
		t.Fatalf("setup failure: %+v", a)
	}
	if strings.Contains(string(out.Output), "check-ran") || !strings.Contains(string(out.Output), "setup-output") {
		t.Fatalf("output %q", out.Output)
	}
}

func TestAC095_WorktreeRemoved(t *testing.T) {
	f := newWT(t)
	ctx := context.Background()
	for name, command := range map[string]string{"pass": "true", "fail": "false"} {
		p := f.plan(checkDef(command))
		out := f.run(t, p)
		if out.Kept != "" {
			t.Fatalf("%s: kept %s", name, out.Kept)
		}
		f.gone(t, p.RunID)
	}
	// Abort: a dirty checkout never runs anything and leaves nothing behind.
	p := f.plan(checkDef("echo should-not-run"))
	_, err := Execute(ctx, GitCheckouts{Repo: f.repo}, f.runtimeDir, p, &Hooks{AfterCheckout: func(path string) {
		_ = os.WriteFile(filepath.Join(path, "a.txt"), []byte("tampered\n"), 0o600)
	}})
	if !apierr.HasCode(err, "dirty_checkout") {
		t.Fatalf("abort: %v", err)
	}
	f.gone(t, p.RunID)
	// Keep policies.
	for _, tc := range []struct {
		keep    KeepPolicy
		command string
		kept    bool
	}{
		{KeepNever, "false", false}, {KeepOnFailure, "true", false}, {KeepOnFailure, "false", true},
		{KeepAlways, "true", true}, {KeepAlways, "false", true},
	} {
		p := f.plan(checkDef(tc.command))
		p.Keep = tc.keep
		out := f.run(t, p)
		if (out.Kept != "") != tc.kept {
			t.Fatalf("keep %s command %s: kept %q", tc.keep, tc.command, out.Kept)
		}
		if tc.kept {
			if out.Kept != VerifyPath(f.runtimeDir, p.RunID) || !f.worktreeListed(t, out.Kept) {
				t.Fatalf("kept worktree %s missing", out.Kept)
			}
			if err := removeVerifyWorktree(ctx, GitCheckouts{Repo: f.repo}, f.runtimeDir, p.RunID, out.Kept); err != nil {
				t.Fatal(err)
			}
		}
		f.gone(t, p.RunID)
	}
}

func TestAC093_EnvironmentReducedAndHashed(t *testing.T) {
	f := newWT(t)
	dump := filepath.Join(t.TempDir(), "env.txt")
	d := checkDef("env > " + dump)
	d.EnvAllowlist = []string{"KEEP", "PATH", "ABSENT"}
	source := []string{"SECRET=hunter2-value", "KEEP=visible-value", "PATH=" + os.Getenv("PATH")}
	run := func(src []string, setup string) schema.EvidenceAttestation {
		d := d
		if setup != "" {
			d.Setup = &setup
		}
		p := f.plan(d)
		p.SourceEnv = src
		return f.run(t, p).Attestation
	}
	a := run(source, "")
	seen, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	lines := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(seen)), "\n") {
		name, _, _ := strings.Cut(line, "=")
		lines[name] = true
	}
	if !strings.Contains(string(seen), "KEEP=visible-value") || strings.Contains(string(seen), "SECRET") || strings.Contains(string(seen), "hunter2") {
		t.Fatalf("child environment:\n%s", seen)
	}
	for name := range lines {
		// The shell and Go add PWD and friends; nothing else may appear.
		switch name {
		case "KEEP", "PATH", "PWD", "OLDPWD", "SHLVL", "_":
		default:
			t.Fatalf("unexpected variable %s in child environment:\n%s", name, seen)
		}
	}
	sum := sha256.Sum256([]byte("visible-value"))
	if got := a.Environment.Variables["KEEP"]; got != hex.EncodeToString(sum[:]) {
		t.Fatalf("variable digest %q", got)
	}
	if _, ok := a.Environment.Variables["ABSENT"]; ok || len(a.Environment.Variables) != 2 {
		t.Fatalf("variables %v", a.Environment.Variables)
	}
	raw, _ := json.Marshal(a)
	if strings.Contains(string(raw), "visible-value") || strings.Contains(string(raw), "hunter2") {
		t.Fatal("attestation contains a variable value")
	}
	if a.Environment.OS == "" || a.Environment.Arch == "" || !strings.HasPrefix(a.Environment.Toolchains["git"], "git version") {
		t.Fatalf("environment %+v", a.Environment)
	}
	// The hash is stable, and changes with a variable value or a lockfile.
	again := run(source, "")
	if again.EnvironmentHash != a.EnvironmentHash {
		t.Fatal("environment hash is not stable")
	}
	changed := run([]string{"KEEP=other-value", "PATH=" + os.Getenv("PATH")}, "")
	if changed.EnvironmentHash == a.EnvironmentHash {
		t.Fatal("environment hash ignored a variable change")
	}
	locked := run(source, "echo pinned > go.sum")
	if locked.EnvironmentHash == a.EnvironmentHash || locked.Environment.Lockfiles["go.sum"] == "" {
		t.Fatalf("environment hash ignored a lockfile: %+v", locked.Environment.Lockfiles)
	}
	if hash, err := EnvironmentHash(a.Environment); err != nil || hash != a.EnvironmentHash {
		t.Fatalf("EnvironmentHash %s %v", hash, err)
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitDead(t *testing.T, pid int) {
	t.Helper()
	for range 100 {
		if !alive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("child process %d survived the run", pid)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestEvidenceProcessFailure(t *testing.T) {
	f := newWT(t)
	ctx := context.Background()
	expect := func(t *testing.T, a schema.EvidenceAttestation, want string) {
		t.Helper()
		if a.Result != "fail" || reason(a) != want {
			t.Fatalf("want fail/%s, got %s/%q setup=%+v check=%+v", want, a.Result, reason(a), a.Setup, a.Check)
		}
	}

	t.Run("nonzero exit", func(t *testing.T) {
		a := f.run(t, f.plan(checkDef("exit 7"))).Attestation
		expect(t, a, ReasonCheckFailed)
		if exit(a.Check) != 7 {
			t.Fatalf("exit %d", exit(a.Check))
		}
	})

	t.Run("start failure", func(t *testing.T) {
		d := checkDef("true")
		d.WorkingDirectory = "missing"
		_, err := Execute(ctx, GitCheckouts{Repo: f.repo}, f.runtimeDir, f.plan(d), nil)
		if !apierr.HasCode(err, "invalid_working_directory") {
			t.Fatalf("missing working directory: %v", err)
		}
	})

	t.Run("check timeout kills descendants", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pid")
		d := checkDef("sleep 300 & echo $! > " + pidFile + "; sleep 300")
		d.TimeoutSeconds = 1
		start := time.Now()
		a := f.run(t, f.plan(d)).Attestation
		expect(t, a, ReasonCheckTimeout)
		if !a.Check.TimedOut || a.Check.ExitStatus != nil || time.Since(start) > 15*time.Second {
			t.Fatalf("check %+v after %s", a.Check, time.Since(start))
		}
		waitDead(t, readPID(t, pidFile))
	})

	t.Run("setup timeout", func(t *testing.T) {
		d := checkDef("echo never")
		slow := "sleep 300"
		d.Setup, d.TimeoutSeconds = &slow, 1
		a := f.run(t, f.plan(d)).Attestation
		expect(t, a, ReasonSetupTimeout)
		if !a.Setup.TimedOut || a.Check != nil {
			t.Fatalf("setup %+v check %+v", a.Setup, a.Check)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pid")
		cctx, cancel := context.WithCancel(ctx)
		go func() {
			for range 200 {
				if _, err := os.Stat(pidFile); err == nil {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			cancel()
		}()
		p := f.plan(checkDef("sleep 300 & echo $! > " + pidFile + "; sleep 300"))
		out, err := Execute(cctx, GitCheckouts{Repo: f.repo}, f.runtimeDir, p, nil)
		if err != nil {
			t.Fatal(err)
		}
		expect(t, out.Attestation, ReasonCancelled)
		if out.Attestation.Check == nil || out.Attestation.Check.TimedOut {
			t.Fatalf("check %+v", out.Attestation.Check)
		}
		waitDead(t, readPID(t, pidFile))
		f.gone(t, p.RunID)
	})

	t.Run("cancelled before start", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		out, err := Execute(cctx, GitCheckouts{Repo: f.repo}, f.runtimeDir, f.plan(checkDef("true")), nil)
		if err == nil {
			expect(t, out.Attestation, ReasonCancelled)
		}
	})

	t.Run("straggler after clean exit", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pid")
		a := f.run(t, f.plan(checkDef("sleep 300 >/dev/null 2>&1 & echo $! > "+pidFile))).Attestation
		if a.Result != "pass" {
			t.Fatalf("a clean exit passes: %+v", a)
		}
		waitDead(t, readPID(t, pidFile))
	})

	mutations := map[string]string{
		"modified": "echo more >> a.txt",
		"deleted":  "rm a.txt",
		"chmod":    "chmod +x a.txt",
		"replaced": "rm a.txt && ln -s run.sh a.txt",
	}
	for name, command := range mutations {
		t.Run("check mutates tracked file: "+name, func(t *testing.T) {
			// Exit 0, yet the run fails: the checkout no longer is the bound tree.
			a := f.run(t, f.plan(checkDef(command))).Attestation
			expect(t, a, ReasonTrackedMutation)
			if exit(a.Check) != 0 || a.ExecutedTree != f.b.Tree {
				t.Fatalf("check %+v tree %s", a.Check, a.ExecutedTree)
			}
		})
		t.Run("setup mutates tracked file: "+name, func(t *testing.T) {
			d := checkDef("echo check-ran")
			d.Setup = &command
			out := f.run(t, f.plan(d))
			expect(t, out.Attestation, ReasonTrackedMutation)
			if out.Attestation.Check != nil || strings.Contains(string(out.Output), "check-ran") {
				t.Fatal("the check ran on a mutated checkout")
			}
		})
	}

	t.Run("untracked and ignored files are allowed", func(t *testing.T) {
		a := f.run(t, f.plan(checkDef("echo build > output.bin; mkdir -p node_modules && echo x > node_modules/m"))).Attestation
		if a.Result != "pass" {
			t.Fatalf("generated files should not fail the run: %+v", a)
		}
	})

	t.Run("bounded output", func(t *testing.T) {
		d := checkDef("printf 0123456789abcdefghij; head -c 3000000 /dev/zero")
		d.OutputLimitBytes = 10
		p := f.plan(d)
		out := f.run(t, p)
		a := out.Attestation
		sum := sha256.Sum256([]byte("0123456789"))
		if a.Result != "pass" || a.OutputBytes != 20+3000000 || !a.OutputTruncated || a.OutputDigest != hex.EncodeToString(sum[:]) {
			t.Fatalf("bounded output: bytes=%d truncated=%v digest=%s", a.OutputBytes, a.OutputTruncated, a.OutputDigest)
		}
		if string(out.Output) != "0123456789" {
			t.Fatalf("retained %q", out.Output)
		}
		if raw, _ := os.ReadFile(OutputPath(f.runtimeDir, p.RunID)); string(raw) != "0123456789" {
			t.Fatalf("output file %q", raw)
		}
	})

	t.Run("zero output limit", func(t *testing.T) {
		d := checkDef("echo hidden")
		d.OutputLimitBytes = 0
		a := f.run(t, f.plan(d)).Attestation
		empty := sha256.Sum256(nil)
		if a.OutputBytes != 7 || !a.OutputTruncated || a.OutputDigest != hex.EncodeToString(empty[:]) {
			t.Fatalf("%+v", a)
		}
	})

	t.Run("judgment is not runnable", func(t *testing.T) {
		d := checkDef("true")
		d.Category = "judgment"
		if _, err := Execute(ctx, GitCheckouts{Repo: f.repo}, f.runtimeDir, f.plan(d), nil); !apierr.HasCode(err, "not_runnable") {
			t.Fatalf("judgment run: %v", err)
		}
	})
}

func TestAC096_237_IsolationNone(t *testing.T) {
	f := newWT(t)
	p := f.plan(checkDef("true"))
	a := f.run(t, p).Attestation
	if a.Isolation != "none" {
		t.Fatalf("isolation %q", a.Isolation)
	}
	a.VerifierSession = p.VerifierSession
	key, err := identity.NewSession(p.VerifierSession)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"sandbox", "container", "", "NONE"} {
		a.Isolation = value
		if _, err := envelope.Seal(schema.KindEvidenceAttestation, a, key); err == nil {
			t.Fatalf("schema accepted isolation %q", value)
		}
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "security-model.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	for _, want := range []string{"Evidence is not isolated", `isolation: "none"`, "per-run container or virtual machine", "no network by default", "read-only source", "separate verifier key custody"} {
		if !strings.Contains(text, want) {
			t.Fatalf("docs/security-model.md lacks %q", want)
		}
	}
}

// errCrash simulates the daemon dying at a boundary.
var errCrash = errors.New("simulated crash")

func TestEvidenceExecuteRecovery(t *testing.T) {
	// Interruption before and after every boundary Execute crosses. After a
	// restart (Reconcile), no verification worktree remains, the run is
	// discoverable as interrupted, and no outcome was produced.
	stages := []string{StageBeforeWorktreeAdd, StageAfterWorktreeAdd, StageReady, StagePrepared, StageAfterSetup, StageAfterCheck, StageExecuted}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			f := newWT(t)
			ctx := context.Background()
			author := filepath.Join(t.TempDir(), "author")
			git(t, f.repo.Dir, "worktree", "add", "--detach", "-q", author, f.base)
			d := checkDef("echo checked")
			setup := "echo setup"
			d.Setup = &setup
			p := f.plan(d)
			out, err := Execute(ctx, GitCheckouts{Repo: f.repo}, f.runtimeDir, p, &Hooks{Fault: func(s string) error {
				if s == stage {
					return errCrash
				}
				return nil
			}})
			if !errors.Is(err, errCrash) || out.Attestation.Manifest != "" {
				t.Fatalf("fault at %s: %v %+v", stage, err, out)
			}
			interrupted, err := Reconcile(ctx, GitCheckouts{Repo: f.repo}, f.runtimeDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(interrupted) != 1 || interrupted[0].RunID != p.RunID || interrupted[0].OperationID != p.OperationID || interrupted[0].VerifierSession != p.VerifierSession {
				t.Fatalf("interrupted %+v", interrupted)
			}
			f.gone(t, p.RunID)
			if !f.worktreeListed(t, author) {
				t.Fatal("reconcile removed an unrelated worktree")
			}
			j, err := ReadJournal(f.runtimeDir, p.RunID)
			if err != nil || j.State != StateInterrupted {
				t.Fatalf("journal %+v %v", j, err)
			}
			if again, err := Reconcile(ctx, GitCheckouts{Repo: f.repo}, f.runtimeDir); err != nil || len(again) != 0 {
				t.Fatalf("reconcile is not idempotent: %v %v", again, err)
			}
			// A fresh run after the restart succeeds.
			if a := f.run(t, f.plan(d)).Attestation; a.Result != "pass" {
				t.Fatalf("rerun: %+v", a)
			}
		})
	}
}
