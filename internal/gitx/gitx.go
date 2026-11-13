// Package gitx is a thin wrapper over the git command line (ADR 0004),
// which Ledger uses only to read Phase 1 repositories. It never changes
// repository or user Git configuration; anything it needs is passed per
// command.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

// Repo is a Git working tree.
type Repo struct {
	// Dir is the absolute top-level directory of the working tree.
	Dir string
	// CommonDir is the absolute shared .git directory, the same for every
	// linked worktree.
	CommonDir string
	// MainDir is the main working tree, which holds Ledger's state for
	// every linked worktree.
	MainDir string
}

// Error describes a failed git invocation.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.Args, " "), e.ExitCode, strings.TrimSpace(e.Stderr))
}

// Run executes git in dir with optional stdin and extra environment, and
// returns stdout.
func Run(ctx context.Context, dir string, stdin io.Reader, env []string, args ...string) ([]byte, error) {
	args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)
	cmd := exec.CommandContext(ctx, gitBinary(), args...) //nolint:gosec // running git with Ledger-built arguments is this package's purpose
	cmd.Dir = dir
	cmd.Stdin = stdin
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	cmd.Env = append(cmd.Env, "GIT_NO_REPLACE_OBJECTS=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, apierr.New("cancelled", "git operation cancelled: %v", ctx.Err())
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, &Error{Args: args[4:], ExitCode: ee.ExitCode(), Stderr: stderr.String()}
		}
		return nil, apierr.New("git_unavailable", "running git: %v", err)
	}
	return stdout.Bytes(), nil
}

// gitBinary returns the git executable that `git` itself runs. On macOS,
// /usr/bin/git is a shim that resolves the developer toolchain on every
// call, which more than doubles the cost of each invocation.
var gitBinary = sync.OnceValue(func() string {
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return "git"
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "git")
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
		return p
	}
	return "git"
})

// Open finds the working tree containing dir. It fails with code
// "not_a_git_repo" outside a working tree.
func Open(ctx context.Context, dir string) (*Repo, error) {
	out, err := Run(ctx, dir, nil, nil, "rev-parse", "--show-toplevel")
	top := strings.TrimSpace(string(out))
	if err != nil || top == "" {
		return nil, apierr.New("not_a_git_repo", "%s is not inside a Git working tree", dir)
	}
	top = filepath.Clean(top)
	// A relative --git-common-dir is relative to the working directory, so
	// ask from the top level.
	common, err := trim(Run(ctx, top, nil, nil, "rev-parse", "--git-common-dir"))
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(top, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		return nil, apierr.New("unsupported_repository", "%s has no main working tree (bare or separate git dir)", top)
	}
	return &Repo{Dir: top, CommonDir: common, MainDir: filepath.Dir(common)}, nil
}

func (r *Repo) git(ctx context.Context, args ...string) ([]byte, error) {
	return Run(ctx, r.Dir, nil, nil, args...)
}

func trim(b []byte, err error) (string, error) {
	return strings.TrimSpace(string(b)), err
}

// ResolveRef returns the object a ref points to, or "" if it does not exist.
func (r *Repo) ResolveRef(ctx context.Context, ref string) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TreeEntry is one blob in a tree listing.
type TreeEntry struct {
	Path string
	Blob string
	Mode string
}

// ListTree returns every blob reachable from the tree of commit.
func (r *Repo) ListTree(ctx context.Context, commit string) ([]TreeEntry, error) {
	out, err := r.git(ctx, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for rec := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || f[1] != "blob" {
			return nil, fmt.Errorf("gitx: unexpected ls-tree entry %q", rec)
		}
		entries = append(entries, TreeEntry{Path: path, Blob: f[2], Mode: f[0]})
	}
	return entries, nil
}

// Commit is the parsed content of a commit object.
type Commit struct {
	SHA     string
	Tree    string
	Parents []string
	Message string
}

var objectID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// ReadCommit parses a commit object. A missing object fails with
// "dependency_unavailable"; an object that is not a commit fails with
// "invalid_commit".
func (r *Repo) ReadCommit(ctx context.Context, sha string) (Commit, error) {
	if !objectID.MatchString(sha) {
		return Commit{}, apierr.New("invalid_commit", "%q is not a full object id", sha)
	}
	if _, err := r.git(ctx, "cat-file", "-e", sha+"^{commit}"); err != nil {
		var ge *Error
		if errors.As(err, &ge) {
			return Commit{}, apierr.New("dependency_unavailable", "commit %s is not available", sha)
		}
		return Commit{}, err
	}
	out, err := r.git(ctx, "cat-file", "commit", sha)
	if err != nil {
		return Commit{}, err
	}
	headers, message, _ := strings.Cut(string(out), "\n\n")
	c := Commit{SHA: sha, Message: message}
	for line := range strings.SplitSeq(headers, "\n") {
		// Continuation lines (for example a signature) begin with a space.
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "tree":
			c.Tree = value
		case "parent":
			c.Parents = append(c.Parents, value)
		}
	}
	if c.Tree == "" {
		return Commit{}, apierr.New("invalid_commit", "commit %s has no tree", sha)
	}
	return c, nil
}
