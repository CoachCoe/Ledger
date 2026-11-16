// Package bridge converts between Git history and the native store (ADR
// 0028). Git is read and written only in an inert bare staging repository
// with system and global configuration disabled, hooks pointed at
// /dev/null and transferred objects checked, so nothing a source
// repository configures runs during import or export.
package bridge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/gitx"
)

// hardened is the environment of every staging command: no system or
// global configuration, so no user filters, credential helpers, aliases or
// attributes apply.
var hardened = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1"}

// safe precedes every staging command's arguments.
var safe = []string{"-c", "transfer.fsckObjects=true", "-c", "core.attributesFile=/dev/null", "-c", "protocol.allow=never", "-c", "protocol.file.allow=always"}

// Staging is Ledger's bare Git repository for one object format.
type Staging struct {
	Dir    string
	Format string // sha1 or sha256
}

// OpenStaging creates or opens the staging repository for format under
// dir, which Ledger owns.
func OpenStaging(ctx context.Context, dir, format string) (*Staging, error) {
	if format != "sha1" && format != "sha256" {
		return nil, apierr.New("unsupported_repository", "Git object format %q is not supported", format)
	}
	s := &Staging{Dir: filepath.Join(dir, "bridge-"+format+".git"), Format: format}
	if _, err := os.Stat(filepath.Join(s.Dir, "HEAD")); err == nil {
		return s, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := gitx.Run(ctx, dir, nil, hardened, append(safe, "init", "-q", "--bare", "--object-format="+format, s.Dir)...); err != nil {
		return nil, err
	}
	return s, nil
}

// Git runs git in the staging repository.
func (s *Staging) Git(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return gitx.Run(ctx, s.Dir, stdin, hardened, append(safe, args...)...)
}

// ObjectFormat returns the object format of the Git repository at dir.
func ObjectFormat(ctx context.Context, dir string) (string, error) {
	out, err := gitx.Run(ctx, dir, nil, hardened, append(safe, "rev-parse", "--show-object-format")...)
	if err != nil {
		return "", apierr.New("not_a_git_repo", "%s is not a Git repository", dir)
	}
	return strings.TrimSpace(string(out)), nil
}

// catFile reads objects from the staging repository through one
// long-running git cat-file --batch.
type catFile struct {
	mu  sync.Mutex
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func (s *Staging) catFile(ctx context.Context) (*catFile, error) {
	args := append(append([]string{"-c", "core.hooksPath=/dev/null"}, safe...), "cat-file", "--batch")
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // Ledger-built arguments in Ledger's staging repository
	cmd.Dir = s.Dir
	cmd.Env = append(append(os.Environ(), hardened...), "GIT_DIR="+s.Dir, "LC_ALL=C", "GIT_NO_REPLACE_OBJECTS=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, apierr.New("git_unavailable", "running git: %v", err)
	}
	return &catFile{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<16)}, nil
}

// read returns the type and content of object sha.
func (c *catFile) read(sha string) (string, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := io.WriteString(c.in, sha+"\n"); err != nil {
		return "", nil, err
	}
	header, err := c.out.ReadString('\n')
	if err != nil {
		return "", nil, err
	}
	f := strings.Fields(header)
	if len(f) == 2 && f[1] == "missing" {
		return "", nil, apierr.New("dependency_unavailable", "Git object %s is missing", sha)
	}
	if len(f) != 3 {
		return "", nil, fmt.Errorf("bridge: unexpected cat-file header %q", header)
	}
	n, err := strconv.Atoi(f[2])
	if err != nil {
		return "", nil, err
	}
	data := make([]byte, n+1)
	if _, err := io.ReadFull(c.out, data); err != nil {
		return "", nil, err
	}
	return f[1], data[:n], nil
}

func (c *catFile) close() error {
	_ = c.in.Close()
	return c.cmd.Wait()
}
