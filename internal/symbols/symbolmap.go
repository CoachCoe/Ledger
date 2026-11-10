package symbols

import (
	"bytes"
	"encoding/json"
	"runtime/debug"
	"sync"

	"golang.org/x/text/unicode/norm"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/store"
)

// AnalyzerVersion names this package's extraction rules. Bump it whenever
// a change could alter the output for any input, so maps written by the
// old rules are never mistaken for the new ones (ADR 0025).
const AnalyzerVersion = "1"

// Analyzer pins what produced a symbol map: these rules, the grammar
// module and the tree-sitter runtime, each at an exact version.
type Analyzer struct {
	Version  string `json:"version"`
	Language string `json:"language"`
	Grammar  string `json:"grammar"`
	Runtime  string `json:"runtime"`
}

// Map is a symbol_map object: the analysis of one blob at one path, since
// locators and the grammar both depend on the path.
type Map struct {
	Analyzer     Analyzer      `json:"analyzer"`
	Blob         string        `json:"blob"`
	Path         string        `json:"path"`
	Declarations []Declaration `json:"declarations"`
	References   []Reference   `json:"references"`
	Imports      []Import      `json:"imports"`
	Fallback     string        `json:"fallback,omitzero"`
}

var grammarModules = map[string]string{
	"javascript": "github.com/tree-sitter/tree-sitter-javascript",
	"typescript": "github.com/tree-sitter/tree-sitter-typescript",
	"tsx":        "github.com/tree-sitter/tree-sitter-typescript",
	"python":     "github.com/tree-sitter/tree-sitter-python",
}

const runtimeModule = "github.com/tree-sitter/go-tree-sitter"

var moduleVersions = sync.OnceValue(func() map[string]string {
	out := map[string]string{}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Replace != nil {
				d = d.Replace
			}
			out[d.Path] = d.Version
		}
	}
	return out
})

func languageFor(a SymbolAnalyzer) string {
	switch a := a.(type) {
	case JavaScriptAnalyzer:
		return "javascript"
	case TypeScriptAnalyzer:
		if a.TSX {
			return "tsx"
		}
		return "typescript"
	case PythonAnalyzer:
		return "python"
	}
	return ""
}

// CurrentAnalyzer returns the pinned identity of the analyzer for path, and
// false when the file is unsupported and so has no map.
func CurrentAnalyzer(p string) (Analyzer, bool, error) {
	language := languageFor(ForPath(p))
	if language == "" {
		return Analyzer{}, false, nil
	}
	mod := grammarModules[language]
	grammar, runtime := moduleVersions()[mod], moduleVersions()[runtimeModule]
	if grammar == "" || runtime == "" {
		return Analyzer{}, false, apierr.New("analyzer_unpinned", "this binary does not record the version of %s", mod)
	}
	return Analyzer{Version: AnalyzerVersion, Language: language, Grammar: mod + "@" + grammar, Runtime: runtimeModule + "@" + runtime}, true, nil
}

// MapFile analyzes src, the bytes of blob at path p. It returns false for
// unsupported files, which keep the file-level fallbacks instead.
func MapFile(p, blob string, src []byte) (Map, bool, error) {
	p = norm.NFC.String(p)
	analyzer, ok, err := CurrentAnalyzer(p)
	if !ok || err != nil {
		return Map{}, false, err
	}
	if len(src) > store.ChunkThreshold {
		return Map{}, false, nil
	}
	a, err := ForPath(p).Analyze(p, src)
	if err != nil {
		return Map{}, false, err
	}
	m := Map{Analyzer: analyzer, Blob: blob, Path: p, Declarations: a.Declarations, References: a.References, Imports: a.Imports, Fallback: a.Fallback}
	if m.Declarations == nil {
		m.Declarations = []Declaration{}
	}
	if m.References == nil {
		m.References = []Reference{}
	}
	if m.Imports == nil {
		m.Imports = []Import{}
	}
	return m, true, nil
}

func (m Map) encode() ([]byte, error) { return canon.Marshal(m) }

// Put stores m as a symbol_map object.
func (m Map) Put(b *store.Batch) (string, error) {
	raw, err := m.encode()
	if err != nil {
		return "", err
	}
	return b.Put(store.KindSymbolMap, raw)
}

// LoadMap reads symbol_map id.
func LoadMap(s *store.Store, id string) (Map, error) {
	kind, data, err := s.Get(id)
	if err != nil {
		return Map{}, err
	}
	if kind != store.KindSymbolMap {
		return Map{}, apierr.New("object_kind", "object %s is a %s, not a symbol_map", id, kind)
	}
	var m Map
	if err := json.Unmarshal(data, &m); err != nil {
		return Map{}, apierr.New("object_corrupt", "symbol map %s is malformed", id)
	}
	return m, nil
}

// Recompute reruns the analyzer a stored map names over its blob and
// reports whether the result is the same bytes. A map from another
// analyzer cannot be recomputed by this binary.
func Recompute(s *store.Store, id string) (bool, error) {
	m, err := LoadMap(s, id)
	if err != nil {
		return false, err
	}
	current, ok, err := CurrentAnalyzer(m.Path)
	if err != nil {
		return false, err
	}
	if !ok || current != m.Analyzer {
		return false, apierr.New("analyzer_unavailable", "symbol map %s was made by an analyzer this binary does not have", id).With("analyzer", m.Analyzer)
	}
	var src bytes.Buffer
	if err := s.WriteFile(m.Blob, &src); err != nil {
		return false, err
	}
	again, ok, err := MapFile(m.Path, m.Blob, src.Bytes())
	if err != nil || !ok {
		return false, err
	}
	raw, err := again.encode()
	if err != nil {
		return false, err
	}
	return store.ID(store.KindSymbolMap, raw) == id, nil
}
