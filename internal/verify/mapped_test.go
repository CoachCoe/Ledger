package verify

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/schema"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=C", "GIT_COMMITTER_EMAIL=c@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// A mapping stands in for a Phase 1 commit only when its fields hash to
// the commit's id, in either object format.
func TestMappedCommitID(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		dir := t.TempDir()
		gitOut(t, dir, "init", "-q", "--object-format="+format)
		gitOut(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
		gitOut(t, dir, "commit", "-q", "--allow-empty", "-m", "Add refunds\n\nLedger-Manifest: abc")
		sha := strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))
		headers, message, _ := strings.Cut(gitOut(t, dir, "cat-file", "commit", sha), "\n\n")
		m := schema.BridgeMapping{GitCommit: sha, Message: message, Direction: "import"}
		for line := range strings.SplitSeq(headers, "\n") {
			key, value, _ := strings.Cut(line, " ")
			switch key {
			case "tree":
				m.GitTree = value
			case "parent":
				m.GitParents = append(m.GitParents, value)
			case "author":
				m.Author = value
			case "committer":
				m.Committer = value
			}
		}
		if got := commitID(m); got != sha {
			t.Fatalf("%s: rebuilt %s, commit %s", format, got, sha)
		}
		m.Message += "tampered\n"
		if commitID(m) == sha {
			t.Fatalf("%s: an altered message reproduced the commit id", format)
		}
	}
}
