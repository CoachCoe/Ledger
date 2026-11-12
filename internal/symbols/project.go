package symbols

import (
	"context"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/text/unicode/norm"

	"github.com/CoachCoe/Ledger/internal/schema"
)

type Edge struct {
	Source     schema.Symbol `json:"source"`
	Target     schema.Symbol `json:"target"`
	Relation   string        `json:"relation"`
	Confidence string        `json:"confidence"`
	ObservedBy string        `json:"observed_by"`
}
type Fallback struct {
	Path     string `json:"path"`
	Reason   string `json:"reason"`
	Relation string `json:"relation,omitzero"`
}
type Summary struct {
	Advisory   bool            `json:"advisory"`
	Written    []schema.Symbol `json:"written"`
	Read       []schema.Symbol `json:"read"`
	Edges      []Edge          `json:"edges"`
	Fallbacks  []Fallback      `json:"fallbacks"`
	Unresolved []Reference     `json:"unresolved"`
}

// Engine caches immutable blob analyses. Native parse trees never escape Analyze.
type Engine struct {
	mu    sync.Mutex
	cache map[string]Analysis
}

func (e *Engine) load(ctx context.Context, src Source, tree string) (map[string]Analysis, error) {
	entries, err := src.Entries(ctx, tree)
	if err != nil {
		return nil, err
	}
	result := map[string]Analysis{}
	pending := []Entry{}
	paths := map[string]int{}
	for _, entry := range entries {
		paths[norm.NFC.String(entry.Path)]++
	}
	e.mu.Lock()
	if e.cache == nil {
		e.cache = map[string]Analysis{}
	}
	for _, entry := range entries {
		p := norm.NFC.String(entry.Path)
		if paths[p] > 1 {
			result[p] = Analysis{Fallback: "ambiguous_path"}
			continue
		}
		if !entry.Regular {
			result[p] = Analysis{Fallback: "non_regular_file"}
			continue
		}
		if ForPath(p) == nil {
			result[p] = Analysis{Fallback: "unsupported_file"}
			continue
		}
		if entry.Size > maxSource {
			result[p] = Analysis{Fallback: "source_too_large"}
			continue
		}
		if a, ok := e.cache[p+":"+entry.ID]; ok {
			result[p] = a
		} else {
			pending = append(pending, entry)
		}
	}
	e.mu.Unlock()
	// Bound each read's buffered output too, not only each parser input.
	for start := 0; start < len(pending); {
		end := start
		var size int64
		for end < len(pending) && end-start < 128 && size+max(pending[end].Size, 0) <= 8<<20 {
			size += max(pending[end].Size, 0)
			end++
		}
		if err := e.loadBatch(ctx, src, pending[start:end], result); err != nil {
			return nil, err
		}
		start = end
	}
	return result, nil
}

func (e *Engine) loadBatch(ctx context.Context, src Source, entries []Entry, result map[string]Analysis) error {
	contents, err := src.Read(ctx, entries)
	if err != nil {
		return err
	}
	for i, entry := range entries {
		p := norm.NFC.String(entry.Path)
		a := Analysis{Fallback: "source_too_large"}
		if b := contents[i]; b != nil && len(b) <= maxSource {
			if a, err = ForPath(p).Analyze(p, b); err != nil {
				return err
			}
		}
		result[p] = a
		e.mu.Lock()
		if len(e.cache) >= 4096 {
			clear(e.cache)
		}
		e.cache[p+":"+entry.ID] = a
		e.mu.Unlock()
	}
	return nil
}

