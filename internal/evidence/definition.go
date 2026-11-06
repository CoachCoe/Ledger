// Package evidence resolves exact manifest/commit/check-definition
// bindings and (from M3.2) runs checks in verification worktrees.
package evidence

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/CoachCoe/Ledger/internal/schema"
)

// MaxOutputLimit is the largest output_limit_bytes Ledger will run with.
// The record schema allows 1 GiB; evidence execution caps it lower.
const MaxOutputLimit = 64 << 20

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	envPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// ValidateDefinition checks that d is a safe, runnable-shaped check
// definition. It applies to every category; Runnable adds the rule that
// judgment checks are not executed.
func ValidateDefinition(d schema.CheckDefinition) error {
	if !namePattern.MatchString(d.Name) {
		return fmt.Errorf("name %q must match %s", d.Name, namePattern)
	}
	switch d.Category {
	case "mechanical", "behavioral", "judgment":
	default:
		return fmt.Errorf("category %q is not mechanical, behavioral or judgment", d.Category)
	}
	if d.Setup != nil {
		if err := validCommand("setup", *d.Setup); err != nil {
			return err
		}
	}
	if err := validCommand("command", d.Command); err != nil {
		return err
	}
	if err := validWorkingDirectory(d.WorkingDirectory); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, name := range d.EnvAllowlist {
		switch {
		case !envPattern.MatchString(name):
			return fmt.Errorf("env_allowlist name %q is not a valid variable name", name)
		case seen[name]:
			return fmt.Errorf("env_allowlist repeats %q", name)
		}
		seen[name] = true
	}
	if d.TimeoutSeconds < 1 || d.TimeoutSeconds > 86400 {
		return fmt.Errorf("timeout_seconds %d is outside 1..86400", d.TimeoutSeconds)
	}
	if d.OutputLimitBytes < 0 || d.OutputLimitBytes > MaxOutputLimit {
		return fmt.Errorf("output_limit_bytes %d is outside 0..%d", d.OutputLimitBytes, MaxOutputLimit)
	}
	return nil
}

// Runnable reports whether a definition may be executed. Judgment checks
// are answered by reviewers, never run.
func Runnable(d schema.CheckDefinition) error {
	if d.Category == "judgment" {
		return fmt.Errorf("check %s is a judgment check and cannot be executed", d.Name)
	}
	return nil
}

func validCommand(field, command string) error {
	switch {
	case strings.TrimSpace(command) == "":
		return fmt.Errorf("%s must not be empty", field)
	case strings.ContainsRune(command, 0):
		return fmt.Errorf("%s must not contain NUL", field)
	}
	return nil
}

// validWorkingDirectory requires a normalized repository-relative path:
// no absolute path, traversal, backslash, NUL, empty or "." segment other
// than the lone ".", and NFC form.
func validWorkingDirectory(dir string) error {
	switch {
	case dir == "":
		return fmt.Errorf("working_directory must not be empty")
	case strings.ContainsAny(dir, "\\\x00"):
		return fmt.Errorf("working_directory %q contains a backslash or NUL", dir)
	case strings.HasPrefix(dir, "/"):
		return fmt.Errorf("working_directory %q must be relative to the repository", dir)
	case !norm.NFC.IsNormalString(dir):
		return fmt.Errorf("working_directory %q is not NFC-normalized", dir)
	case path.Clean(dir) != dir:
		return fmt.Errorf("working_directory %q is not a clean path", dir)
	case dir == ".." || strings.HasPrefix(dir, "../"):
		return fmt.Errorf("working_directory %q escapes the repository", dir)
	}
	return nil
}
