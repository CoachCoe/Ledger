package record

import (
	"context"
	"os"
	"slices"

	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// StageImportObjects is the fault stage before an import writes anything.
const StageImportObjects = "import_objects"

// ImportRecords publishes already-staged, already-verified records through
// the same recovery path as a fetch followed by reindex: write the missing
// object files atomically (members before receipts), add them to the
// records ref with compare-and-swap (Phase 1) or index them and append
// them to the head log (native), and scan
// for conflicts. Records that fail group verification stay on disk
// unindexed and are reported in Unverifiable. It never signs, never
// resolves a conflict, and importing the same records again changes
// nothing.
func (s *Store) ImportRecords(ctx context.Context, objs []envelope.Object) (*Report, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := s.Index.Refresh(); err != nil {
		return nil, err
	}
	if err := s.completeJournals(ctx, false); err != nil {
		return nil, err
	}
	if err := s.fault(StageImportObjects); err != nil {
		return nil, err
	}
	if err := s.fault(StageObjects); err != nil {
		return nil, err
	}
	var members, receipts []envelope.Object
	for _, o := range objs {
		if o.Envelope.Kind == schema.KindOperationReceipt {
			receipts = append(receipts, o)
		} else {
			members = append(members, o)
		}
	}
	imported := 0
	write := func(list []envelope.Object) error {
		for _, o := range list {
			_, statErr := os.Stat(s.ObjectPath(o.Hash))
			if err := s.writeObject(o); err != nil {
				return err
			}
			if statErr != nil {
				imported++
			}
		}
		return nil
	}
	if err := write(members); err != nil {
		return nil, err
	}
	if err := s.fault(StageReceipt); err != nil {
		return nil, err
	}
	if err := write(receipts); err != nil {
		return nil, err
	}
	r := &Report{Imported: imported}
	if s.log == nil {
		if err := s.fault(StageRef); err != nil {
			return nil, err
		}
		sync, err := s.syncRef(ctx)
		if err != nil {
			return nil, err
		}
		r.Materialized, r.AddedToRef, r.InvalidInRef = len(sync.Materialized), sync.Added, sync.Invalid
	}
	if err := s.fault(StageIndex); err != nil {
		return nil, err
	}
	if err := s.indexAll(ctx, r); err != nil {
		return nil, err
	}
	if err := s.fault(StageLog); err != nil {
		return nil, err
	}
	if err := s.reconcileLog(ctx, r); err != nil {
		return nil, err
	}
	if err := s.completeJournals(ctx, false); err != nil {
		return nil, err
	}
	slices.Sort(r.Pending)
	return r, s.conflicts(ctx, r)
}
