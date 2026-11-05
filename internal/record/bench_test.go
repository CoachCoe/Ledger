package record

import (
	"testing"

	"github.com/CoachCoe/Ledger/internal/schema"
)

func BenchmarkDo(b *testing.B) {
	s := newStore(b)
	for b.Loop() {
		if _, err := s.Do(ctx, op("bench"), members(intentRecord(b, schema.NewID(schema.PrefixIntent)))); err != nil {
			b.Fatal(err)
		}
	}
}