// Compare uses the exact frozen trees, including deleted declarations. Context
// references use path#lexical.name; an ambiguous/unrecognized reference is labeled.
func (e *Engine) Compare(ctx context.Context, src Source, base, candidate string, files, contexts []string) (Summary, error) {
	before, err := e.load(ctx, src, base)
	if err != nil {
		return Summary{}, err
	}
	after, err := e.load(ctx, src, candidate)
	if err != nil {
		return Summary{}, err
	}
	r := Summary{Advisory: true, Written: []schema.Symbol{}, Read: []schema.Symbol{}, Edges: []Edge{}, Fallbacks: []Fallback{}, Unresolved: []Reference{}}
	written := map[string]bool{}
	for _, raw := range files {
		startWritten := len(r.Written)
		p := norm.NFC.String(raw)
		old, oldOK := before[p]
		next, nextOK := after[p]
		if oldOK && old.Fallback != "" {
			r.Fallbacks = append(r.Fallbacks, Fallback{Path: p, Reason: old.Fallback})
		}
		if nextOK && next.Fallback != "" {
			r.Fallbacks = append(r.Fallbacks, Fallback{Path: p, Reason: next.Fallback})
		}
		oldMap := map[string]Declaration{}
		newMap := map[string]Declaration{}
		for _, d := range old.Declarations {
			oldMap[Key(d.Symbol)] = d
		}
		for _, d := range next.Declarations {
			newMap[Key(d.Symbol)] = d
		}
		for key, d := range oldMap {
			if n, ok := newMap[key]; !ok || n.Revision != d.Revision {
				written[key] = true
				r.Written = append(r.Written, d.Symbol)
			}
		}
		for key, d := range newMap {
			if o, ok := oldMap[key]; !ok || o.Revision != d.Revision {
				if !written[key] {
					r.Written = append(r.Written, d.Symbol)
				}
				written[key] = true
			}
		}
		// A mode-only edit is a file-level write even when syntax is identical.
		if len(oldMap) == 0 && len(newMap) == 0 && old.Fallback == "" && next.Fallback == "" {
			r.Fallbacks = append(r.Fallbacks, Fallback{Path: p, Reason: "unmapped_change"})
		}
		if len(r.Written) == startWritten && (len(oldMap) > 0 || len(newMap) > 0) && old.Fallback == "" && next.Fallback == "" {
			r.Fallbacks = append(r.Fallbacks, Fallback{Path: p, Reason: "non_syntax_change"})
		}
	}
	read := map[string]schema.Symbol{}
	edges := map[string]Edge{}
	candidateSources := map[string]bool{}
	for _, a := range after {
		for _, d := range a.Declarations {
			candidateSources[Key(d.Symbol)] = true
		}
	}
	for revision, project := range []map[string]Analysis{before, after} {
		for _, a := range project {
			for _, ref := range a.References {
				if !written[Key(ref.Source)] {
					continue
				}
				// Surviving declarations describe the candidate's reads. The base
				// contributes only deleted declarations, not calls removed by edits.
				if revision == 0 && candidateSources[Key(ref.Source)] {
					continue
				}
				target, confidence, fallback := resolve(project, ref, a.Imports)
				if fallback != "" {
					r.Fallbacks = append(r.Fallbacks, Fallback{Path: fallback, Reason: "unresolved_import", Relation: "read"})
				}
				if target == nil {
					r.Unresolved = append(r.Unresolved, ref)
					continue
				}
				if Same(ref.Source, *target) {
					continue
				}
				read[Key(*target)] = *target
				edge := Edge{ref.Source, *target, "read", confidence, "tree-sitter"}
				edges[Key(ref.Source)+":"+Key(*target)] = edge
			}
		}
	}
	for _, ref := range contexts {
		p, name, ok := strings.Cut(ref, "#")
		p = norm.NFC.String(p)
		if cleanPath(p) != nil {
			r.Unresolved = append(r.Unresolved, Reference{Name: ref, Confidence: "unresolved"})
			continue
		}
		a, exists := after[p]
		if !exists {
			a = before[p]
		}
		matches := []schema.Symbol{}
		if ok {
			for _, d := range a.Declarations {
				if d.Symbol.Name == name {
					matches = append(matches, d.Symbol)
				}
			}
		}
		if len(matches) == 1 {
			read[Key(matches[0])] = matches[0]
		} else {
			r.Fallbacks = append(r.Fallbacks, Fallback{Path: p, Reason: "declared_context", Relation: "read"})
		}
	}
	for _, s := range read {
		r.Read = append(r.Read, s)
	}
	for _, s := range r.Written {
		r.Edges = append(r.Edges, Edge{Source: s, Target: s, Relation: "write", Confidence: "direct", ObservedBy: "tree-sitter"})
	}
	for _, edge := range edges {
		r.Edges = append(r.Edges, edge)
	}
	sortSymbols(r.Written)
	sortSymbols(r.Read)
	slices.SortFunc(r.Edges, func(a, b Edge) int { return strings.Compare(Key(a.Source)+Key(a.Target), Key(b.Source)+Key(b.Target)) })
	slices.SortFunc(r.Fallbacks, func(a, b Fallback) int {
		return strings.Compare(a.Path+":"+a.Reason+":"+a.Relation, b.Path+":"+b.Reason+":"+b.Relation)
	})
	r.Fallbacks = slices.Compact(r.Fallbacks)
	slices.SortFunc(r.Unresolved, func(a, b Reference) int {
		return strings.Compare(Key(a.Source)+a.Name+strconv.FormatBool(a.Shadowed), Key(b.Source)+b.Name+strconv.FormatBool(b.Shadowed))
	})
	r.Unresolved = slices.CompactFunc(r.Unresolved, func(a, b Reference) bool {
		return Same(a.Source, b.Source) && a.Name == b.Name && a.Shadowed == b.Shadowed
	})
	return r, nil
}
func sortSymbols(s []schema.Symbol) {
	slices.SortFunc(s, func(a, b schema.Symbol) int { return strings.Compare(Key(a), Key(b)) })
}

