// Package observe provides platform watchers as coalesced triggers. Git
// snapshots, never watcher event histories, are the source of write sets.
package observe

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"

	"github.com/CoachCoe/Ledger/internal/gitx"
)

type WorkspaceObserver interface{ Close() error }
type Manager struct {
	mu       sync.Mutex
	watches  map[string]WorkspaceObserver
	paths    map[string]string
	Notify   func()
	Errors   chan error
	Disabled bool
}

func New(disabled bool, notify func()) *Manager {
	return &Manager{watches: map[string]WorkspaceObserver{}, paths: map[string]string{}, Notify: notify, Errors: make(chan error, 1), Disabled: disabled}
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
		w, err := start(path, m.Notify, func(err error) {
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

func ignored(ctx context.Context, root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return true
	}
	if rel == ".git" || rel == ".ledger" || strings.HasPrefix(rel, ".git/") || strings.HasPrefix(rel, ".ledger/") {
		return true
	}
	_, err = gitx.Run(ctx, root, nil, nil, "check-ignore", "-q", "--", rel)
	return err == nil
}

// filter caches ignored top-level directories. Dependency installs can emit
// hundreds of thousands of events; they must not spawn one Git per file.
// Tracked directories are never elided, even when an ignore pattern matches.
type filter struct {
	root       string
	ignoredTop map[string]bool
	known      map[string]bool
	tracked    map[string]bool
}

func newFilter(root string) (*filter, error) {
	out, err := gitx.Run(context.Background(), root, nil, nil, "ls-files", "--cached", "-z")
	if err != nil {
		return nil, err
	}
	f := &filter{root: root, ignoredTop: map[string]bool{}, known: map[string]bool{}, tracked: map[string]bool{}}
	for _, path := range strings.Split(string(out), "\x00") {
		top, _, _ := strings.Cut(path, "/")
		f.tracked[top] = true
	}
	return f, nil
}

func (f *filter) ignored(ctx context.Context, path string) bool {
	rel, err := filepath.Rel(f.root, path)
	if err != nil {
		return true
	}
	top, _, _ := strings.Cut(rel, string(filepath.Separator))
	if top == ".git" || top == ".ledger" {
		return true
	}
	if f.known[top] && f.ignoredTop[top] {
		return true
	}
	if filepath.Base(rel) == ".gitignore" {
		clear(f.known)
		clear(f.ignoredTop)
		return false
	}
	if !f.known[top] {
		f.known[top] = true
		f.ignoredTop[top] = !f.tracked[top] && ignored(ctx, f.root, filepath.Join(f.root, top))
	}
	if f.ignoredTop[top] {
		return true
	}
	return ignored(ctx, f.root, path)
}
