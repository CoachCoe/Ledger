// Package observe provides platform watchers as coalesced triggers.
// Snapshots, never watcher event histories, are the source of write sets.
package observe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/CoachCoe/Ledger/internal/ignore"
)

type WorkspaceObserver interface{ Close() error }

// Filter decides which event paths under a workspace root are ignored.
type Filter interface {
	Ignored(ctx context.Context, path string) bool
}

type Manager struct {
	mu       sync.Mutex
	watches  map[string]WorkspaceObserver
	paths    map[string]string
	Notify   func()
	Errors   chan error
	Disabled bool
	// Filter returns the ignore filter for a workspace root.
	Filter func(root string) (Filter, error)
}

func New(disabled bool, filter func(root string) (Filter, error), notify func()) *Manager {
	return &Manager{watches: map[string]WorkspaceObserver{}, paths: map[string]string{}, Notify: notify, Errors: make(chan error, 1), Disabled: disabled, Filter: filter}
}

// Watch starts one watcher on root, outside any Manager.
func Watch(root string, filter Filter, notify func(), report func(error)) (WorkspaceObserver, error) {
	return start(root, filter, notify, report)
}

func (m *Manager) Sync(paths map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, w := range m.watches {
		if paths[id] != m.paths[id] {
			if err := w.Close(); err != nil {
				return err
			}
			delete(m.watches, id)
			delete(m.paths, id)
		}
	}
	if m.Disabled {
		return nil
	}
	for id, path := range paths {
		if m.watches[id] != nil {
			continue
		}
		filter, err := m.Filter(path)
		if err != nil {
			return err
		}
		w, err := start(path, filter, m.Notify, func(err error) {
			select {
			case m.Errors <- err:
			default:
			}
		})
		if err != nil {
			return err
		}
		m.watches[id] = w
		m.paths[id] = path
	}
	return nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []error
	for id, w := range m.watches {
		errs = append(errs, w.Close())
		delete(m.watches, id)
	}
	return errors.Join(errs...)
}

// RulesFilter returns filters that follow .gitignore and .ledgerignore
// rules without Git, as native workspace snapshots do.
func RulesFilter(caseSensitive bool) func(root string) (Filter, error) {
	return func(root string) (Filter, error) {
		return &rulesFilter{root: root, tree: ignore.New(root, caseSensitive, ".git", ".ledger")}, nil
	}
}

type rulesFilter struct {
	mu   sync.Mutex
	root string
	tree *ignore.Tree
}

// Ignored reports false when the rules cannot be read: a spurious trigger
// costs one snapshot, a missed one loses an observation.
func (f *rulesFilter) Ignored(_ context.Context, path string) bool {
	rel, err := filepath.Rel(f.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	if rel == "." {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if slices.Contains(ignore.Files, filepath.Base(rel)) {
		f.tree.Invalidate()
		return false
	}
	fi, err := os.Lstat(path)
	ignored, err := f.tree.Ignored(filepath.ToSlash(rel), err == nil && fi.IsDir())
	return err == nil && ignored
}
