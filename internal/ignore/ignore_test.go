package ignore

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var files = []string{
	"README.md", "readme.MD", "notes.txt", "debug.log", "app/debug.log", "app/keep.log",
	"build/out.js", "build/sub/x.js", "src/build/y.js", "src/build",
	"node_modules/a/index.js", "pkg/node_modules/b/index.js",
	"dist/a.map", "src/dist/b.map", "doc/a.md", "doc/sub/b.md", "doc/sub/deeper/c.md",
	"a/b/c/d.tmp", "a/x/c/d.tmp", "a/c/d.tmp", "foo/bar/baz", "foo/bar", "x/foo/bar",
	"lit[1].txt", "q1.txt", "q12.txt", "#hash", "!bang", "space ", "trail\\ ",
	"cache/keep/me.txt", "cache/drop.txt", "logs/today/a.log", "logs/keep.log",
	"src/gen/a.pb.go", "src/gen/sub/b.pb.go", "src/gen/README", "CaseDir/file",
	"only/dir/x", "only/file", "env/.env", ".env", ".env.example", "été.txt",
}

var rules = map[string]string{
	".gitignore": strings.Join([]string{
		"# comment", "", "*.log", "!app/keep.log", "/build/", "dist", "node_modules/",
		"doc/**/*.md", "!doc/sub/b.md", "a/**/d.tmp", "foo/bar", "lit\\[1\\].txt", "q?.txt",
		"\\#hash", "\\!bang", "space", "trail\\ ", "cache/*", "!cache/keep/", "logs/",
		"!logs/keep.log", "**/gen/**/*.pb.go", "casedir/", "only/*/", ".env*", "!.env.example",
		"été.txt\r",
	}, "\n"),
	"src/.gitignore": "build\n!dist\n",
	"app/.gitignore": "!debug.log\n",
}

func write(t *testing.T, root string) {
	t.Helper()
	for _, f := range files {
		full := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(full); err == nil && info.IsDir() {
			continue
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil && !strings.Contains(err.Error(), "directory") {
			t.Fatal(err)
		}
	}
	for name, content := range rules {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func walk(t *testing.T, tree *Tree, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		ignored, err := tree.Ignored(rel, d.IsDir())
		if err != nil {
			return err
		}
		switch {
		case ignored && d.IsDir():
			return filepath.SkipDir
		case !ignored && !d.IsDir():
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func gitVisible(t *testing.T, root string, ignoreCase bool) []string {
	t.Helper()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return string(out)
	}
	git("init", "-q")
	out := git("-c", "core.ignorecase="+map[bool]string{true: "true", false: "false"}[ignoreCase], "-c", "core.quotepath=false",
		"ls-files", "--others", "--exclude-standard", "-z")
	var visible []string
	for f := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		if f != "" && !strings.HasPrefix(f, ".git/") {
			visible = append(visible, f)
		}
	}
	slices.Sort(visible)
	return visible
}

func TestMatchesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is the reference implementation")
	}
	for _, sensitive := range []bool{true, false} {
		root := t.TempDir()
		write(t, root)
		if !sensitive {
			if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(rules[".gitignore"]+"\nREADME.MD\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got := walk(t, New(root, sensitive, ".git"), root)
		want := gitVisible(t, root, !sensitive)
		if !slices.Equal(got, want) {
			for _, f := range want {
				if !slices.Contains(got, f) {
					t.Errorf("sensitive=%v: %s is visible to Git but ignored", sensitive, f)
				}
			}
			for _, f := range got {
				if !slices.Contains(want, f) {
					t.Errorf("sensitive=%v: %s is ignored by Git but visible", sensitive, f)
				}
			}
		}
	}
}

func TestLedgerignoreOverridesGitignore(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{".gitignore": "*.gen\n", ".ledgerignore": "!keep.gen\nsecret/\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tree := New(root, true, ".git", ".ledger")
	for rel, want := range map[string]bool{"a.gen": true, "keep.gen": false, "secret/x": true, ".ledger/store": true, "src/a.ts": false} {
		got, err := tree.Ignored(rel, false)
		if err != nil || got != want {
			t.Errorf("%s: ignored=%v (%v), want %v", rel, got, err, want)
		}
	}
}
