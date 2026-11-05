package workspace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/gitx"
)

func TestSnapshotKeepsFixedBaseAndUserIndex(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	index := filepath.Join(t.TempDir(), "private.index")
	env := []string{"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@invalid"}
	run := func(args ...string) []byte {
		out, err := gitx.Run(ctx, root, nil, env, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(root, "a"), []byte("base\n"), 0600)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0600)
	run("add", ".")
	run("commit", "-qm", "base")
	base := strings.TrimSpace(string(run("rev-parse", "HEAD")))
	os.WriteFile(filepath.Join(root, "a"), []byte("staged\n"), 0600)
	run("add", "a")
	os.WriteFile(filepath.Join(root, "a"), []byte("live\n"), 0700)
	os.WriteFile(filepath.Join(root, "new\nname"), []byte("untracked\n"), 0600)
	os.Mkdir(filepath.Join(root, "ignored"), 0700)
	os.WriteFile(filepath.Join(root, "ignored", "x"), []byte("ignored\n"), 0600)
	before, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Capture(ctx, root, base, index)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(snap.Files, []string{"a", "new\nname"}) {
		t.Fatal(snap.Files)
	}
	after, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	if !bytes.Equal(before, after) {
		t.Fatal("user index changed")
	}
	run("commit", "-qam", "agent commit")
	again, err := Capture(ctx, root, base, index)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(again.Files, snap.Files) {
		t.Fatal("agent commit disappeared from write set")
	}
	stable, err := StableSnapshot(ctx, root, base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if stable.Tree != again.Tree {
		t.Fatal("stable capture changed tree")
	}
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("later edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := string(run("show", stable.Tree+":a")); got != "live\n" {
		t.Fatalf("later edit changed captured tree: %q", got)
	}
}

func TestStableSnapshotRejectsCaptureTimeEdits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	env := []string{"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@invalid"}
	run := func(args ...string) string {
		t.Helper()
		out, err := gitx.Run(ctx, root, nil, env, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "changing"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "base")
	base := run("rev-parse", "HEAD")
	// A clean filter deterministically edits the source after reading it,
	// exercising a real file change during every git-add capture.
	script := filepath.Join(t.TempDir(), "change.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nvalue=$(cat)\nprintf '%s\\n' \"$value\"\nprintf '%sx\\n' \"$value\" > changing\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("changing filter=changing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("config", "filter.changing.clean", "'"+script+"'")
	run("config", "filter.changing.required", "true")
	if _, err := StableSnapshot(ctx, root, base, t.TempDir()); !apierr.HasCode(err, "concurrent_edit") {
		t.Fatalf("capture-time edits accepted: %v", err)
	}
}

func TestSnapshotCoversRenameDeleteModeAndSymlink(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	env := []string{"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@invalid"}
	run := func(args ...string) string {
		t.Helper()
		out, err := gitx.Run(ctx, root, nil, env, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	for _, name := range []string{"a", "deleted", "mode"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-qm", "base")
	base := run("rev-parse", "HEAD")
	run("config", "core.filemode", "false") // private snapshots must still capture executable bits
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "renamed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "deleted")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "mode"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("renamed", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	snap, err := Capture(ctx, root, base, filepath.Join(t.TempDir(), "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(snap.Files, []string{"a", "deleted", "link", "mode", "renamed"}) {
		t.Fatal(snap.Files)
	}
	if snap.Status["a"] != "D" || snap.Status["renamed"] != "A" || snap.Status["deleted"] != "D" {
		t.Fatal(snap.Status)
	}
	tree := run("ls-tree", "-r", snap.Tree)
	if !strings.Contains(tree, "100755 blob") || !strings.Contains(tree, "120000 blob") {
		t.Fatal(tree)
	}
}

func TestCommittedUnpopulatedSubmoduleChangeRejected(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	env := []string{"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@invalid"}
	run := func(args ...string) string {
		t.Helper()
		out, err := gitx.Run(ctx, root, nil, env, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("commit", "--allow-empty", "-qm", "base")
	base := run("rev-parse", "HEAD")
	run("update-index", "--add", "--cacheinfo", "160000,"+base+",sub")
	run("commit", "-qm", "agent submodule")
	if _, err := Capture(ctx, root, base, filepath.Join(t.TempDir(), "index")); !apierr.HasCode(err, "unsupported_submodule") {
		t.Fatal(err)
	}
}
