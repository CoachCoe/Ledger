package store

import (
	"path"
	"slices"
	"strings"
)

// Change is one path that differs between two trees. Status is A (added),
// D (deleted), M (content changed) or T (type changed), as Git reports
// them; Old or New is the zero File for an addition or deletion.
type Change struct {
	Path   string
	Status string
	Old    File
	New    File
}

// Diff lists the paths that differ from tree a to tree b, sorted by path.
// Subtrees with equal ids are skipped without being read. A path that is
// a directory on one side and a file on the other reports the file's
// deletion or addition and every file beneath the directory.
func (s *Store) Diff(a, b string) ([]Change, error) {
	var out []Change
	var walk func(a, b, prefix string) error
	all := func(id, prefix, status string) error {
		files, err := s.Files(id)
		if err != nil {
			return err
		}
		for _, f := range files {
			f.Path = path.Join(prefix, f.Path)
			c := Change{Path: f.Path, Status: status}
			if status == "A" {
				c.New = f
			} else {
				c.Old = f
			}
			out = append(out, c)
		}
		return nil
	}
	walk = func(a, b, prefix string) error {
		if a == b {
			return nil
		}
		var ta, tb Tree
		var err error
		if a != "" {
			if ta, err = s.ReadTree(a); err != nil {
				return err
			}
		}
		if b != "" {
			if tb, err = s.ReadTree(b); err != nil {
				return err
			}
		}
		ea, eb := index(ta), index(tb)
		names := make([]string, 0, len(ea)+len(eb))
		for n := range ea {
			names = append(names, n)
		}
		for n := range eb {
			if _, ok := ea[n]; !ok {
				names = append(names, n)
			}
		}
		slices.Sort(names)
		for _, n := range names {
			x, inA := ea[n]
			y, inB := eb[n]
			p := path.Join(prefix, n)
			switch {
			case inA && inB && x == y:
			case inA && inB && x.Type == TypeTree && y.Type == TypeTree:
				if err := walk(x.ID, y.ID, p); err != nil {
					return err
				}
			case inA && inB && x.Type != TypeTree && y.Type != TypeTree:
				status := "M"
				if x.Type != y.Type {
					status = "T"
				}
				out = append(out, Change{Path: p, Status: status, Old: File{p, x.Type, x.ID}, New: File{p, y.Type, y.ID}})
			default:
				if inA {
					if x.Type == TypeTree {
						if err := all(x.ID, p, "D"); err != nil {
							return err
						}
					} else {
						out = append(out, Change{Path: p, Status: "D", Old: File{p, x.Type, x.ID}})
					}
				}
				if inB {
					if y.Type == TypeTree {
						if err := all(y.ID, p, "A"); err != nil {
							return err
						}
					} else {
						out = append(out, Change{Path: p, Status: "A", New: File{p, y.Type, y.ID}})
					}
				}
			}
		}
		return nil
	}
	if err := walk(a, b, ""); err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(x, y Change) int { return strings.Compare(x.Path, y.Path) })
	return out, nil
}

func index(t Tree) map[string]Entry {
	m := make(map[string]Entry, len(t.Entries))
	for _, e := range t.Entries {
		m[e.Name] = e
	}
	return m
}
