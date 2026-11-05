package record

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/gitx"
)

const maxRefAttempts = 5

func (s *Store) refIndexFile() string { return filepath.Join(s.Dir, "runtime", "records.index") }

// refTipFile caches the records ref commit that refIndexFile matches.
func (s *Store) refTipFile() string { return filepath.Join(s.Dir, "runtime", "records.tip") }

// appendToRef is the fast path for publishing new objects: when the ref is
// still at the commit the private index matches, only the new objects are
// added. It reports false whenever that does not hold, and syncRef must
// then reconcile in full.
func (s *Store) appendToRef(ctx context.Context, hashes []string) (previous string, applied bool, err error) {
	raw, err := os.ReadFile(s.refTipFile())
	tip := strings.TrimSpace(string(raw))
	if err != nil || tip == "" {
		return "", false, nil
	}
	paths := make([]string, len(hashes))
	for i, h := range hashes {
		paths[i] = s.ObjectPath(h)
	}
	blobs, err := s.Repo.WriteBlobs(ctx, paths)
	if err != nil {
		return "", false, err
	}
	entries := make([]gitx.TreeEntry, len(hashes))
	for i, h := range hashes {
		entries[i] = gitx.TreeEntry{Path: refPath(h), Blob: blobs[i]}
	}
	tree, err := s.Repo.BuildTree(ctx, s.refIndexFile(), tip, entries)
	if err != nil {
		if cached, e := s.Repo.ResolveRef(ctx, tip); e == nil && cached == "" {
			return "", false, nil
		}
		return "", false, err
	}
	commit, err := s.Repo.CommitTree(ctx, tree, []string{tip}, "ledger records\n")
	if err != nil {
		return "", false, err
	}
	if err := s.fault(StageRefCAS); err != nil {
		return "", false, err
	}
	if err := s.Repo.UpdateRefCAS(ctx, RecordsRef, commit, tip); err != nil {
		_ = os.Remove(s.refTipFile())
		if apierr.HasCode(err, "ref_moved") {
			return tip, false, nil
		}
		return "", false, err
	}
	return tip, true, os.WriteFile(s.refTipFile(), []byte(commit+"\n"), 0600)
}

// refSync reports what syncRef changed.
type refSync struct {
	// Materialized lists objects copied from the ref to disk.
	Materialized []string
	// Added counts disk objects added to the ref.
	Added int
	// Invalid lists ref entries whose content does not match their name.
	Invalid []string
}

// syncRef makes refs/ledger/records and .ledger/objects hold the same
// objects: ref objects missing on disk are verified and materialized, and
// disk objects missing from the ref are committed onto it with
// compare-and-swap. If the ref moves concurrently, the new commit keeps
// both tips as parents; an external update is never overwritten.
func (s *Store) syncRef(ctx context.Context, previous ...string) (*refSync, error) {
	out := &refSync{}
	expected, err := s.Repo.ResolveRef(ctx, RecordsRef)
	if err != nil {
		return nil, err
	}
	tip := expected
	if len(previous) > 0 && previous[0] != "" {
		expected = previous[0]
	}
	for attempt := 0; ; attempt++ {
		inRef, err := s.materialize(ctx, tip, out)
		if err != nil {
			return nil, err
		}
		onDisk, err := s.listObjects()
		if err != nil {
			return nil, err
		}
		var missing []string
		for _, h := range onDisk {
			if inRef[h] {
				continue
			}
			// Only verified content may reach the shared ref.
			if ok, err := s.verifyFile(h); err != nil {
				return nil, err
			} else if ok {
				missing = append(missing, h)
			}
		}
		if len(missing) == 0 && (expected == tip || expected == "") {
			// The private index may not match the ref; rebuild it next time.
			if err := os.Remove(s.refTipFile()); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			return out, nil
		}
		paths := make([]string, len(missing))
		for i, h := range missing {
			paths[i] = s.ObjectPath(h)
		}
		blobs, err := s.Repo.WriteBlobs(ctx, paths)
		if err != nil {
			return nil, err
		}
		entries := make([]gitx.TreeEntry, len(missing))
		for i, h := range missing {
			entries[i] = gitx.TreeEntry{Path: refPath(h), Blob: blobs[i]}
		}
		tree, err := s.Repo.BuildTree(ctx, s.refIndexFile(), tip, entries)
		if err != nil {
			return nil, err
		}
		parents := refParents(expected, tip)
		commit, err := s.Repo.CommitTree(ctx, tree, parents, "ledger records\n")
		if err != nil {
			return nil, err
		}
		if err := s.fault(StageRefCAS); err != nil {
			return nil, err
		}
		err = s.Repo.UpdateRefCAS(ctx, RecordsRef, commit, tip)
		if err == nil {
			out.Added = len(missing)
			return out, os.WriteFile(s.refTipFile(), []byte(commit+"\n"), 0o600)
		}
		if !apierr.HasCode(err, "ref_moved") || attempt+1 == maxRefAttempts {
			return nil, err
		}
		if tip, err = s.Repo.ResolveRef(ctx, RecordsRef); err != nil {
			return nil, err
		}
	}
}

// parents returns the parents for a records commit built on tip. When the
// ref moved away from the expected tip to one that does not contain it,
// both are kept so neither history is lost.
func refParents(expected, tip string) []string {
	if expected == "" {
		if tip == "" {
			return nil
		}
		return []string{tip}
	}
	if tip == "" || expected == tip {
		return []string{expected}
	}
	return []string{expected, tip}
}

// materialize copies verified ref objects that are missing on disk and
// returns the set of hashes present in the ref.
func (s *Store) materialize(ctx context.Context, tip string, out *refSync) (map[string]bool, error) {
	inRef := map[string]bool{}
	if tip == "" {
		return inRef, nil
	}
	entries, err := s.Repo.ListTree(ctx, tip)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		h, ok := hashFromRefPath(e.Path)
		if !ok {
			out.Invalid = appendNew(out.Invalid, e.Path)
			continue
		}
		inRef[h] = true
		if _, err := os.Stat(s.ObjectPath(h)); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		raw, err := s.Repo.ReadBlob(ctx, e.Blob)
		if err != nil {
			return nil, err
		}
		o, err := envelope.ParseStored(raw, h)
		if err != nil {
			out.Invalid = appendNew(out.Invalid, h)
			continue
		}
		if err := s.writeObject(o); err != nil {
			return nil, err
		}
		out.Materialized = append(out.Materialized, h)
	}
	return inRef, nil
}

func appendNew(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

func refPath(hash string) string { return hash[:2] + "/" + hash[2:] + ".json" }

func hashFromRefPath(p string) (string, bool) {
	dir, file, ok := strings.Cut(p, "/")
	h := dir + strings.TrimSuffix(file, ".json")
	return h, ok && len(dir) == 2 && strings.HasSuffix(file, ".json") && isHash(h)
}

// listObjects returns the hashes of all well-named object files on disk.
func (s *Store) listObjects() ([]string, error) {
	var out []string
	root := filepath.Join(s.Dir, "objects")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if h, ok := hashFromRefPath(filepath.ToSlash(rel)); ok {
			out = append(out, h)
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}
