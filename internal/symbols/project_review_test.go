package symbols

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
)

// fixedSource serves one tree of the given files, which no store would
// build.
type fixedSource map[string]string

func (f fixedSource) Entries(context.Context, string) ([]Entry, error) {
	var out []Entry
	for _, p := range slices.Sorted(maps.Keys(f)) {
		out = append(out, Entry{Path: p, ID: p, Regular: true, Size: int64(len(f[p]))})
	}
	return out, nil
}

func (f fixedSource) Read(_ context.Context, entries []Entry) ([][]byte, error) {
	out := make([][]byte, len(entries))
	for i, e := range entries {
		out[i] = []byte(f[e.ID])
	}
	return out, nil
}

// A store refuses normalization aliases; a source that serves them
// anyway still does not have one file silently replace the other.
func TestNormalizedPathAliasesFallBack(t *testing.T) {
	var engine Engine
	project, err := engine.load(context.Background(), fixedSource{"café.ts": "function f() {}", "cafe\u0301.ts": "function f() {}"}, "tree")
	if err != nil {
		t.Fatal(err)
	}
	if len(project) != 1 || project["café.ts"].Fallback != "ambiguous_path" {
		t.Fatalf("normalized aliases silently overwritten: %+v", project)
	}
}

func TestOversizeSourceKeepsOtherFilesAnalyzable(t *testing.T) {
	s := newStore(t)
	tree := storeTree(t, s, map[string]string{"large.ts": strings.Repeat("x", maxSource+1), "small.ts": "function f() {}"})
	var engine Engine
	project, err := engine.load(context.Background(), StoreSource{Store: s}, tree)
	if err != nil {
		t.Fatal(err)
	}
	if project["large.ts"].Fallback != "source_too_large" || len(project["small.ts"].Declarations) == 0 {
		t.Fatal(project)
	}
}
