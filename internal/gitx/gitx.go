// Package gitx is a thin wrapper over the git command line (ADR 0004).
// Ledger never changes repository or user Git configuration; anything it
// needs is passed per command.
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
	"strconv"
	"strings"
	"sync"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

// MinVersion is the oldest Git release Ledger supports.
var MinVersion = [3]int{2, 25, 0}

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

// Commit identity for Ledger-authored commits, so commit creation never
// depends on the user's configuration.
var ledgerIdentity = []string{
	"GIT_AUTHOR_NAME=Ledger", "GIT_AUTHOR_EMAIL=ledger@localhost",
	"GIT_COMMITTER_NAME=Ledger", "GIT_COMMITTER_EMAIL=ledger@localhost",
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

// Version returns the installed Git version.
func Version(ctx context.Context) ([3]int, error) {
	out, err := Run(ctx, "", nil, nil, "version")
	if err != nil {
		return [3]int{}, err
	}
	return parseVersion(string(out))
}

var versionRE = regexp.MustCompile(`git version (\d+)\.(\d+)(?:\.(\d+))?`)

func parseVersion(s string) ([3]int, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, apierr.New("git_unavailable", "unrecognized git version %q", strings.TrimSpace(s))
	}
	var v [3]int
	for i := range 3 {
		if m[i+1] != "" {
			v[i], _ = strconv.Atoi(m[i+1])
		}
	}
	return v, nil
}

// CheckVersion fails with code "git_too_old" if Git is older than MinVersion.
func CheckVersion(ctx context.Context) error {
	v, err := Version(ctx)
	if err != nil {
		return err
	}
	for i := range 3 {
		if v[i] != MinVersion[i] {
			if v[i] < MinVersion[i] {
				return apierr.New("git_too_old", "git %d.%d.%d is older than the required %d.%d.%d",
					v[0], v[1], v[2], MinVersion[0], MinVersion[1], MinVersion[2])
			}
			break
		}
	}
	return nil
}

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

func (r *Repo) git(ctx context.Context, stdin io.Reader, env []string, args ...string) ([]byte, error) {
	return Run(ctx, r.Dir, stdin, env, args...)
}

func trim(b []byte, err error) (string, error) {
	return strings.TrimSpace(string(b)), err
}

// ResolveRef returns the object a ref points to, or "" if it does not exist.
func (r *Repo) ResolveRef(ctx context.Context, ref string) (string, error) {
	out, err := r.git(ctx, nil, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// WriteBlobs stores files as blobs and returns their object ids in order.
func (r *Repo) WriteBlobs(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	quoted := make([]string, len(paths))
	escape := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\r", "\\r", "\t", "\\t")
	for i, p := range paths {
		quoted[i] = "\"" + escape.Replace(p) + "\""
	}
	out, err := r.git(ctx, strings.NewReader(strings.Join(quoted, "\n")+"\n"), nil, "hash-object", "-w", "--no-filters", "--stdin-paths")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	if len(ids) != len(paths) {
		return nil, fmt.Errorf("gitx: hash-object returned %d ids for %d paths", len(ids), len(paths))
	}
	return ids, nil
}

// TreeEntry is one blob in a tree listing.
type TreeEntry struct {
	Path string
	Blob string
}

// ListTree returns every blob reachable from the tree of commit.
func (r *Repo) ListTree(ctx context.Context, commit string) ([]TreeEntry, error) {
	out, err := r.git(ctx, nil, nil, "ls-tree", "-r", "-z", "--full-tree", commit)
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
		entries = append(entries, TreeEntry{Path: path, Blob: f[2]})
	}
	return entries, nil
}

// ReadBlob returns the content of a blob.
func (r *Repo) ReadBlob(ctx context.Context, blob string) ([]byte, error) {
	return r.git(ctx, nil, nil, "cat-file", "blob", blob)
}

// BuildTree writes a tree containing base's tree (if base is not empty)
// plus the given entries, using a private index file so the user's index
// is never touched. The index file is left matching the returned tree.
func (r *Repo) BuildTree(ctx context.Context, indexFile, base string, add []TreeEntry) (string, error) {
	readArgs := []string{"read-tree", "--empty"}
	if base != "" {
		readArgs = []string{"read-tree", base}
	}
	if _, err := r.git(ctx, nil, []string{"GIT_INDEX_FILE=" + indexFile}, readArgs...); err != nil {
		return "", err
	}
	return r.ExtendTree(ctx, indexFile, add)
}

// ExtendTree adds entries to the private index file and writes its tree.
func (r *Repo) ExtendTree(ctx context.Context, indexFile string, add []TreeEntry) (string, error) {
	env := []string{"GIT_INDEX_FILE=" + indexFile}
	if len(add) > 0 {
		var info strings.Builder
		for _, e := range add {
			fmt.Fprintf(&info, "100644 %s\t%s\x00", e.Blob, e.Path)
		}
		if _, err := r.git(ctx, strings.NewReader(info.String()), env, "update-index", "-z", "--add", "--index-info"); err != nil {
			return "", err
		}
	}
	return trim(r.git(ctx, nil, env, "write-tree"))
}

// CommitTree creates a commit of tree with the given parents and message.
func (r *Repo) CommitTree(ctx context.Context, tree string, parents []string, message string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	return trim(r.git(ctx, strings.NewReader(message), ledgerIdentity, args...))
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.git(ctx, nil, nil, "merge-base", "--is-ancestor", a, b)
	var ge *Error
	if errors.As(err, &ge) && ge.ExitCode == 1 {
		return false, nil
	}
	return err == nil, err
}

// UpdateRefCAS moves ref from old to new atomically. old == "" requires
// that the ref does not exist. If the ref is not at old, it fails with
// code "ref_moved" and leaves the ref unchanged.
func (r *Repo) UpdateRefCAS(ctx context.Context, ref, newID, old string) error {
	expected := old
	if expected == "" {
		expected = strings.Repeat("0", len(newID))
	}
	_, err := r.git(ctx, nil, nil, "update-ref", ref, newID, expected)
	if err == nil {
		return nil
	}
	current, rerr := r.ResolveRef(ctx, ref)
	if rerr == nil && current != old {
		return apierr.New("ref_moved", "%s is at %q, expected %q", ref, current, old).With("current", current)
	}
	return err
}
