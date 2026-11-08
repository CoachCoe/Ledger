package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// toolchainProbes are the version commands recorded when the tool is found
// in the reduced environment. Absent tools are omitted.
var toolchainProbes = []struct {
	name    string
	command string
}{
	{"node", "node --version"},
	{"python3", "python3 --version"},
	{"go", "go version"},
	{"git", "git --version"},
}

// lockfileNames are hashed when present at the checkout root or the check's
// working directory.
var lockfileNames = []string{
	"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "poetry.lock", "Pipfile.lock",
	"uv.lock", "requirements.txt", "go.sum", "Cargo.lock",
}

const (
	probeTimeout   = 2 * time.Second
	toolVersionMax = 256
)

// BuildEnvironment describes the environment a check will run in and returns
// the child's environment list. The child sees only allowlisted variables
// present in source; the description holds a digest of each value, never the
// value. root is the checkout root and workdir the check's directory.
func BuildEnvironment(ctx context.Context, root, workdir string, d schema.CheckDefinition, source []string) (schema.Environment, []string, error) {
	child, variables := childEnvironment(d, source)
	env := schema.Environment{
		OS: runtime.GOOS, Arch: runtime.GOARCH,
		Toolchains: map[string]string{}, Variables: variables, Lockfiles: map[string]string{},
	}
	for _, probe := range toolchainProbes {
		out := NewBoundedOutput(toolVersionMax)
		res := RunPhase(ctx, PhaseSpec{Command: probe.command, Dir: workdir, Env: child, Timeout: probeTimeout}, out)
		if !res.OK() {
			continue
		}
		if line, _, _ := strings.Cut(strings.TrimSpace(string(out.Bytes())), "\n"); line != "" {
			env.Toolchains[probe.name] = line
		}
	}
	dirs := map[string]bool{root: true, workdir: true}
	for dir := range dirs {
		for _, name := range lockfileNames {
			sum, ok := hashRegular(filepath.Join(dir, name))
			if !ok {
				continue
			}
			rel, err := filepath.Rel(root, filepath.Join(dir, name))
			if err != nil {
				return env, nil, err
			}
			env.Lockfiles[filepath.ToSlash(rel)] = sum
		}
	}
	return env, child, nil
}

// childEnvironment selects the allowlisted variables present in source and
// the digest of each value.
func childEnvironment(d schema.CheckDefinition, source []string) ([]string, map[string]string) {
	have := map[string]string{}
	for _, kv := range source {
		if name, value, ok := strings.Cut(kv, "="); ok {
			have[name] = value
		}
	}
	child := []string{}
	variables := map[string]string{}
	for _, name := range slices.Compact(slices.Sorted(slices.Values(d.EnvAllowlist))) {
		value, ok := have[name]
		if !ok {
			continue
		}
		child = append(child, name+"="+value)
		sum := sha256.Sum256([]byte(value))
		variables[name] = hex.EncodeToString(sum[:])
	}
	return child, variables
}

// hashRegular returns the SHA-256 of a regular file; symlinks and other
// file types are skipped so a link cannot pull in content from outside.
func hashRegular(path string) (string, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), true
}

// EnvironmentHash is the SHA-256 of the canonical JSON of the description.
func EnvironmentHash(e schema.Environment) (string, error) {
	raw, err := canon.Marshal(e)
	if err != nil {
		return "", err
	}
	return canon.Hash(raw), nil
}
