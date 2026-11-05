package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
		if _, err := Run(ctx, dir, nil, append(env, ledgerIdentity...), args...); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string][3]int{
		"git version 2.54.0 (Apple Git-157)\n": {2, 54, 0},
		"git version 2.25.1":                   {2, 25, 1},
		"git version 2.39.5.windows.1":         {2, 39, 5},
		"git version 3.0":                      {3, 0, 0},
	} {
		if got, err := parseVersion(in); err != nil || got != want {
			t.Errorf("parseVersion(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := parseVersion("hg 6"); err == nil {
		t.Error("accepted non-git version")
	}
	if err := CheckVersion(context.Background()); err != nil {
		t.Fatalf("installed git rejected: %v", err)
	}
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
	if _, err := r.git(context.Background(), nil, nil, "worktree", "add", "-q", "--detach", wt); err != nil {
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

func TestTreeCommitAndCAS(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	userIndex, _ := os.ReadFile(filepath.Join(r.CommonDir, "index"))
	f := filepath.Join(t.TempDir(), "obj\nwith\tquotes\"")
	if err := os.WriteFile(f, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blobs, err := r.WriteBlobs(ctx, []string{f})
	if err != nil {
		t.Fatal(err)
	}
	idx := filepath.Join(t.TempDir(), "index")
	tree, err := r.BuildTree(ctx, idx, "", []TreeEntry{{Path: "ab/cd.json", Blob: blobs[0]}})
	if err != nil {
		t.Fatal(err)
	}
	c1, err := r.CommitTree(ctx, tree, nil, "records\n")
	if err != nil {
		t.Fatal(err)
	}
	const ref = "refs/ledger/records"
	if err := r.UpdateRefCAS(ctx, ref, c1, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRefCAS(ctx, ref, c1, ""); !apierr.HasCode(err, "ref_moved") {
		t.Fatalf("create over existing ref = %v", err)
	}
	tree2, err := r.BuildTree(ctx, idx, c1, []TreeEntry{{Path: "ef/01.json", Blob: blobs[0]}})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.CommitTree(ctx, tree2, []string{c1}, "records\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRefCAS(ctx, ref, c2, c1); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRefCAS(ctx, ref, c1, c1); !apierr.HasCode(err, "ref_moved") {
		t.Fatalf("stale CAS = %v", err)
	}
	if got, _ := r.ResolveRef(ctx, ref); got != c2 {
		t.Fatalf("ref at %s after failed CAS, want %s", got, c2)
	}
	entries, err := r.ListTree(ctx, c2)
	if err != nil || len(entries) != 2 || entries[0].Path != "ab/cd.json" || entries[1].Path != "ef/01.json" {
		t.Fatalf("ListTree = %v, %v", entries, err)
	}
	if b, err := r.ReadBlob(ctx, entries[1].Blob); err != nil || string(b) != "hello\n" {
		t.Fatalf("ReadBlob = %q, %v", b, err)
	}
	if ok, err := r.IsAncestor(ctx, c1, c2); err != nil || !ok {
		t.Fatalf("IsAncestor(c1, c2) = %v, %v", ok, err)
	}
	if ok, err := r.IsAncestor(ctx, c2, c1); err != nil || ok {
		t.Fatalf("IsAncestor(c2, c1) = %v, %v", ok, err)
	}
	if missing, err := r.ResolveRef(ctx, "refs/ledger/none"); err != nil || missing != "" {
		t.Fatalf("ResolveRef(missing) = %q, %v", missing, err)
	}
	if after, _ := os.ReadFile(filepath.Join(r.CommonDir, "index")); string(after) != string(userIndex) {
		t.Fatal("user index changed")
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
	ids, err := r.WriteBlobs(ctx, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRefCAS(ctx, "refs/replace/"+ids[0], ids[1], ""); err != nil {
		t.Fatal(err)
	}
	raw, err := r.ReadBlob(ctx, ids[0])
	if err != nil || string(raw) != "original\n" {
		t.Fatalf("replacement object changed signed content: %s %v", raw, err)
	}
}
