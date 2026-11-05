package record

import (
	"context"
	"errors"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func TestJournalReplaysFrozenEffects(t *testing.T) {
	s := newStore(t)
	calls := 0
	failed := false
	w := openStore(t, s.Repo, Options{Effect: func(_ context.Context, e Effect) error {
		if e.New != "frozen" {
			t.Fatal(e)
		}
		calls++
		return nil
	}, Fault: func(stage string) error {
		if stage == StageReceipt && !failed {
			failed = true
			return errors.New("crash")
		}
		return nil
	}})
	o := op("effect")
	object := intentRecord(t, schema.NewID(schema.PrefixIntent))
	_, err := w.DoEffects(ctx, o, func(*index.Index) (any, []envelope.Object, []Effect, error) {
		return map[string]bool{"done": true}, []envelope.Object{object}, []Effect{{Kind: "test", New: "frozen"}}, nil
	})
	if err == nil {
		t.Fatal("fault missing")
	}
	if done, _ := w.Index.Committed(ctx, object.Hash); done {
		t.Fatal("pending object visible")
	}
	restarted := openStore(t, s.Repo, Options{Effect: func(_ context.Context, e Effect) error {
		calls++
		if e.New != "frozen" {
			t.Fatal(e)
		}
		return nil
	}})
	if _, err := restarted.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := restarted.Do(ctx, o, func(*index.Index) (any, []envelope.Object, error) {
		t.Fatal("retry rebuilt operation")
		return nil, nil, nil
	})
	if err != nil || !res.Replayed {
		t.Fatalf("%+v %v", res, err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestCommittedEffectsAreNotReapplied(t *testing.T) {
	s := newStore(t)
	calls := 0
	w := openStore(t, s.Repo, Options{Effect: func(context.Context, Effect) error { calls++; return nil }, Fault: func(stage string) error {
		if stage == StageDone {
			return errors.New("crash after commit")
		}
		return nil
	}})
	_, err := w.DoEffects(ctx, op("committed-effect"), func(*index.Index) (any, []envelope.Object, []Effect, error) {
		return map[string]bool{"done": true}, []envelope.Object{intentRecord(t, schema.NewID(schema.PrefixIntent))}, []Effect{{Kind: "test"}}, nil
	})
	if err == nil {
		t.Fatal("fault missing")
	}
	restarted := openStore(t, s.Repo, Options{Effect: func(context.Context, Effect) error { t.Fatal("committed ref effect reapplied"); return nil }})
	if _, err := restarted.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

// A committed group whose cleanup is pending keeps its journal. Rebuilding
// the index must not treat that journal as unverifiable before the groups
// that supply its keys are indexed again, or every later operation fails.
func TestReindexWithPendingCleanup(t *testing.T) {
	s := newStore(t)
	w := openStore(t, s.Repo, Options{Effect: func(context.Context, Effect) error { return errors.New("user files remain") }})
	_, err := w.DoEffects(ctx, op("cleanup"), func(*index.Index) (any, []envelope.Object, []Effect, error) {
		return map[string]bool{"done": true}, []envelope.Object{intentRecord(t, schema.NewID(schema.PrefixIntent))}, []Effect{{Kind: "test", After: true}}, nil
	})
	if !apierr.HasCode(err, "cleanup_pending") {
		t.Fatal(err)
	}
	r, err := w.Reindex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.CleanupPending) != 1 {
		t.Fatalf("cleanup not reported: %+v", r)
	}
	if _, err := w.Do(ctx, op("after-reindex"), func(*index.Index) (any, []envelope.Object, error) {
		return map[string]bool{}, []envelope.Object{intentRecord(t, schema.NewID(schema.PrefixIntent))}, nil
	}); err != nil {
		t.Fatal(err)
	}
}
