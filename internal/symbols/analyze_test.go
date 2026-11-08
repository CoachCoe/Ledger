package symbols

import (
	"strings"
	"testing"
)

func TestAC007_020_181_221_Analyzers(t *testing.T) {
	for _, tc := range []struct {
		path, src string
		names     []string
	}{
		{"a.js", "export function f(x) { return x; }\nclass C { m() {} }\nconst value=1;", []string{"f", "C", "C.m", "value"}},
		{"a.ts", "abstract class C { abstract m(x: number): void; n() {} }\ninterface I { f(): void; }\ntype T = string;", []string{"C", "C.m", "C.n", "I", "I.f", "T"}},
		{"a.tsx", "export const View = () => <span/>;", []string{"View"}},
		{"a.py", "x = 1\ndef f(x):\n return x\nclass C:\n def m(self):\n  pass\n", []string{"x", "f", "C", "C.m"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			a, err := ForPath(tc.path).Analyze(tc.path, []byte(tc.src))
			if err != nil || a.Fallback != "" {
				t.Fatalf("%+v %v", a, err)
			}
			for _, name := range tc.names {
				found := false
				for _, d := range a.Declarations {
					if d.Symbol.Name == name {
						found = true
					}
				}
				if !found {
					t.Errorf("missing %s in %+v", name, a.Declarations)
				}
			}
		})
	}
}
func TestAC117_LocatorEditsAndAmbiguity(t *testing.T) {
	a, _ := ForPath("a.ts").Analyze("a.ts", []byte("function f(x: number) { return x; }\nfunction g() { return 1; }"))
	b, _ := ForPath("a.ts").Analyze("a.ts", []byte("function f(x: string) { return x + '!'; }\nfunction g() { return 1; }"))
	for _, old := range a.Declarations {
		for _, next := range b.Declarations {
			if Same(old.Symbol, next.Symbol) {
				if old.Symbol.Name == "f" && old.Revision == next.Revision {
					t.Fatal("edit lost")
				}
				if old.Symbol.Name == "g" && old.Revision != next.Revision {
					t.Fatal("unrelated declaration changed")
				}
			}
		}
	}
	dup, _ := ForPath("a.js").Analyze("a.js", []byte("function f() {}\nfunction f() {}"))
	if dup.Fallback != "ambiguous_declaration" {
		t.Fatal(dup)
	}
}
func TestAC187_ParserFallback(t *testing.T) {
	for _, p := range []string{"a.js", "a.ts", "a.tsx", "a.py"} {
		a, err := ForPath(p).Analyze(p, []byte("def function ( {"))
		if err != nil || !strings.Contains(a.Fallback, "parser") {
			t.Fatalf("%s: %+v %v", p, a, err)
		}
	}
	if ForPath("a.go") != nil {
		t.Fatal("unsupported file must stay file level")
	}
}

func TestAC182_PythonMethodReferences(t *testing.T) {
	a, err := ForPath("a.py").Analyze("a.py", []byte("def f():\n return 0\nclass C:\n def f(self):\n  return 1\n def g(self):\n  return self.f()\n"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range a.References {
		if ref.Source.Name != "C.g" {
			continue
		}
		if ref.Name == "f" {
			t.Fatal("attribute name was treated as a global")
		}
		if ref.Name == "self.f" {
			target, confidence, _ := resolve(map[string]Analysis{"a.py": a}, ref, nil)
			if target == nil || target.Name != "C.f" || confidence != "static_inferred" {
				t.Fatal(target, confidence)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("method reference missing")
	}
}
