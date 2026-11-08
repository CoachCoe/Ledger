package coord

import (
	"testing"

	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/symbols"
)

func TestAC003_184_OverlapTruthTable(t *testing.T) {
	f := schema.Symbol{Path: "a.ts", Language: "typescript", Name: "f", Kind: "function"}
	g := f
	g.Name = "g"
	other := f
	other.Path = "b.ts"
	for _, tc := range []struct {
		name string
		a, b symbols.Summary
		want int
		kind string
	}{
		{"write_write", symbols.Summary{Written: []schema.Symbol{f}}, symbols.Summary{Written: []schema.Symbol{f}}, 1, "write_write"},
		{"write_read", symbols.Summary{Written: []schema.Symbol{f}}, symbols.Summary{Read: []schema.Symbol{f}}, 1, "write_read"},
		{"read_read", symbols.Summary{Read: []schema.Symbol{f}}, symbols.Summary{Read: []schema.Symbol{f}}, 0, ""},
		{"unrelated_same_file", symbols.Summary{Written: []schema.Symbol{f}}, symbols.Summary{Written: []schema.Symbol{g}}, 0, ""},
		{"same_name_different_module", symbols.Summary{Written: []schema.Symbol{f}}, symbols.Summary{Written: []schema.Symbol{other}}, 0, ""},
		{"parser_fallback", symbols.Summary{Fallbacks: []symbols.Fallback{{Path: "a.ts", Reason: "parser_error"}}}, symbols.Summary{Written: []schema.Symbol{g}}, 1, "file_level"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Overlaps(tc.a, tc.b)
			if len(got) != tc.want {
				t.Fatalf("got %+v", got)
			}
			if len(got) > 0 && got[0].Kind != tc.kind {
				t.Fatal(got)
			}
		})
	}
}

func TestAC186_LeasePathNormalization(t *testing.T) {
	f := schema.Symbol{Path: "Café.ts", Language: "typescript", Name: "f", Kind: "function"}
	s := symbols.Summary{Written: []schema.Symbol{f}}
	l := Lease{Paths: []string{"cafe\u0301.ts"}}
	if len(LeaseConflicts(s, l, false)) != 1 {
		t.Fatal("case/NFC equivalent path missed")
	}
	if len(LeaseConflicts(s, l, true)) != 0 {
		t.Fatal("case-sensitive paths conflated")
	}
	if s.Written[0].Path != "Café.ts" {
		t.Fatal("pure evaluation mutated input")
	}
	read := symbols.Summary{Fallbacks: []symbols.Fallback{{Path: "cafe\u0301.ts", Reason: "unresolved_import", Relation: "read"}}}
	if len(LeaseConflicts(read, l, false)) != 0 {
		t.Fatal("a reference must not conflict with a write lease")
	}
}
