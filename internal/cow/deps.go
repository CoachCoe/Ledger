package cow

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/store"
)

// manifests returns the dependency manifests and lock files among files,
// by NFC path.
func manifests(files []store.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		if capability.Dependency(f.Path) {
			out[f.Path] = f.ID
		}
	}
	return out
}

// depKey names one shared dependency tree: the directory it serves and
// every manifest of the tree that produced it.
func depKey(rel string, ms map[string]string) (string, error) {
	type manifest struct {
		Path string `json:"path"`
		ID   string `json:"id"`
	}
	list := []manifest{}
	for p, id := range ms {
		list = append(list, manifest{p, id})
	}
	slices.SortFunc(list, func(a, b manifest) int { return strings.Compare(a.Path, b.Path) })
	raw, err := canon.Marshal(struct {
		Dir       string     `json:"dir"`
		Manifests []manifest `json:"manifests"`
	}{rel, list})
	if err != nil {
		return "", err
	}
	return canon.Hash(raw), nil
}

// depDirs lists the dependency directories a tree can have: each
// configured name beside each manifest.
func (m *Manager) depDirs(ms map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for p := range ms {
		for _, name := range m.cfg.DepDirs {
			rel := path.Join(path.Dir(p), name)
			if !seen[rel] {
				seen[rel] = true
				out = append(out, rel)
			}
		}
	}
	slices.Sort(out)
	return out
}

func (m *Manager) depPath(key string) string { return filepath.Join(m.cfg.Runtime, "deps", key) }

// humanMatches reports whether the default workspace has exactly the
// manifests ms, so its installed dependencies are the ones they produce.
func (m *Manager) humanMatches(ms map[string]string) (bool, error) {
	for p, id := range ms {
		data, err := os.ReadFile(filepath.Join(m.cfg.Human, filepath.FromSlash(p)))
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		got, err := store.FileID(data)
		if err != nil {
			return false, err
		}
		if got != id {
			return false, nil
		}
	}
	return true, nil
}

// shared returns the template a workspace's dependency directory rel is
// cloned from for key, capturing the read-only shared copy from the
// default workspace on first use. It returns "" when there is nothing to
// share. The caller holds the key's lock.
func (m *Manager) shared(rel, key string, ms map[string]string) (string, error) {
	template := filepath.Join(m.depPath(key), "links")
	if _, err := os.Lstat(template); err == nil {
		return template, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	target, err := m.captureDeps(rel, key, ms)
	if err != nil || target == "" {
		return "", err
	}
	return template, m.buildTemplate(target, template)
}

// captureDeps returns the read-only shared copy for key, cloning it from the
// default workspace if needed.
func (m *Manager) captureDeps(rel, key string, ms map[string]string) (string, error) {
	// Keep the directory's own name: Node resolves a package's siblings by
	// looking for a node_modules directory above its real path.
	dst := filepath.Join(m.depPath(key), path.Base(rel))
	if _, err := os.Lstat(dst); err == nil {
		return dst, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if m.cfg.Human == "" {
		return "", nil
	}
	src := filepath.Join(m.cfg.Human, filepath.FromSlash(rel))
	if info, err := os.Lstat(src); err != nil || !info.IsDir() {
		return "", nil //nolint:nilerr // no installed dependencies to share
	}
	if ok, err := m.humanMatches(ms); err != nil || !ok {
		return "", err
	}
	tmp, err := m.tmpPath("deps")
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return "", err
	}
	if err := cloneTree(src, filepath.Join(tmp, path.Base(rel))); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := setWritable(filepath.Join(tmp, path.Base(rel)), false); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, m.depPath(key)); err != nil {
		return "", err
	}
	return dst, nil
}

// sharedMarker, inside a workspace's dependency directory, names the
// shared copy its package links point into.
const sharedMarker = ".ledger-shared"

// linkDeps gives workspace id each shared dependency tree for the
// manifests ms of its base. The
// dependency directory itself is private and writable, because tools such
// as Vite write caches and temporary files directly inside node_modules;
// each package in it is a link into the read-only shared copy, and hidden
// entries other than .bin are private clones.
func (m *Manager) linkDeps(id string, ms map[string]string) error {
	for _, rel := range m.depDirs(ms) {
		key, err := depKey(rel, ms)
		if err != nil {
			return err
		}
		if err := m.linkDep(id, rel, key, ms); err != nil {
			return err
		}
	}
	return nil
}

// linkDep links one dependency directory. It holds the key's lock until
// the link is in place, so pruning never removes a copy being linked.
func (m *Manager) linkDep(id, rel, key string, ms map[string]string) error {
	defer m.lock("deps:" + key)()
	template, err := m.shared(rel, key, ms)
	if err != nil || template == "" {
		return err
	}
	dir := filepath.Join(m.Path(id), filepath.FromSlash(rel))
	if _, err := os.Lstat(dir); err == nil {
		return nil // tracked content or an earlier link already there
	}
	if _, err := os.Lstat(filepath.Dir(dir)); err != nil {
		return nil //nolint:nilerr // the tree has no directory to link into
	}
	tmp, err := m.tmpPath("link")
	if err != nil {
		return err
	}
	if err := cloneTree(template, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return os.Rename(tmp, dir)
}

// referenced returns the keys of the shared copies some workspace still
// links to.
func (m *Manager) referenced() (map[string]bool, error) {
	entries, err := os.ReadDir(filepath.Join(m.cfg.Runtime, "workspaces"))
	if err != nil {
		return nil, err
	}
	deps := m.depPath("")
	out := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || !validWorkspace(e.Name()) {
			continue
		}
		links, err := m.linked(e.Name())
		if err != nil {
			return nil, err
		}
		for _, target := range links {
			if rel, err := filepath.Rel(deps, target); err == nil {
				out[strings.Split(filepath.ToSlash(rel), "/")[0]] = true
			}
		}
	}
	return out, nil
}