func resolve(project map[string]Analysis, ref Reference, imps []Import) (*schema.Symbol, string, string) {
	if ref.Shadowed {
		return nil, "unresolved", ""
	}
	name := ref.Name
	if strings.HasPrefix(name, "self.") || strings.HasPrefix(name, "this.") {
		prefix, _, _ := strings.Cut(ref.Source.Name, ".")
		name = prefix + "." + strings.SplitN(name, ".", 2)[1]
	}
	local := project[ref.Source.Path]
	for scope := ref.Source.Name; ; {
		candidate := name
		if scope != "" {
			candidate = scope + "." + name
		}
		matches := []schema.Symbol{}
		for _, d := range local.Declarations {
			if d.Symbol.Name == candidate {
				matches = append(matches, d.Symbol)
			}
		}
		if len(matches) == 1 && matches[0].Disambiguator == nil {
			return &matches[0], "static_inferred", ""
		}
		if len(matches) > 1 {
			return nil, "unresolved", ref.Source.Path
		}
		if scope == "" {
			break
		}
		i := strings.LastIndexByte(scope, '.')
		if i < 0 {
			scope = ""
		} else {
			scope = scope[:i]
		}
	}
	for _, imp := range imps {
		member := imp.Name
		if name != imp.Local {
			if imp.Name != "*" || !strings.HasPrefix(name, imp.Local+".") {
				continue
			}
			member = strings.TrimPrefix(name, imp.Local+".")
		}
		p := modulePath(project, ref.Source.Path, imp.Module, ref.Source.Language)
		if p == "" {
			return nil, "unresolved", ""
		}
		matches := []schema.Symbol{}
		for _, d := range project[p].Declarations {
			if d.Symbol.Name == member {
				matches = append(matches, d.Symbol)
			}
		}
		if len(matches) == 1 && matches[0].Disambiguator == nil {
			return &matches[0], "static_inferred", ""
		}
		return nil, "unresolved", p
	}
	return nil, "unresolved", ""
}

func modulePath(project map[string]Analysis, from, module, language string) string {
	var stem string
	if language == "python" {
		if strings.HasPrefix(module, ".") {
			dots := len(module) - len(strings.TrimLeft(module, "."))
			dir := path.Dir(from)
			for i := 1; i < dots; i++ {
				dir = path.Dir(dir)
			}
			stem = path.Join(dir, strings.ReplaceAll(strings.TrimLeft(module, "."), ".", "/"))
		} else {
			stem = strings.ReplaceAll(module, ".", "/")
		}
	} else {
		if !strings.HasPrefix(module, ".") {
			return ""
		}
		stem = path.Join(path.Dir(from), module)
	}
	candidates := []string{stem, stem + ".ts", stem + ".tsx", stem + ".js", stem + ".jsx", stem + ".mjs", stem + ".py", path.Join(stem, "index.ts"), path.Join(stem, "index.js"), path.Join(stem, "__init__.py")}
	if strings.HasSuffix(stem, ".js") {
		candidates = append(candidates, strings.TrimSuffix(stem, ".js")+".ts")
	}
	// Exact spelling takes priority. Otherwise multiple candidates are ambiguous.
	if _, ok := project[stem]; ok {
		return stem
	}
	found := ""
	for _, p := range candidates[1:] {
		if _, ok := project[p]; ok {
			if found != "" && found != p {
				return ""
			}
			found = p
		}
	}
	return found
}
