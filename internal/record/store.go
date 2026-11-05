// Package record implements the RecordStore: content-addressed record
// files under .ledger/objects, their mirror on refs/ledger/records, and
// publication of operation groups with recovery (ADR 0009,
// docs/contracts/recovery.md).
package record

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/lock"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// RecordsRef mirrors .ledger/objects.
const RecordsRef = "refs/ledger/records"

// DirName is the Ledger directory at the top of the main working tree.
const DirName = ".ledger"

// Publication stages, in order. Options.Fault is called before each.
const (
	StageJournal = "journal"
	StageEffects = "effects"
	StageObjects = "objects"
	StageReceipt = "receipt"
	StageRef     = "ref"
	StageRefCAS  = "ref_cas"
	StageIndex   = "index"
	StageCleanup = "cleanup"
	StageDone    = "done"
)

// Options configures a Store.
type Options struct {
	Effect func(context.Context, Effect) error
	// Fault, if set, is called before each publication stage. Returning an
	// error stops publication there, as a crash would; tests use it to
	// inject failures at every boundary.
	Fault func(stage string) error
	// Now returns the current time; defaults to time.Now.
	Now func() time.Time
}

// Store is an open RecordStore.
type Store struct {
	Dir   string // the .ledger directory
	Repo  *gitx.Repo
	Index *index.Index
	opts  Options
}

// Open opens the RecordStore of repo, which must have been initialized.
// It does not run recovery; call Recover when a full reconciliation is
// wanted (daemon start, reindex).
func Open(ctx context.Context, repo *gitx.Repo, opts Options) (*Store, error) {
	dir := filepath.Join(repo.MainDir, DirName)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, apierr.New("not_initialized", "%s has no %s directory; run ledger init", repo.MainDir, DirName)
	}
	for _, sub := range []string{"objects", "quarantine", "journal", "runtime"} {
		if err := atomicfile.MakeDir(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	idx, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		return nil, err
	}
	return &Store{Dir: dir, Repo: repo, Index: idx, opts: opts}, nil
}

// lock takes the exclusive RecordStore lock, which the kernel releases if
// the holder dies.
func (s *Store) lock() (func(), error) { return lock.Acquire(filepath.Join(s.Dir, "lock"), false) }

// Close closes the index.
func (s *Store) Close() error { return s.Index.Close() }

func (s *Store) fault(stage string) error {
	if s.opts.Fault == nil {
		return nil
	}
	return s.opts.Fault(stage)
}

// Op identifies a mutating operation. Digest is the frozen canonical
// request digest; retrying the same ID with the same Digest returns the
// original result.
type Op struct {
	ID     string
	Name   string
	Digest string
	Signer identity.Signer
}

// Result is a committed operation's outcome.
type Result struct {
	OperationID string         `json:"operation_id"`
	Receipt     string         `json:"receipt"`
	Result      jsontext.Value `json:"result"`
	Members     []string       `json:"members"`
	// Replayed is true when an earlier commit of the same request was
	// returned instead of performing the operation again.
	Replayed bool `json:"replayed"`
}

// BuildFunc prepares an operation's records under the store lock. It may
// read the index, which reflects every committed group.
type BuildFunc func(idx *index.Index) (result any, members []envelope.Object, err error)

// Do performs an operation: under the RecordStore lock it builds the
// records, validates them against current state, and publishes them with
// a signed receipt. The receipt is the visibility boundary: a crash at any
// point leaves either no accepted change or a group that recovery
// completes.
func (s *Store) Do(ctx context.Context, op Op, build BuildFunc) (*Result, error) {
	return s.do(ctx, op, func(idx *index.Index) (any, []envelope.Object, []Effect, error) {
		result, objects, err := build(idx)
		return result, objects, nil, err
	})
}

