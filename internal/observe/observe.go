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

	"github.com/CoachCoe/Ledger/internal/gitx"
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
	// Filter returns the ignore filter for a workspace root; it defaults
	// to Git's rules.
	Filter func(root string) (Filter, error)
}

func New(disabled bool, notify func()) *Manager {
	return &Manager{watches: map[string]WorkspaceObserver{}, paths: map[string]string{}, Notify: notify, Errors: make(chan error, 1), Disabled: disabled, Filter: GitFilter}
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

// WatchDirs lists root and every directory holding a tracked or
// unignored file, with one Git call.
func (f *filter) WatchDirs(ctx context.Context) ([]string, error) {
	out, err := gitx.Run(ctx, f.root, nil, nil, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{f.root: true}
	dirs := []string{f.root}
	for _, p := range strings.Split(string(out), "\x00") {
		for d := filepath.Dir(p); d != "." && d != ""; d = filepath.Dir(d) {
			if full := filepath.Join(f.root, d); !seen[full] {
				seen[full] = true
				dirs = append(dirs, full)
			}
		}
	}
	return dirs, nil
}

// GitFilter returns a Filter that follows Git's ignore rules for root.
func GitFilter(root string) (Filter, error) { return newFilter(root) }

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

func (f *filter) Ignored(ctx context.Context, path string) bool {
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
