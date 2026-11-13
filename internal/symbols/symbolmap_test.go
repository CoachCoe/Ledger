package symbols

import (
	"path/filepath"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/store"
)

// P2-AC-005
func TestSymbolMapRecomputesByteForByte(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "objects"), true)
	if err != nil {
		t.Fatal(err)
	}
	b := s.Batch()
	for _, f := range []struct{ path, src string }{
		{"src/billing/invoice.ts", "import { round } from './money'\nexport class Invoice {\n  total(lines: number[]) { return round(lines.reduce((a, b) => a + b, 0)) }\n}\n"},
		{"app/views.py", "from .models import Order\n\ndef total(order):\n    return sum(l.amount for l in order.lines)\n"},
		{"web/broken.js", "function ( {\n"},
	} {
		blob, err := b.PutFile([]byte(f.src))
		if err != nil {
			t.Fatal(err)
		}
		m, ok, err := MapFile(f.path, blob, []byte(f.src))
		if err != nil || !ok {
			t.Fatalf("%s: %v %v", f.path, ok, err)
		}
		id, err := m.Put(b)
		if err != nil {
			t.Fatal(err)
		}
		same, err := Recompute(s, id)
		if err != nil || !same {
			t.Fatalf("%s: recompute same=%v err=%v", f.path, same, err)
		}
		loaded, err := LoadMap(s, id)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Analyzer.Grammar == "" || loaded.Analyzer.Runtime == "" || loaded.Blob != blob {
			t.Fatalf("%s: map does not pin its analyzer and blob: %+v", f.path, loaded.Analyzer)
		}
		if f.path == "web/broken.js" && loaded.Fallback != "parser_error" {
			t.Fatalf("broken file fallback %q", loaded.Fallback)
		}
		if f.path == "src/billing/invoice.ts" && len(loaded.Declarations) < 3 {
			t.Fatalf("declarations %+v", loaded.Declarations)
		}
	}
	if _, ok, err := MapFile("README.md", "x", []byte("# hi")); ok || err != nil {
		t.Fatalf("unsupported file got a map: %v %v", ok, err)
	}

	old := Map{Analyzer: Analyzer{Version: "0", Language: "typescript", Grammar: "g@v0", Runtime: "r@v0"}, Blob: "x", Path: "a.ts", Declarations: []Declaration{}, References: []Reference{}, Imports: []Import{}}
	id, err := old.Put(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Recompute(s, id); !apierr.HasCode(err, "analyzer_unavailable") {
		t.Fatalf("map from another analyzer: %v", err)
	}
}
