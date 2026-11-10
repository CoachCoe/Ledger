package store

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
)

// Entry types in a tree.
const (
	TypeFile       = "file"
	TypeExecutable = "executable"
	TypeSymlink    = "symlink"
	TypeTree       = "tree"
	// TypeGitlink names a submodule commit by its Git id; the commit is not
	// a store object.
	TypeGitlink = "gitlink"
)

type Entry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	ID   string `json:"id"`
}

type Tree struct {
	Entries []Entry `json:"entries"`
}

// key is the name under which two entries collide: the NFC form, case
// folded when the repository is case-insensitive.
func (s *Store) key(name string) string {
	n := norm.NFC.String(name)
	if !s.sensitive {
		n = cases.Fold().String(n)
	}
	return n
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && utf8.ValidString(name) &&
		!strings.ContainsAny(name, "/\x00") && norm.NFC.IsNormalString(name)
}

// WriteTree validates entries, sorts them by UTF-8 bytes and stores the
// tree. Names must be NFC and must not collide under the case policy.
func (b *Batch) WriteTree(entries []Entry) (string, error) {
	es := slices.Clone(entries)
	seen := map[string]string{}
	for _, e := range es {
		if !validName(e.Name) {
			return "", apierr.New("invalid_path", "invalid tree entry name %q", e.Name).With("name", e.Name)
		}
		switch e.Type {
		case TypeFile, TypeExecutable, TypeSymlink, TypeTree:
			if !validID(e.ID) {
				return "", apierr.New("invalid_object_id", "entry %q has invalid id %q", e.Name, e.ID)
			}
		case TypeGitlink:
			if len(e.ID) != 40 && len(e.ID) != 64 || strings.Trim(e.ID, "0123456789abcdef") != "" {
				return "", apierr.New("invalid_object_id", "gitlink %q has invalid id %q", e.Name, e.ID)
			}
		default:
			return "", apierr.New("invalid_entry_type", "entry %q has unknown type %q", e.Name, e.Type)
		}
		k := b.s.key(e.Name)
		if other, ok := seen[k]; ok {
			return "", apierr.New("path_collision", "%q and %q collide", other, e.Name).With("paths", []string{other, e.Name})
		}
		seen[k] = e.Name
	}
	slices.SortFunc(es, func(x, y Entry) int { return strings.Compare(x.Name, y.Name) })
	if es == nil {
		es = []Entry{}
	}
	raw, err := canon.Marshal(Tree{Entries: es})
	if err != nil {
		return "", err
	}
	return b.Put(KindTree, raw)
}

// ReadTree returns the entries of tree id.
func (s *Store) ReadTree(id string) (Tree, error) {
	data, err := s.getKind(id, KindTree)
	if err != nil {
		return Tree{}, err
	}
	var t Tree
	if err := json.Unmarshal(data, &t); err != nil {
		return Tree{}, apierr.New("object_corrupt", "tree %s is malformed", id).With("object", id)
	}
	return t, nil
}

// File is one path of a flat listing for BuildTree.
type File struct {
	Path string // slash-separated, relative
	Type string // file, executable, symlink or gitlink
	ID   string
}

// BuildTree writes the trees for a flat listing and returns the root id.
// Paths are stored NFC-normalized; two paths that normalize to the same
// name, or collide under the case policy, are rejected, as is a path that
// is both a file and a directory.
func (b *Batch) BuildTree(files []File) (string, error) {
	type dir struct {
		entries []Entry
		names   map[string]string // collision key → path as given
	}
	dirs := map[string]*dir{"": {names: map[string]string{}}}
	get := func(d string) *dir {
		if dirs[d] == nil {
			dirs[d] = &dir{names: map[string]string{}}
		}
		return dirs[d]
	}
	claim := func(d, name, given string) error {
		k := b.s.key(name)
		if other, ok := get(d).names[k]; ok && other != given {
			return apierr.New("path_collision", "%q and %q collide", other, given).With("paths", []string{other, given})
		}
		get(d).names[k] = given
		return nil
	}
	for _, f := range files {
		if f.Type == TypeTree {
			return "", apierr.New("invalid_entry_type", "listing entry %q must not be a tree", f.Path)
		}
		given := strings.Split(f.Path, "/")
		segs := make([]string, len(given))
		for i, seg := range given {
			segs[i] = norm.NFC.String(seg)
			if !validName(segs[i]) {
				return "", apierr.New("invalid_path", "invalid path %q", f.Path).With("path", f.Path)
			}
		}
		for i := range len(segs) - 1 {
			parent := strings.Join(segs[:i], "/")
			if err := claim(parent, segs[i], strings.Join(given[:i+1], "/")+"/"); err != nil {
				return "", err
			}
		}
		parent := strings.Join(segs[:len(segs)-1], "/")
		name := segs[len(segs)-1]
		if err := claim(parent, name, f.Path); err != nil {
			return "", err
		}
		get(parent).entries = append(get(parent).entries, Entry{Name: name, Type: f.Type, ID: f.ID})
	}
	order := make([]string, 0, len(dirs))
	for d := range dirs {
		order = append(order, d)
	}
	depth := func(d string) int {
		if d == "" {
			return 0
		}
		return strings.Count(d, "/") + 1
	}
	// Deepest first, so every subtree id exists before its parent is encoded.
	slices.SortFunc(order, func(x, y string) int { return depth(y) - depth(x) })
	var root string
	for _, d := range order {
		id, err := b.WriteTree(dirs[d].entries)
		if err != nil {
			return "", err
		}
		if d == "" {
			root = id
			continue
		}
		parent, name := path.Split(d)
		parent = strings.TrimSuffix(parent, "/")
		dirs[parent].entries = append(dirs[parent].entries, Entry{Name: name, Type: TypeTree, ID: id})
	}
	return root, nil
}

// Materialize writes tree id into dst, which must not exist.
func (s *Store) Materialize(id, dst string) error {
	t, err := s.ReadTree(id)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o755); err != nil { //nolint:gosec // a checkout gets the modes Git gives one
		return err
	}
	for _, e := range t.Entries {
		p := filepath.Join(dst, e.Name)
		switch e.Type {
		case TypeTree:
			if err := s.Materialize(e.ID, p); err != nil {
				return err
			}
		case TypeSymlink:
			target, err := s.getKind(e.ID, KindBlob)
			if err != nil {
				return err
			}
			if err := os.Symlink(string(target), p); err != nil {
				return err
			}
		case TypeFile, TypeExecutable:
			mode := fs.FileMode(0o644)
			if e.Type == TypeExecutable {
				mode = 0o755
			}
			if err := s.materializeFile(e.ID, p, mode); err != nil {
				return err
			}
		default:
			return apierr.New("unsupported_submodule", "submodule %s cannot be materialized", path.Join(dst, e.Name))
		}
	}
	return nil
}

func (s *Store) materializeFile(id, p string, mode fs.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	err = s.WriteFile(id, f)
	if e := f.Close(); err == nil {
		err = e
	}
	return err
}
