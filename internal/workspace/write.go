package workspace

import (
	"io"
	"os"
	"path"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// writeText replaces one regular text file under the workspace root at
// workspaceDir atomically. It never follows a symlink or writes Ledger
// metadata. A retry after replacement succeeds if the exact desired bytes are
// already present; no signed history is implied.
func writeText(workspaceDir, name, content string, expected *string) (string, error) {
	if len(content) > 262144 || !utf8.ValidString(content) {
		return "", apierr.New("invalid_content", "text must be valid UTF-8 and at most 256 KiB")
	}
	n, err := capability.Normalize(name, true)
	if err != nil {
		return "", err
	}
	if n != name {
		return "", apierr.New("invalid_path", "path must use NFC normalization")
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") || strings.EqualFold(part, ".ledger") {
			return "", apierr.New("invalid_path", "metadata cannot be edited")
		}
	}
	root, err := os.OpenRoot(workspaceDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	parts := strings.Split(name, "/")
	for i := range parts {
		part := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(part)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", apierr.New("invalid_path", "symlinks cannot be edited")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", apierr.New("invalid_path", "parent is not a directory")
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return "", apierr.New("invalid_path", "target must be a regular file")
		}
	}
	want := canon.Hash([]byte(content))
	mode := os.FileMode(0600)
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	switch {
	case err == nil:
		info, e := f.Stat()
		if e != nil {
			_ = f.Close()
			return "", e
		}
		if !info.Mode().IsRegular() {
			_ = f.Close()
			return "", apierr.New("invalid_path", "target must be a regular file")
		}
		mode = info.Mode().Perm()
		raw, e := io.ReadAll(io.LimitReader(f, 262145))
		_ = f.Close()
		if e != nil {
			return "", e
		}
		current := canon.Hash(raw)
		if current == want {
			return want, nil
		}
		if len(raw) > 262144 || expected == nil || *expected != current {
			return "", apierr.New("content_conflict", "file changed; supply its current SHA-256")
		}
	case !os.IsNotExist(err):
		return "", err
	case expected != nil:
		return "", apierr.New("content_conflict", "file no longer exists")
	}
	if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
		return "", err
	}
	tmp := path.Join(path.Dir(name), ".ledger-edit-"+schema.NewID(""))
	out, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Remove(tmp) }()
	err = out.Chmod(mode)
	if err == nil {
		_, err = out.WriteString(content)
	}
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := root.Rename(tmp, name); err != nil {
		return "", err
	}
	dir, err := root.Open(path.Dir(name))
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return "", err
	}
	return want, nil
}
