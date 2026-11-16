package bridge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=A U Thor", "GIT_AUTHOR_EMAIL=author@example.com", "GIT_COMMITTER_NAME=C O Mitter", "GIT_COMMITTER_EMAIL=committer@example.com",
		"GIT_AUTHOR_DATE=1762344000 +0100", "GIT_COMMITTER_DATE=1762344000 +0000")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// source makes a Git repository with three first-parent commits covering
// executables, symlinks, nesting, deletion and a merge from a side branch.
func source(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, map[string]string{"README.md": "hello\n", "src/a.ts": "export const a = 1;\n", "src/deep/b.py": "b = 2\n", "run.sh": "#!/bin/sh\n"})
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src/a.ts", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "first")
	git(t, dir, "checkout", "-q", "-b", "side")
	write(t, dir, map[string]string{"side.txt": "side\n"})
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "side")
	git(t, dir, "checkout", "-q", "main")
	write(t, dir, map[string]string{"src/a.ts": "export const a = 2;\n", "src/c.ts": "export const c = 3;\n"})
	if err := os.Remove(filepath.Join(dir, "src/deep/b.py")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "second")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	return dir
}

type fixture struct {
	ctx     context.Context
	staging *Staging
	objects *store.Store
}

func newFixture(t *testing.T, format string) fixture {
	t.Helper()
	ctx := context.Background()
	s, err := OpenStaging(ctx, t.TempDir(), format)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{ctx, s, objects}
}

func (f fixture) importAll(t *testing.T, src, ref string) []Imported {
	t.Helper()
	tip, err := f.staging.Fetch(f.ctx, src, ref)
	if err != nil {
		t.Fatal(err)
	}
	commits, err := f.staging.FirstParents(f.ctx, tip)
	if err != nil {
		t.Fatal(err)
	}
	b := f.objects.Batch()
	im, err := f.staging.NewImporter(f.ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close()
	var out []Imported
	for _, c := range commits {
		i, err := im.Commit(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, i)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	return out
}

// P2-AC-030, P2-AC-031: imported first-parent history exports back to the
// same Git trees, and the same inputs export to the same commits.
func TestImportExportRoundTrip(t *testing.T) {
	src := source(t)
	f := newFixture(t, "sha1")
	imported := f.importAll(t, src, "refs/heads/main")
	if len(imported) != 3 || imported[2].Message != "merge side\n" || len(imported[2].Parents) != 2 {
		t.Fatalf("imported %+v", imported)
	}
	if imported[0].Author != "A U Thor <author@example.com> 1762344000 +0100" {
		t.Fatalf("author %q", imported[0].Author)
	}
	files, err := f.objects.Files(imported[0].NativeTree)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, file := range files {
		types[file.Path] = file.Type
	}
	if types["run.sh"] != store.TypeExecutable || types["link"] != store.TypeSymlink || types["src/deep/b.py"] != store.TypeFile {
		t.Fatalf("native types %v", types)
	}
	export := func(s *Staging) []Written {
		outs := []Out{}
		for i, c := range imported {
			o := Out{Tree: c.NativeTree, Author: c.Author, Committer: c.Committer, Message: c.Message}
			if i > 0 {
				o.Parent, o.ParentTree = ":"+string(rune('0'+i)), imported[i-1].NativeTree
			}
			outs = append(outs, o)
		}
		w, err := s.Export(f.ctx, f.objects, outs)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	written := export(f.staging)
	for i, w := range written {
		if w.Tree != imported[i].Tree {
			t.Fatalf("commit %d exported tree %s, imported %s", i, w.Tree, imported[i].Tree)
		}
	}
	other, err := OpenStaging(f.ctx, t.TempDir(), "sha1")
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range export(other) {
		if w.SHA != written[i].SHA {
			t.Fatalf("export %d is not deterministic: %s and %s", i, w.SHA, written[i].SHA)
		}
	}
	if got := git(t, src, "rev-parse", "main^{tree}"); written[2].Tree != got {
		t.Fatalf("tip tree %s, source %s", written[2].Tree, got)
	}
}

func TestImportSHA256(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main", "--object-format=sha256")
	write(t, dir, map[string]string{"a.txt": "a\n", "d/b.txt": "b\n"})
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "one")
	format, err := ObjectFormat(context.Background(), dir)
	if err != nil || format != "sha256" {
		t.Fatalf("format %q, %v", format, err)
	}
	f := newFixture(t, format)
	imported := f.importAll(t, dir, "main")
	w, err := f.staging.Export(f.ctx, f.objects, []Out{{Tree: imported[0].NativeTree, Author: imported[0].Author, Committer: imported[0].Committer, Message: imported[0].Message}})
	if err != nil || w[0].Tree != imported[0].Tree || len(w[0].SHA) != 64 {
		t.Fatalf("sha256 round trip %+v, %v", w, err)
	}
}

// P2-AC-032: hooks, filters and configuration in the source repository
// run during neither import nor export.
func TestHostileSourceRunsNothing(t *testing.T) {
	src := source(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	for _, hook := range []string{"post-checkout", "pre-push", "post-merge", "reference-transaction", "post-index-change"} {
		p := filepath.Join(src, ".git", "hooks", hook)
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, src, "config", "filter.evil.smudge", "touch "+marker)
	git(t, src, "config", "filter.evil.clean", "touch "+marker)
	git(t, src, "config", "trailer.sign.command", "touch "+marker)
	git(t, src, "config", "uploadpack.packObjectsHook", "touch "+marker)
	write(t, src, map[string]string{".gitattributes": "* filter=evil\n"})
	git(t, src, "add", ".gitattributes")
	git(t, src, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "attributes")
	if _, err := os.Stat(marker); err == nil {
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
	}
	f := newFixture(t, "sha1")
	imported := f.importAll(t, src, "main")
	last := imported[len(imported)-1]
	if _, err := f.staging.Export(f.ctx, f.objects, []Out{{Tree: last.NativeTree, Author: last.Author, Committer: last.Committer, Message: last.Message}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook, filter or configured command in the source repository ran")
	}
}

func TestImportRefusesNamesTheStoreCannotHold(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, map[string]string{"café.txt": "x\n"})
	git(t, dir, "-c", "core.precomposeUnicode=false", "add", "-A")
	git(t, dir, "commit", "-q", "-m", "nfd")
	f := newFixture(t, "sha1")
	tip, err := f.staging.Fetch(f.ctx, dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	im, err := f.staging.NewImporter(f.ctx, f.objects.Batch())
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close()
	if _, err := im.Commit(tip); !apierr.HasCode(err, "unsupported_path") {
		t.Fatalf("NFD name: %v", err)
	}
	if _, err := f.staging.Fetch(f.ctx, dir, "--upload-pack=touch"); !apierr.HasCode(err, "invalid_ref") {
		t.Fatalf("option as ref: %v", err)
	}
}
