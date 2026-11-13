package cow

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/text/unicode/norm"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/oplog"
	"github.com/CoachCoe/Ledger/internal/store"
)

func nfc(p string) string { return norm.NFC.String(p) }

// Restore writes the tree of the entry with hash source back into the
// workspace. The current state is recorded first, and the restore is a new
// entry, so nothing in the log is lost. Ignored files are left alone.
func (m *Manager) Restore(id, source string, session *string) (oplog.Entry, string, error) {
	defer m.lock(id)()
	l, err := m.log(id)
	if err != nil {
		return oplog.Entry{}, "", err
	}
	target, ok := l.Get(source)
	if !ok {
		return oplog.Entry{}, "", apierr.New("operation_unknown", "workspace %s has no entry %s", id, source)
	}
	if _, _, _, err := m.snapshot(id, oplog.Snapshot, "restore", session); err != nil {
		return oplog.Entry{}, "", err
	}
	_, current, err := m.capture(id)
	if err != nil {
		return oplog.Entry{}, "", err
	}
	want, err := m.cfg.Store.Files(target.Tree)
	if err != nil {
		return oplog.Entry{}, "", err
	}
	if err := m.rewrite(m.Path(id), current, want); err != nil {
		return oplog.Entry{}, "", err
	}
	tree, _, err := m.capture(id)
	if err != nil {
		return oplog.Entry{}, "", err
	}
	if tree != target.Tree {
		_, _, _, _ = l.Append(oplog.Entry{Kind: oplog.Snapshot, Tree: tree, Base: target.Base, Trigger: "restore", Session: session}, false)
		return oplog.Entry{}, "", apierr.New("concurrent_edit", "files changed while restoring; the workspace state was recorded, retry the restore")
	}
	src := source
	e, h, _, err := l.Append(oplog.Entry{Kind: oplog.Restore, Tree: tree, Base: target.Base, Source: &src, Trigger: "call", Session: session}, false)
	return e, h, err
}

// rewrite turns the files current, as named on disk under root, into want.
func (m *Manager) rewrite(root string, current, want []store.File) error {
	wanted := map[string]store.File{}
	for _, f := range want {
		wanted[f.Path] = f
	}
	have := map[string]store.File{}
	var dirs []string
	for _, f := range current {
		n := nfc(f.Path)
		if w, ok := wanted[n]; ok && w.Type == f.Type && w.ID == f.ID {
			have[n] = f
			continue
		}
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(f.Path))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			dirs = append(dirs, d)
		}
	}
	// Deepest first; a directory still holding ignored files stays.
	slices.SortFunc(dirs, func(a, b string) int { return strings.Count(b, "/") - strings.Count(a, "/") })
	for _, d := range slices.Compact(dirs) {
		err := os.Remove(filepath.Join(root, filepath.FromSlash(d)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			return err
		}
	}
	for _, f := range want {
		if _, ok := have[f.Path]; ok {
			continue
		}
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // a checkout gets the modes Git gives one
			return apierr.New("workspace_dirty", "cannot restore %s: %v", f.Path, err)
		}
		if err := m.cfg.Store.Checkout(f.Type, f.ID, p); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return apierr.New("workspace_dirty", "ignored content occupies %s", f.Path)
			}
			return err
		}
	}
	return nil
}
