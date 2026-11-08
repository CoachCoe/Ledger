package symbols

import (
	"fmt"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/schema"
)

func TestLocalBindingsDoNotResolveAsImports(t *testing.T) {
	for _, tc := range []struct{ path, api, src string }{
		{"caller.js", "export function f() {}", "import {f} from './api'; function g() { const f = getCallback(); return f(); }"},
		{"caller.ts", "export function f() {}", "import {f} from './api'; function g() { const {f} = callbacks; return f(); }"},
		{"caller.py", "def f():\n pass\n", "from api import f\ndef g():\n f = get_callback()\n return f()\n"},
		{"caller.py", "def f():\n pass\n", "from api import f\ndef outer(f):\n def inner():\n  return f()\n return inner()\n"},
	} {
		t.Run(tc.path+tc.src, func(t *testing.T) {
			a, err := ForPath(tc.path).Analyze(tc.path, []byte(tc.src))
			if err != nil || a.Fallback != "" {
				t.Fatalf("%+v %v", a, err)
			}
			apiPath := "api" + tc.path[strings.LastIndex(tc.path, "."):]
			api, err := ForPath(apiPath).Analyze(apiPath, []byte(tc.api))
			if err != nil {
				t.Fatal(err)
			}
			project := map[string]Analysis{tc.path: a, apiPath: api}
			found := false
			for _, ref := range a.References {
				if ref.Source.Name == "<module>" || ref.Name != "f" {
					continue
				}
				found = true
				target, _, _ := resolve(project, ref, a.Imports)
				if target != nil && target.Path == apiPath {
					t.Fatalf("shadowed binding resolved to import: %+v", ref)
				}
			}
			if !found {
				t.Fatal("reference missing")
			}
		})
	}
}

func TestParameterTypesAndDefaultsRemainReferences(t *testing.T) {
	src := "import {Model} from './api'; function g(value: Model = Model.create()) { return Model.create(); }"
	a, err := ForPath("caller.ts").Analyze("caller.ts", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	api := Analysis{Declarations: []Declaration{{Symbol: schema.Symbol{Path: "api.ts", Language: "typescript", Name: "Model", Kind: "class"}}}}
	resolved := false
	for _, ref := range a.References {
		if ref.Source.Name != "g" || ref.Name != "Model" {
			continue
		}
		target, _, _ := resolve(map[string]Analysis{"caller.ts": a, "api.ts": api}, ref, a.Imports)
		if target != nil {
			resolved = true
		}
	}
	if !resolved {
		t.Fatal("parameter type/default identifiers were treated as parameter bindings")
	}
}

func BenchmarkAnalyzeLargeFile(b *testing.B) {
	for _, count := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			var src strings.Builder
			for i := range count {
				fmt.Fprintf(&src, "function f%d(x) { return x + %d; }\n", i, i)
			}
			raw := []byte(src.String())
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for b.Loop() {
				if _, err := ForPath("large.ts").Analyze("large.ts", raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
