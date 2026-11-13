// Package cow provides copy-on-write workspaces over the native store
// (ADR 0026): each base tree is materialized once and cloned per
// workspace, observation uses a stat cache instead of a Git index, every
// change is an operation log entry, and ignored dependency trees are
// shared read-only.
package cow

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/ignore"
	"github.com/CoachCoe/Ledger/internal/oplog"
	"github.com/CoachCoe/Ledger/internal/store"
)

type Config struct {
	Store *store.Store
	// Runtime holds workspaces, base trees, shared dependency trees and
	// stat caches. Clones need it on one filesystem.
	Runtime string
	// Logs holds the operation logs, which outlive their workspaces.
	Logs          string
	CaseSensitive bool
	// Human is the default workspace, where shared dependency trees are
	// captured from. Empty disables sharing.
	Human string
	// DepDirs names ignored dependency directories; it defaults to
	// node_modules.
	DepDirs []string
	Workers int
}

type Manager struct {
	cfg   Config
	mu    sync.Mutex
	logs  map[string]*oplog.Log
	locks map[string]*sync.Mutex // per workspace, and per base or dependency key
	// tracked holds each workspace's snapshot paths, for its watcher filter.
	tracked map[string]map[string]bool
	bases   map[string]baseInfo
}

// baseInfo is what every workspace created from one base tree shares.
type baseInfo struct {
	tracked   map[string]bool
	manifests map[string]string
}

func New(cfg Config) (*Manager, error) {
	if cfg.DepDirs == nil {
		cfg.DepDirs = []string{"node_modules"}
	}
	if cfg.Workers == 0 {
		cfg.Workers = 8
	}
	for _, d := range []string{"workspaces", "trees", "deps", "cache", "tmp"} {
		if err := os.MkdirAll(filepath.Join(cfg.Runtime, d), 0o700); err != nil {
			return nil, err
		}
	}
	return &Manager{cfg: cfg, logs: map[string]*oplog.Log{}, locks: map[string]*sync.Mutex{}, tracked: map[string]map[string]bool{}, bases: map[string]baseInfo{}}, nil
}

