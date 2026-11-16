package cli

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
)

// TestScripts runs the scripts against a real ledger binary rather than the
// test binary, because the client starts the daemon by re-executing itself.
func TestScripts(t *testing.T) {
	// Keep socket paths below macOS's 104-byte sockaddr_un limit.
	root, err := os.MkdirTemp("/tmp", "ledger-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	bin := filepath.Join(root, "bin")
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "ledger"), "./cmd/ledger")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	fixtures, err := filepath.Abs("../../testdata/fixtures")
	if err != nil {
		t.Fatal(err)
	}
	testscript.Run(t, testscript.Params{
		Dir:         "testdata/script",
		WorkdirRoot: root,
		Setup: func(env *testscript.Env) error {
			env.Setenv("PATH", bin+string(os.PathListSeparator)+env.Getenv("PATH"))
			env.Setenv("XDG_CONFIG_HOME", env.WorkDir+"/config")
			env.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
			env.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			env.Setenv("GIT_AUTHOR_NAME", "test")
			env.Setenv("GIT_AUTHOR_EMAIL", "test@invalid")
			env.Setenv("GIT_COMMITTER_NAME", "test")
			env.Setenv("GIT_COMMITTER_EMAIL", "test@invalid")
			env.Defer(func() { stopDaemon(env, filepath.Join(bin, "ledger")) })
			return nil
		},
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"fixture": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) != 2 {
					ts.Fatalf("usage: fixture name dir")
				}
				ts.Check(os.CopyFS(ts.MkAbs(args[1]), os.DirFS(filepath.Join(fixtures, args[0]))))
			},
			"jsonvar": jsonvar,
			"untar":   untar,
		},
	})
}

// untar FILE extracts a tar file of testdata into the work directory,
// keeping file modes, which a repository's Git objects and keys need.
func untar(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: untar file")
	}
	f, err := os.Open(filepath.Join("testdata", args[0]))
	ts.Check(err)
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		ts.Check(err)
		target := ts.MkAbs(filepath.FromSlash(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			ts.Check(os.MkdirAll(target, 0o700))
			ts.Check(os.Chmod(target, fs.FileMode(hdr.Mode)&fs.ModePerm|0o700))
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			ts.Check(err)
			ts.Check(os.WriteFile(target, data, fs.FileMode(hdr.Mode)&fs.ModePerm|0o200))
		default:
			ts.Fatalf("unexpected tar entry %s", hdr.Name)
		}
	}
}

// jsonvar NAME PATH sets NAME to the string at the dot-separated PATH in
// the last command's JSON stdout.
func jsonvar(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 2 {
		ts.Fatalf("usage: jsonvar NAME PATH")
	}
	var v any
	ts.Check(json.Unmarshal([]byte(ts.ReadFile("stdout")), &v))
	for _, key := range strings.Split(args[1], ".") {
		if array, ok := v.([]any); ok {
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(array) {
				ts.Fatalf("invalid array index %q", key)
			}
			v = array[i]
			continue
		}
		object, ok := v.(map[string]any)
		if !ok {
			ts.Fatalf("%s: no object at %q", args[1], key)
		}
		v = object[key]
	}
	s, ok := v.(string)
	if !ok {
		ts.Fatalf("%s is %v, not a string", args[1], v)
	}
	ts.Setenv(args[0], s)
}

// stopDaemon stops a daemon a failed script left running in $WORK/repo.
func stopDaemon(env *testscript.Env, ledger string) {
	repo := filepath.Join(env.WorkDir, "repo")
	if _, err := os.Stat(filepath.Join(repo, ".ledger", "runtime", "ledgerd.sock")); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stop := exec.CommandContext(ctx, ledger, "daemon", "stop")
	stop.Dir = repo
	stop.Env = env.Vars
	if out, err := stop.CombinedOutput(); err != nil {
		env.T().Log("stop daemon:", err, string(out))
	}
}
