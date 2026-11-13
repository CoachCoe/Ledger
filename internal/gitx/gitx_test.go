package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

// newRepo creates a repository with one commit.
func newRepo(t *testing.T) *Repo {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	env := []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
	for _, args := range [][]string{
		{"init", "-q"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if _, err := Run(ctx, dir, nil, append(env, "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@invalid"), args...); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOpen(t *testing.T) {
	r := newRepo(t)
	sub := filepath.Join(r.Dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Open(context.Background(), sub)
	if err != nil || got.Dir != r.Dir || got.CommonDir != filepath.Join(r.Dir, ".git") || got.MainDir != r.Dir {
		t.Fatalf("Open(sub) = %+v, %v", got, err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := r.git(context.Background(), "worktree", "add", "-q", "--detach", wt); err != nil {
		t.Fatal(err)
	}
	linked, err := Open(context.Background(), wt)
	if err != nil || linked.MainDir != r.Dir || linked.Dir == r.Dir {
		t.Fatalf("Open(linked worktree) = %+v, %v", linked, err)
	}
	if _, err := Open(context.Background(), t.TempDir()); !apierr.HasCode(err, "not_a_git_repo") {
		t.Fatalf("Open outside repo = %v", err)
	}
}

func TestErrorCarriesCommandAndStderr(t *testing.T) {
	_, err := Run(context.Background(), t.TempDir(), nil, nil, "cat-file", "blob", "deadbeef")
	var ge *Error
	if !errors.As(err, &ge) || ge.ExitCode == 0 || ge.Stderr == "" || ge.Args[0] != "cat-file" {
		t.Fatalf("err = %#v", err)
	}
}

func TestReplacementObjectsDoNotRetargetStoredContent(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("replacement\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := Run(ctx, r.Dir, nil, nil, "hash-object", "-w", a, b)
	if err != nil {
		t.Fatal(err)
	}
	ids := strings.Fields(string(out))
	if _, err := Run(ctx, r.Dir, nil, nil, "update-ref", "refs/replace/"+ids[0], ids[1]); err != nil {
		t.Fatal(err)
	}
	raw, err := Run(ctx, r.Dir, nil, nil, "cat-file", "blob", ids[0])
	if err != nil || string(raw) != "original\n" {
		t.Fatalf("replacement object changed signed content: %s %v", raw, err)
	}
}

func TestReadCommit(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	head, err := repo.ResolveRef(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.ReadCommit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := trim(repo.git(ctx, "rev-parse", head+"^{tree}"))
	if err != nil {
		t.Fatal(err)
	}
	if c.SHA != head || c.Tree != tree || len(c.Parents) != 0 || c.Message == "" {
		t.Fatalf("unexpected commit %+v", c)
	}
	if _, err := repo.ReadCommit(ctx, "0123456789012345678901234567890123456789"); !apierr.HasCode(err, "dependency_unavailable") {
		t.Fatalf("missing commit: %v", err)
	}
	if _, err := repo.ReadCommit(ctx, "--help"); !apierr.HasCode(err, "invalid_commit") {
		t.Fatalf("option-like id: %v", err)
	}
	if _, err := repo.ReadCommit(ctx, tree); !apierr.HasCode(err, "dependency_unavailable") {
		t.Fatalf("tree as commit: %v", err)
	}
}