// PruneDeps removes the shared dependency copies no workspace links to.
// They are read-only, so a plain recursive delete cannot remove them.
func (m *Manager) PruneDeps() error {
	entries, err := os.ReadDir(m.depPath(""))
	if err != nil {
		return err
	}
	used, err := m.referenced()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !used[e.Name()] {
			if err := m.pruneDep(e.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

// pruneDep removes the copy for key unless a workspace linked it after
// PruneDeps looked.
func (m *Manager) pruneDep(key string) error {
	defer m.lock("deps:" + key)()
	used, err := m.referenced()
	if err != nil || used[key] {
		return err
	}
	dir := m.depPath(key)
	if err := setWritable(dir, true); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return os.RemoveAll(dir)
}

// buildTemplate writes, at dst, the dependency directory each workspace
// clones: a link per package of the shared copy target, private copies of
// its hidden entries, and the marker. Cloning one prepared directory is a
// single call on APFS instead of a symlink per package.
func (m *Manager) buildTemplate(target, dst string) error {
	tmp, err := m.tmpPath("link")
	if err != nil {
		return err
	}
	if err := os.Mkdir(tmp, 0o755); err != nil { //nolint:gosec // a checkout gets the modes Git gives one
		return err
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return err
	}
	for _, e := range entries {
		src, dst := filepath.Join(target, e.Name()), filepath.Join(tmp, e.Name())
		if !strings.HasPrefix(e.Name(), ".") || e.Name() == ".bin" {
			err = os.Symlink(src, dst)
		} else {
			err = cloneTree(src, dst)
			if err == nil {
				err = setWritable(dst, true)
			}
		}
		if err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, sharedMarker), []byte(target+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// linked returns the workspace's dependency directories whose packages
// still link to a shared copy, with that copy.
func (m *Manager) linked(id string) (map[string]string, error) {
	root := m.Path(id)
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" || d.Name() == ".ledger" {
			return filepath.SkipDir
		}
		if !slices.Contains(m.cfg.DepDirs, d.Name()) {
			return nil
		}
		if raw, err := os.ReadFile(filepath.Join(p, sharedMarker)); err == nil {
			out[p] = strings.TrimSuffix(string(raw), "\n")
		}
		return filepath.SkipDir
	})
	return out, err
}

// manifestsChanged reports whether files' dependency manifests differ
// from those of base.
func (m *Manager) manifestsChanged(base string, files []store.File) (bool, error) {
	baseFiles, err := m.cfg.Store.Files(base)
	if err != nil {
		return false, err
	}
	normalized := make([]store.File, len(files))
	for i, f := range files {
		normalized[i] = store.File{Path: nfc(f.Path), ID: f.ID}
	}
	a, b := manifests(baseFiles), manifests(normalized)
	if len(a) != len(b) {
		return true, nil
	}
	for p, id := range a {
		if b[p] != id {
			return true, nil
		}
	}
	return false, nil
}

// Detach replaces the workspace's links to shared dependency trees with
// private, writable clones, so installing into them changes no other
// workspace. rel names one dependency directory; empty means all.
func (m *Manager) Detach(id, rel string) error {
	if err := m.detachLocked(id, rel); err != nil {
		return err
	}
	return m.PruneDeps()
}

func (m *Manager) detachLocked(id, rel string) error {
	defer m.lock(id)()
	return m.detach(id, rel)
}

func (m *Manager) detach(id, rel string) error {
	links, err := m.linked(id)
	if err != nil {
		return err
	}
	for dir, target := range links {
		if rel != "" && dir != filepath.Join(m.Path(id), filepath.FromSlash(rel)) {
			continue
		}
		tmp, err := m.tmpPath("detach")
		if err != nil {
			return err
		}
		if err := cloneTree(target, tmp); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
		if err := setWritable(tmp, true); err != nil {
			return err
		}
		// Keep what tools already wrote into the private directory.
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Name() == sharedMarker || e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			if err := os.RemoveAll(filepath.Join(tmp, e.Name())); err != nil {
				return err
			}
			if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(tmp, e.Name())); err != nil {
				return err
			}
		}
		aside, err := m.tmpPath("detached")
		if err != nil {
			return err
		}
		if err := os.Rename(dir, aside); err != nil {
			return err
		}
		if err := os.Rename(tmp, dir); err != nil {
			return apierr.New("deps_detach_failed", "could not replace %s with a private copy: %v", dir, err)
		}
		if err := os.RemoveAll(aside); err != nil {
			return err
		}
	}
	return nil
}

// setWritable adds or removes write permission on every file and
// directory under dir.
func setWritable(dir string, writable bool) error {
	if info, err := os.Lstat(dir); err != nil {
		return err
	} else if info.Mode()&fs.ModeSymlink != 0 {
		return nil
	} else if !info.IsDir() {
		mode := info.Mode().Perm()
		if writable {
			return os.Chmod(dir, mode|0o200)
		}
		return os.Chmod(dir, mode&^0o222)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		next := mode &^ 0o222
		if writable {
			next = mode | 0o200
		}
		if next == mode {
			return nil
		}
		return root.Chmod(p, next)
	})
}
