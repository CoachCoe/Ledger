package workspace

import (
	"context"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"golang.org/x/text/unicode/norm"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

type Snapshot struct {
	Tree   string            `json:"tree"`
	Files  []string          `json:"files"`
	Status map[string]string `json:"status"`
}

// Capture observes the complete difference from a fixed base, including
// agent-made commits. indexFile is private and may persist between observations.
func Capture(ctx context.Context, path, base, indexFile string) (Snapshot, error) {
	env := []string{"GIT_INDEX_FILE=" + indexFile}
	run := func(args ...string) ([]byte, error) {
		return gitx.Run(ctx, path, nil, env, append([]string{"-c", "core.filemode=true", "-c", "core.symlinks=true"}, args...)...)
	}
	if _, err := os.Stat(indexFile); os.IsNotExist(err) {
		if _, err := run("read-tree", base); err != nil {
			return Snapshot{}, err
		}
	} else if err != nil {
		return Snapshot{}, err
	}
	head, err := run("diff", "--raw", "--no-abbrev", "--no-renames", "-z", base, "HEAD", "--")
	if err != nil {
		return Snapshot{}, err
	}
	parts := strings.Split(strings.TrimSuffix(string(head), "\x00"), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		m := strings.Fields(parts[i])
		if len(m) == 5 && (strings.TrimPrefix(m[0], ":") == "160000" || m[1] == "160000") {
			return Snapshot{}, apierr.New("unsupported_submodule", "committed submodule changes are unsupported: %s", parts[i+1])
		}
	}
	if _, err := run("add", "-A", "--", "."); err != nil {
		return Snapshot{}, err
	}
	out, err := run("write-tree")
	if err != nil {
		return Snapshot{}, err
	}
	tree := strings.TrimSpace(string(out))
	out, err = run("diff", "--raw", "--no-abbrev", "--no-renames", "-z", base, tree, "--")
	if err != nil {
		return Snapshot{}, err
	}
	result := Snapshot{Tree: tree, Files: []string{}, Status: map[string]string{}}
	parts = strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(out) == 0 {
		return result, nil
	}
	for i := 0; i < len(parts); i += 2 {
		if i+1 >= len(parts) {
			return Snapshot{}, apierr.New("git_output", "malformed diff output")
		}
		meta := strings.Fields(parts[i])
		if len(meta) != 5 {
			return Snapshot{}, apierr.New("git_output", "malformed diff metadata")
		}
		if strings.TrimPrefix(meta[0], ":") == "160000" || meta[1] == "160000" {
			return Snapshot{}, apierr.New("unsupported_submodule", "submodule changes are not supported: %s", parts[i+1])
		}
		name := parts[i+1]
		if !utf8.ValidString(name) {
			return Snapshot{}, apierr.New("invalid_path", "file path is not UTF-8")
		}
		result.Files = append(result.Files, norm.NFC.String(name))
		result.Status[norm.NFC.String(name)] = meta[4]
	}
	slices.Sort(result.Files)
	return result, nil
}

// StableSnapshot uses independent private indexes to detect capture-time
// edits. It is bounded and deliberately does not claim an atomic filesystem snapshot.
func StableSnapshot(ctx context.Context, path, base, runtimeDir string) (Snapshot, error) {
	var last Snapshot
	for range 3 {
		pair := make([]Snapshot, 2)
		for i := range pair {
			tmp, err := os.CreateTemp(runtimeDir, "capture-*")
			if err != nil {
				return last, err
			}
			name := tmp.Name()
			tmp.Close()
			os.Remove(name)
			snap, err := Capture(ctx, path, base, name)
			os.Remove(name)
			os.Remove(name + ".lock")
			if err != nil {
				return last, err
			}
			pair[i] = snap
		}
		if pair[0].Tree == pair[1].Tree {
			return pair[1], nil
		}
		last = pair[1]
	}
	return last, apierr.New("concurrent_edit", "files changed during proposal capture; retry when writes settle")
}
func IndexPath(runtimeDir, id string) string {
	return filepath.Join(runtimeDir, "observe-"+id+".index")
}
