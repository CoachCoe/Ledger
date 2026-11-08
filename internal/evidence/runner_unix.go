//go:build unix

package evidence

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// RunPhase runs spec.Command under /bin/sh in its own process group with no
// stdin, copying stdout and stderr into out. The whole group is killed on
// timeout, on cancellation and once the shell exits, so a backgrounded
// child never outlives the phase.
func RunPhase(ctx context.Context, spec PhaseSpec, out *BoundedOutput) PhaseResult {
	started := time.Now()
	if ctx.Err() != nil {
		return PhaseResult{Cancelled: true}
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return PhaseResult{StartErr: err}
	}
	defer pr.Close()
	cmd := exec.Command("/bin/sh", "-c", spec.Command) //nolint:gosec // running the pinned check command is the purpose
	cmd.Dir = spec.Dir
	cmd.Env = append([]string{}, spec.Env...)
	cmd.Stdin = nil
	// *os.File outputs make exec start no copying goroutine, so Wait returns
	// when the shell exits even if a descendant still holds the pipe.
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		pw.Close()
		return PhaseResult{StartErr: err, Duration: time.Since(started)}
	}
	pw.Close()
	pgid := cmd.Process.Pid
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(out, pr)
	}()
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait() // the exit status is read from ProcessState below
		close(waited)
	}()
	timer := time.NewTimer(spec.Timeout)
	defer timer.Stop()

	var res PhaseResult
	select {
	case <-waited:
	case <-timer.C:
		res.TimedOut = true
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-waited
	case <-ctx.Done():
		res.Cancelled = true
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-waited
	}
	// Stragglers: anything left in the group dies with the phase.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case <-copied:
	case <-time.After(pipeDrain):
	}
	res.Duration = time.Since(started)
	if !res.TimedOut && !res.Cancelled && cmd.ProcessState != nil {
		if code := int64(cmd.ProcessState.ExitCode()); code >= 0 {
			res.ExitStatus = &code
		}
	}
	return res
}
