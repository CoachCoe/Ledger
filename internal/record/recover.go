package record

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Report describes what recovery found and changed.
type Report struct {
	CleanupPending []string `json:"cleanup_pending,omitempty"`
	// Quarantined lists files moved to .ledger/quarantine because their
	// content does not match their name.
	Quarantined []string `json:"quarantined"`
	// Materialized counts ref objects copied to disk.
	Materialized int `json:"materialized"`
	// AddedToRef counts disk objects committed to the records ref.
	AddedToRef int `json:"added_to_ref"`
	// InvalidInRef lists ref entries that failed verification.
	InvalidInRef []string `json:"invalid_in_ref"`
	// Indexed counts groups newly indexed.
	Indexed int `json:"indexed"`
	// Unverifiable lists records that are intact but cannot be accepted:
	// unsupported, malformed, or with failing signatures.
	Unverifiable []Problem `json:"unverifiable"`
	// Pending lists records that belong to no committed group.
	Pending []string `json:"pending"`
	// Conflicts lists subjects and operations with conflicting histories.
	Conflicts []Conflict `json:"conflicts"`
	// DiscardedRuntime counts runtime rows dropped by reindex.
	DiscardedRuntime map[string]int `json:"discarded_runtime,omitempty"`
}

// Problem is a record that could not be accepted.
type Problem struct {
	Hash  string `json:"hash"`
	Code  string `json:"code"`
	Error string `json:"error"`
}

// Conflict is a subject or operation whose records disagree.
type Conflict struct {
	ID      string       `json:"id"`
	Kind    string       `json:"kind"`
	Detail  index.Detail `json:"detail,omitzero"`
	Records []string     `json:"records,omitempty"`
}