func (s *Store) do(ctx context.Context, op Op, build EffectsFunc) (*Result, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.Index.Refresh(); err != nil {
		return nil, err
	}
	if err := s.completeJournals(ctx, false); err != nil {
		return nil, err
	}
	if res, err := s.replay(ctx, op); res != nil || err != nil {
		return res, err
	}
	result, members, effects, err := build(s.Index)
	if err != nil {
		return nil, err
	}
	resultBytes, err := canon.Marshal(result)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(members))
	for _, m := range members {
		hashes = append(hashes, m.Hash)
	}
	slices.Sort(hashes)
	receipt, err := envelope.Seal(schema.KindOperationReceipt, schema.OperationReceipt{
		OperationID:   op.ID,
		Operation:     op.Name,
		RequestDigest: op.Digest,
		Result:        resultBytes,
		Members:       hashes,
		At:            schema.Timestamp(s.opts.Now()),
	}, op.Signer)
	if err != nil {
		return nil, err
	}
	g := index.Group{Receipt: receipt, Members: members}
	if err := s.Index.Validate(ctx, g); err != nil {
		return nil, err
	}
	if err := s.publish(ctx, g, effects); err != nil {
		if apierr.HasCode(err, "cleanup_pending") {
			return nil, apierr.As(err).With("committed", true).With("operation_id", op.ID).With("receipt", receipt.Hash)
		}
		return nil, err
	}
	return &Result{OperationID: op.ID, Receipt: receipt.Hash, Result: resultBytes, Members: hashes}, nil
}

func (s *Store) replay(ctx context.Context, op Op) (*Result, error) {
	ops, err := s.Index.Operations(ctx, op.ID)
	if err != nil || len(ops) == 0 {
		return nil, err
	}
	if len(ops) > 1 {
		return nil, apierr.New("operation_conflict", "operation %s has %d conflicting receipts", op.ID, len(ops))
	}
	if ops[0].RequestDigest != op.Digest || ops[0].Operation != op.Name {
		return nil, apierr.New("operation_mismatch", "operation %s was committed with a different request", op.ID)
	}
	o, err := s.Read(ops[0].Receipt)
	if err != nil {
		return nil, err
	}
	var r schema.OperationReceipt
	if err := o.Decode(&r); err != nil {
		return nil, err
	}
	return &Result{OperationID: op.ID, Receipt: o.Hash, Result: r.Result, Members: r.Members, Replayed: true}, nil
}

// journal is the durable record of a fully prepared group, written before
// any object so recovery can finish publishing it without re-signing.
type journal struct {
	Receipt string   `json:"receipt"`
	Objects []string `json:"objects"`
	Effects []Effect `json:"effects,omitempty"`
}

func (s *Store) journalPath(receipt string) string {
	return filepath.Join(s.Dir, "journal", receipt+".json")
}

func (s *Store) publish(ctx context.Context, g index.Group, effects []Effect) error {
	if err := s.fault(StageJournal); err != nil {
		return err
	}
	j := journal{Receipt: g.Receipt.Hash, Effects: effects}
	for _, o := range append(slices.Clone(g.Members), g.Receipt) {
		j.Objects = append(j.Objects, string(o.Bytes))
	}
	jb, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(s.journalPath(g.Receipt.Hash), jb, 0o600); err != nil {
		return err
	}
	return s.finish(ctx, g, effects)
}

// finish publishes a journaled group. Every step is idempotent, so
// recovery can rerun it after a crash at any point.
func (s *Store) finish(ctx context.Context, g index.Group, effects []Effect) error {
	if done, err := s.Index.HasReceipt(ctx, g.Receipt.Hash); err != nil {
		return err
	} else if done {
		return s.cleanup(ctx, g, effects)
	}
	// A durable receipt means its pre-publication effects already completed.
	// Do not try to move the branch backwards if someone advanced it later.
	if _, err := s.Read(g.Receipt.Hash); err != nil {
		if err := s.fault(StageEffects); err != nil {
			return err
		}
		if err := s.effects(ctx, effects, false); err != nil {
			return err
		}
	}
	if err := s.fault(StageObjects); err != nil {
		return err
	}
	for _, m := range g.Members {
		if err := s.writeObject(m); err != nil {
			return err
		}
	}
	if err := s.fault(StageReceipt); err != nil {
		return err
	}
	if err := s.writeObject(g.Receipt); err != nil {
		return err
	}
	if err := s.fault(StageRef); err != nil {
		return err
	}
	hashes := []string{g.Receipt.Hash}
	for _, m := range g.Members {
		hashes = append(hashes, m.Hash)
	}
	sync := &refSync{}
	previous, applied, err := s.appendToRef(ctx, hashes)
	if err != nil {
		return err
	}
	if !applied {
		var err error
		if sync, err = s.syncRef(ctx, previous); err != nil {
			return err
		}
	}
	if err := s.fault(StageIndex); err != nil {
		return err
	}
	if err := s.Index.Apply(ctx, g); err != nil {
		return err
	}
	if len(sync.Materialized) > 0 {
		// Groups fetched onto the ref by others become visible too.
		if err := s.indexAll(ctx, &Report{}); err != nil {
			return err
		}
	}
	return s.cleanup(ctx, g, effects)
}

