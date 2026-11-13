package cow

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/CoachCoe/Ledger/internal/ignore"
	"github.com/CoachCoe/Ledger/internal/observe"
)

type filter struct {
	m     *Manager
	id    string
	root  string
	rules *ignore.Tree
}

// Filter returns the watcher filter for workspace id: the workspace's
// ignore rules, except that tracked paths are always reported.
func (m *Manager) Filter(id string) observe.Filter {
	root := m.Path(id)
	return filter{m: m, id: id, root: root, rules: m.rules(root)}
}

func (f filter) Ignored(_ context.Context, p string) bool {
	rel, err := filepath.Rel(f.root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	if rel == "." {
		return false
	}
	rel = filepath.ToSlash(rel)
	switch path.Base(rel) {
	case ".gitignore", ".ledgerignore":
		f.rules.Invalidate()
		return false
	}
	if f.m.isTracked(f.id, rel) {
		return false
	}
	info, err := os.Lstat(p)
	ignored, err2 := f.rules.Ignored(rel, err == nil && info.IsDir())
	return err2 == nil && ignored
}

// setTracked records the paths of workspace id's latest snapshot, and
// every directory above them.
func (m *Manager) setTracked(id string, c statCache) {
	set := trackedSet(c)
	m.mu.Lock()
	m.tracked[id] = set
	m.mu.Unlock()
}

// trackedSet is never changed once built, so workspaces may share one.
func trackedSet(c statCache) map[string]bool {
	set := make(map[string]bool, len(c.Files))
	for p := range c.Files {
		set[p] = true
		for d := path.Dir(p); d != "." && !set[d]; d = path.Dir(d) {
			set[d] = true
		}
	}
	return set
}

func (m *Manager) isTracked(id, rel string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tracked[id][rel]
}