func (m *Manager) lock(key string) func() {
	m.mu.Lock()
	l := m.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func validWorkspace(id string) bool {
	return strings.HasPrefix(id, "wsp_") && len(id) == 30 && filepath.Base(id) == id
}

// Path returns where workspace id is checked out.
func (m *Manager) Path(id string) string { return filepath.Join(m.cfg.Runtime, "workspaces", id) }

func (m *Manager) cachePath(id string) string {
	return filepath.Join(m.cfg.Runtime, "cache", id+".json")
}

func (m *Manager) basePath(tree string) string { return filepath.Join(m.cfg.Runtime, "trees", tree) }

func (m *Manager) tmpPath(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(m.cfg.Runtime, "tmp", prefix+"-"+hex.EncodeToString(b[:])), nil
}

func (m *Manager) rules(root string) *ignore.Tree {
	return ignore.New(root, m.cfg.CaseSensitive, ".git", ".ledger")
}

func (m *Manager) log(id string) (*oplog.Log, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.logs[id]; l != nil {
		return l, nil
	}
	l, err := oplog.Open(m.cfg.Logs, id)
	if err != nil {
		return nil, err
	}
	m.logs[id] = l
	return l, nil
}

// Log returns workspace id's operation log.
func (m *Manager) Log(id string) ([]oplog.Entry, error) {
	l, err := m.log(id)
	if err != nil {
		return nil, err
	}
	return l.Entries(), nil
}

// ensureBase materializes tree once under trees/ with its stat cache,
// checking that a scan of the result reproduces the tree id.
func (m *Manager) ensureBase(tree string) error {
	defer m.lock("tree:" + tree)()
	dst := m.basePath(tree)
	if _, err := os.Lstat(dst); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := m.tmpPath("tree")
	if err != nil {
		return err
	}
	if err := m.cfg.Store.Materialize(tree, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	files, err := m.cfg.Store.Files(tree)
	if err != nil {
		return err
	}
	tracked := map[string]bool{}
	for _, f := range files {
		tracked[f.Path] = true
	}
	b := m.cfg.Store.Batch()
	res, err := scan(tmp, m.rules(tmp), tracked, "", statCache{Files: map[string]cached{}}, b, m.cfg.Workers)
	if err != nil {
		return err
	}
	got, err := b.BuildTree(res.files)
	if err != nil {
		return err
	}
	if got != tree {
		_ = os.RemoveAll(tmp)
		return apierr.New("materialize_mismatch", "materialized tree %s scans as %s", tree, got)
	}
	// Clones get new inodes, so a clone's first scan matches on the rest.
	for p, e := range res.cache.Files {
		e.Ino = 0
		res.cache.Files[p] = e
	}
	if err := saveCache(dst+".json", res.cache); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// base returns the shared facts about base tree tree, which must be
// materialized.
func (m *Manager) base(tree string) (baseInfo, error) {
	m.mu.Lock()
	info, ok := m.bases[tree]
	m.mu.Unlock()
	if ok {
		return info, nil
	}
	c, err := loadCache(m.basePath(tree) + ".json")
	if err != nil {
		return baseInfo{}, err
	}
	files, err := m.cfg.Store.Files(tree)
	if err != nil {
		return baseInfo{}, err
	}
	info = baseInfo{tracked: trackedSet(c), manifests: manifests(files)}
	m.mu.Lock()
	m.bases[tree] = info
	m.mu.Unlock()
	return info, nil
}

// Create clones base into a new workspace, links its shared dependency
// trees and starts its log with the base tree.
func (m *Manager) Create(id, base string, session *string) (oplog.Entry, error) {
	if !validWorkspace(id) {
		return oplog.Entry{}, apierr.New("invalid_workspace", "invalid workspace id %q", id)
	}
	defer m.lock(id)()
	l, err := m.log(id)
	if err != nil {
		return oplog.Entry{}, err
	}
	if _, _, ok := l.Head(); ok {
		return oplog.Entry{}, apierr.New("workspace_exists", "workspace %s already exists", id)
	}
	// A checkout without a log entry is left by a crash during an earlier
	// Create. The workspace was never handed out, so nothing was edited in
	// it and it is rebuilt.
	if err := os.RemoveAll(m.Path(id)); err != nil {
		return oplog.Entry{}, err
	}
	if err := m.ensureBase(base); err != nil {
		return oplog.Entry{}, err
	}
	tmp, err := m.tmpPath(id)
	if err != nil {
		return oplog.Entry{}, err
	}
	if err := cloneTree(m.basePath(base), tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return oplog.Entry{}, err
	}
	info, err := m.base(base)
	if err != nil {
		return oplog.Entry{}, err
	}
	_ = os.Remove(m.cachePath(id))
	if err := cloneTree(m.basePath(base)+".json", m.cachePath(id)); err != nil {
		return oplog.Entry{}, err
	}
	m.mu.Lock()
	m.tracked[id] = info.tracked
	m.mu.Unlock()
	if err := os.Rename(tmp, m.Path(id)); err != nil {
		return oplog.Entry{}, err
	}
	if err := m.linkDeps(id, info.manifests); err != nil {
		return oplog.Entry{}, err
	}
	e, _, _, err := l.Append(oplog.Entry{Kind: oplog.Snapshot, Tree: base, Base: base, Trigger: "create", Session: session}, false)
	return e, err
}

// Snapshot records the workspace's current tree if it changed. A changed
// dependency manifest first gives the workspace private copies of its
// shared dependency trees.
func (m *Manager) Snapshot(id, trigger string, session *string) (oplog.Entry, string, bool, error) {
	defer m.lock(id)()
	return m.snapshot(id, oplog.Snapshot, trigger, session)
}

// Wrote records the tree after a write through the API as a write entry.
func (m *Manager) Wrote(id string, session *string) (oplog.Entry, string, bool, error) {
	defer m.lock(id)()
	return m.snapshot(id, oplog.Write, "api", session)
}

func (m *Manager) snapshot(id, kind, trigger string, session *string) (oplog.Entry, string, bool, error) {
	l, err := m.log(id)
	if err != nil {
		return oplog.Entry{}, "", false, err
	}
	head, _, ok := l.Head()
	if !ok {
		return oplog.Entry{}, "", false, apierr.New("workspace_unknown", "workspace %s does not exist", id)
	}
	tree, files, err := m.capture(id)
	if err != nil {
		return oplog.Entry{}, "", false, err
	}
	if changed, err := m.manifestsChanged(head.Base, files); err != nil {
		return oplog.Entry{}, "", false, err
	} else if changed {
		if err := m.detach(id, ""); err != nil {
			return oplog.Entry{}, "", false, err
		}
	}
	return l.Append(oplog.Entry{Kind: kind, Tree: tree, Base: head.Base, Trigger: trigger, Session: session}, kind == oplog.Propose)
}

// Propose captures the workspace twice and records the tree as a durable
// propose entry only when both captures agree, retrying a bounded number of
// times. It detects edits during capture; it does not claim an atomic
// filesystem snapshot.
func (m *Manager) Propose(id string, session *string) (oplog.Entry, string, error) {
	defer m.lock(id)()
	l, err := m.log(id)
	if err != nil {
		return oplog.Entry{}, "", err
	}
	head, _, ok := l.Head()
	if !ok {
		return oplog.Entry{}, "", apierr.New("workspace_unknown", "workspace %s does not exist", id)
	}
	for range 3 {
		first, _, err := m.capture(id)
		if err != nil {
			return oplog.Entry{}, "", err
		}
		second, files, err := m.capture(id)
		if err != nil {
			return oplog.Entry{}, "", err
		}
		if first != second {
			continue
		}
		if changed, err := m.manifestsChanged(head.Base, files); err != nil {
			return oplog.Entry{}, "", err
		} else if changed {
			if err := m.detach(id, ""); err != nil {
				return oplog.Entry{}, "", err
			}
		}
		e, h, _, err := l.Append(oplog.Entry{Kind: oplog.Propose, Tree: second, Base: head.Base, Trigger: "propose", Session: session}, true)
		return e, h, err
	}
	return oplog.Entry{}, "", apierr.New("concurrent_edit", "files changed during proposal capture; retry when writes settle")
}

// IgnoredFiles lists the ignored files of a workspace, which a snapshot does
// not capture. Dependency directories are skipped, linked or detached: they
// are rebuilt from their manifests. Removing a workspace that has any other
// ignored file would lose it.
func (m *Manager) IgnoredFiles(id string) ([]string, error) {
	root := m.Path(id)
	rules := m.rules(root)
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		if d.IsDir() && slices.Contains(m.cfg.DepDirs, d.Name()) {
			return filepath.SkipDir
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		if d.IsDir() || m.isTracked(id, rel) {
			return nil
		}
		ignored, err := rules.Ignored(rel, false)
		if ignored {
			out = append(out, rel)
		}
		return err
	})
	return out, err
}

// capture scans the workspace and makes its tree durable.
func (m *Manager) capture(id string) (string, []store.File, error) {
	root := m.Path(id)
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		return "", nil, apierr.New("workspace_unavailable", "workspace %s is missing", id)
	}
	prev, err := loadCache(m.cachePath(id))
	if err != nil {
		return "", nil, err
	}
	tracked := map[string]bool{}
	for p := range prev.Files {
		tracked[p] = true
	}
	b := m.cfg.Store.Batch()
	res, err := scan(root, m.rules(root), tracked, m.depPath(""), prev, b, m.cfg.Workers)
	if err != nil {
		return "", nil, err
	}
	tree, err := b.BuildTree(res.files)
	if err != nil {
		return "", nil, err
	}
	if err := b.Commit(); err != nil {
		return "", nil, err
	}
	if err := saveCache(m.cachePath(id), res.cache); err != nil {
		return "", nil, err
	}
	m.setTracked(id, res.cache)
	return tree, res.files, nil
}

// Remove deletes the workspace checkout and cache, and the shared
// dependency copies nothing links to any more; its log is kept.
func (m *Manager) Remove(id string) error {
	if !validWorkspace(id) {
		return apierr.New("invalid_workspace", "invalid workspace id %q", id)
	}
	if err := m.remove(id); err != nil {
		return err
	}
	return m.PruneDeps()
}

func (m *Manager) remove(id string) error {
	defer m.lock(id)()
	if err := os.RemoveAll(m.Path(id)); err != nil {
		return err
	}
	return os.RemoveAll(m.cachePath(id))
}

// Import stores the files of dir that are not ignored and returns their
// tree. It is how a base tree is made from a plain directory.
func (m *Manager) Import(dir string) (string, error) {
	b := m.cfg.Store.Batch()
	res, err := scan(dir, m.rules(dir), nil, "", statCache{Files: map[string]cached{}}, b, m.cfg.Workers)
	if err != nil {
		return "", err
	}
	tree, err := b.BuildTree(res.files)
	if err != nil {
		return "", err
	}
	return tree, b.Commit()
}

// Checkout clones base tree to dst, a new path on the runtime filesystem,
// for a check to run in. It has no operation log and shares no dependency
// trees; TrackedTree reads it back.
func (m *Manager) Checkout(tree, dst string) error {
	if err := m.ensureBase(tree); err != nil {
		return err
	}
	return cloneTree(m.basePath(tree), dst)
}

// TrackedTree returns the tree of the files of tree as they now are in dir,
// a checkout of it: changed, deleted or retyped tracked files count, and
// new files do not, as with git add -u.
func (m *Manager) TrackedTree(dir, tree string) (string, error) {
	files, err := m.cfg.Store.Files(tree)
	if err != nil {
		return "", err
	}
	tracked := make(map[string]bool, len(files))
	for _, f := range files {
		tracked[f.Path] = true
	}
	b := m.cfg.Store.Batch()
	res, err := scan(dir, m.rules(dir), tracked, "", statCache{Files: map[string]cached{}}, b, m.cfg.Workers)
	if err != nil {
		return "", err
	}
	kept := slices.DeleteFunc(res.files, func(f store.File) bool { return !tracked[f.Path] })
	got, err := b.BuildTree(kept)
	if err != nil {
		return "", err
	}
	return got, b.Commit()
}