func (s *Store) cleanup(ctx context.Context, g index.Group, effects []Effect) error {
	if err := s.fault(StageCleanup); err != nil {
		return err
	}
	if err := s.effects(ctx, effects, true); err != nil {
		return err
	}
	if err := s.fault(StageDone); err != nil {
		return err
	}
	if err := os.Remove(s.journalPath(g.Receipt.Hash)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// completeJournals finishes every journaled group, after verifying it as
// strictly as a new one. With deferUnavailable, groups whose signing keys
// are not indexed yet are left for a later pass instead of failing.
func (s *Store) completeJournals(ctx context.Context, deferUnavailable bool) error {
	files, err := filepath.Glob(filepath.Join(s.Dir, "journal", "*.json"))
	if err != nil {
		return err
	}
	slices.Sort(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var j journal
		if err := json.Unmarshal(raw, &j, json.RejectUnknownMembers(true)); err != nil {
			return apierr.New("journal_corrupt", "%s: %v", f, err)
		}
		if !isHash(j.Receipt) || filepath.Base(f) != j.Receipt+".json" {
			return apierr.New("journal_corrupt", "%s does not match its receipt", f)
		}
		var g index.Group
		for _, b := range j.Objects {
			o, err := envelope.Parse([]byte(b))
			if err != nil {
				return apierr.New("journal_corrupt", "%s: %v", f, err)
			}
			if o.Hash == j.Receipt {
				g.Receipt = o
			} else {
				g.Members = append(g.Members, o)
			}
		}
		if g.Receipt.Hash != j.Receipt {
			return apierr.New("journal_corrupt", "%s does not contain its receipt", f)
		}
		if err := s.Index.Verify(ctx, g); err != nil {
			if deferUnavailable && apierr.HasCode(err, index.CodeUnavailable) {
				continue
			}
			return apierr.New("journal_invalid", "%s: %v", f, err)
		}
		if err := s.finish(ctx, g, j.Effects); err != nil {
			if apierr.HasCode(err, "cleanup_pending") {
				continue
			}
			return err
		}
	}
	return nil
}

// ObjectPath returns where the object with hash is stored.
func (s *Store) ObjectPath(hash string) string {
	return filepath.Join(s.Dir, "objects", hash[:2], hash[2:]+".json")
}

// Read returns a stored object, verifying its hash and encoding.
func (s *Store) Read(hash string) (envelope.Object, error) {
	if !isHash(hash) {
		return envelope.Object{}, apierr.New("invalid_hash", "%q is not a record hash", hash)
	}
	raw, err := os.ReadFile(s.ObjectPath(hash))
	if errors.Is(err, fs.ErrNotExist) {
		return envelope.Object{}, apierr.New("record_unavailable", "record %s is not present", hash)
	}
	if err != nil {
		return envelope.Object{}, err
	}
	return envelope.ParseStored(raw, hash)
}

// writeObject stores o unless an identical file is already present. A
// present file with different content is quarantined first.
func (s *Store) writeObject(o envelope.Object) error {
	path := s.ObjectPath(o.Hash)
	existing, err := os.ReadFile(path)
	if err == nil && bytes.Equal(existing, o.Bytes) {
		return nil
	}
	if err == nil {
		if err := s.quarantine(path, o.Hash); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := atomicfile.MakeDir(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, o.Bytes, 0o600)
}

func isHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