// Recover reconciles the record files, the records ref, and the index:
// it finishes journaled operations, quarantines corrupt files, copies
// objects in both directions between disk and ref, and indexes every
// complete group. It never deletes records or overwrites external ref
// changes.
func (s *Store) Recover(ctx context.Context) (*Report, error) {
	unlock, err := lockFile(filepath.Join(s.Dir, "lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.recoverLocked(ctx)
}

// Reindex deletes the index and rebuilds it from the record files. Runtime
// state (notifications, leases, observations) cannot be rebuilt and is
// reported as discarded.
func (s *Store) Reindex(ctx context.Context) (*Report, error) {
	unlock, err := lockFile(filepath.Join(s.Dir, "lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	discarded, err := s.Index.RuntimeRows()
	if err != nil {
		return nil, err
	}
	if err := s.Index.Reset(ctx); err != nil {
		return nil, err
	}

	r, err := s.recoverLocked(ctx)
	if err != nil {
		return nil, err
	}
	r.DiscardedRuntime = discarded
	return r, nil
}

func (s *Store) recoverLocked(ctx context.Context) (*Report, error) {
	if err := s.Index.Refresh(); err != nil {
		return nil, err
	}
	if err := s.completeJournals(ctx); err != nil {
		return nil, err
	}
	r := &Report{}
	journals, err := filepath.Glob(filepath.Join(s.Dir, "journal", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range journals {
		hash := strings.TrimSuffix(filepath.Base(path), ".json")
		if done, err := s.Index.HasReceipt(ctx, hash); err != nil {
			return nil, err
		} else if done {
			r.CleanupPending = append(r.CleanupPending, hash)
		}
	}
	hashes, err := s.listObjects()
	if err != nil {
		return nil, err
	}
	for _, h := range hashes {
		ok, err := s.verifyFile(h)
		if err != nil {
			return nil, err
		}
		if !ok {
			r.Quarantined = append(r.Quarantined, h)
		}
	}
	sync, err := s.syncRef(ctx)
	if err != nil {
		return nil, err
	}
	r.Materialized, r.AddedToRef, r.InvalidInRef = len(sync.Materialized), sync.Added, sync.Invalid
	if err := s.indexAll(ctx, r); err != nil {
		return nil, err
	}
	return r, s.conflicts(ctx, r)
}

// verifyFile reports whether the object file for h hashes to h, moving it
// to quarantine if not.
func (s *Store) verifyFile(h string) (bool, error) {
	path := s.ObjectPath(h)
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if canon.Hash(raw) == h {
		return true, nil
	}
	return false, s.quarantine(path, h)
}

func (s *Store) quarantine(path, hash string) error {
	dir := filepath.Join(s.Dir, "quarantine")
	for i := 0; ; i++ {
		dst := filepath.Join(dir, fmt.Sprintf("%s.%d.json", hash, i))
		if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			if err := os.Rename(path, dst); err != nil {
				return err
			}
			return syncDir(dir)
		} else if err != nil {
			return err
		}
	}
}

// indexAll indexes every complete, verifiable group not yet indexed. A
// group whose signing keys come from another group waits for it, so the
// result does not depend on the order files are read in.
func (s *Store) indexAll(ctx context.Context, r *Report) error {
	hashes, err := s.listObjects()
	if err != nil {
		return err
	}
	objects := map[string]envelope.Object{}
	var receipts []string
	for _, h := range hashes {
		o, err := s.Read(h)
		if err != nil {
			e := apierr.As(err)
			r.Unverifiable = append(r.Unverifiable, Problem{Hash: h, Code: e.Code, Error: e.Message})
			continue
		}
		objects[h] = o
		if o.Envelope.Kind == schema.KindOperationReceipt {
			receipts = append(receipts, h)
		}
	}
	pending := map[string]index.Group{}
	for _, h := range receipts {
		if done, err := s.Index.HasReceipt(ctx, h); err != nil {
			return err
		} else if done {
			continue
		}
		var rc schema.OperationReceipt
		if err := objects[h].Decode(&rc); err != nil {
			r.Unverifiable = append(r.Unverifiable, Problem{Hash: h, Code: "record_malformed", Error: err.Error()})
			continue
		}
		g := index.Group{Receipt: objects[h]}
		complete := true
		for _, m := range rc.Members {
			o, ok := objects[m]
			complete = complete && ok
			g.Members = append(g.Members, o)
		}
		if complete {
			pending[h] = g
		}
	}
	for progress := true; progress; {
		progress = false
		for _, h := range slices.Sorted(maps.Keys(pending)) {
			err := s.Index.Verify(ctx, pending[h])
			if apierr.HasCode(err, index.CodeUnavailable) {
				continue
			}
			if err != nil {
				e := apierr.As(err)
				r.Unverifiable = append(r.Unverifiable, Problem{Hash: h, Code: e.Code, Error: e.Message})
			} else if err := s.Index.Apply(ctx, pending[h]); err != nil {
				return err
			} else {
				r.Indexed++
			}
			delete(pending, h)
			progress = true
		}
	}
	for _, h := range hashes {
		if _, ok := objects[h]; !ok {
			continue
		}
		committed, err := s.Index.Committed(ctx, h)
		if err != nil {
			return err
		}
		if !committed {
			r.Pending = append(r.Pending, h)
		}
	}
	return nil
}

func (s *Store) conflicts(ctx context.Context, r *Report) error {
	subjects, err := s.Index.Subjects(ctx)
	if err != nil {
		return err
	}
	for _, sub := range subjects {
		if sub.Conflict != "" {
			r.Conflicts = append(r.Conflicts, Conflict{ID: sub.ID, Kind: sub.Conflict, Detail: sub.Detail})
		}
	}
	ops, err := s.Index.ConflictingOperations(ctx)
	if err != nil {
		return err
	}
	for id, receipts := range ops {
		r.Conflicts = append(r.Conflicts, Conflict{ID: id, Kind: "operation_conflict", Records: receipts})
	}
	slices.SortFunc(r.Conflicts, func(a, b Conflict) int { return strings.Compare(a.ID, b.ID) })
	return nil
}
