// Package symbols provides advisory syntax analysis, never a proof of independence.
package symbols

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"unsafe"

	sitter "github.com/tree-sitter/go-tree-sitter"
	javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
	"golang.org/x/text/unicode/norm"

	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/schema"
)

type Declaration struct {
	Symbol   schema.Symbol `json:"symbol"`
	Revision string        `json:"revision"`
	Start    uint          `json:"start"`
	End      uint          `json:"end"`
}
type Reference struct {
	Source     schema.Symbol `json:"source,omitzero"`
	Name       string        `json:"name"`
	Confidence string        `json:"confidence"`
	Shadowed   bool          `json:"shadowed,omitzero"`
}
type Import struct{ Local, Name, Module string }
type Analysis struct {
	Declarations []Declaration
	References   []Reference
	Imports      []Import
	Fallback     string
}

// SymbolAnalyzer is implemented by each supported grammar. Results contain
// no native tree pointers and can safely be cached after the parser closes.
type SymbolAnalyzer interface {
	Analyze(string, []byte) (Analysis, error)
}
type JavaScriptAnalyzer struct{}
type TypeScriptAnalyzer struct{ TSX bool }
type PythonAnalyzer struct{}

func (JavaScriptAnalyzer) Analyze(p string, b []byte) (Analysis, error) {
	return analyze(p, b, "javascript", javascript.Language())
}
func (a TypeScriptAnalyzer) Analyze(p string, b []byte) (Analysis, error) {
	if a.TSX {
		return analyze(p, b, "tsx", typescript.LanguageTSX())
	}
	return analyze(p, b, "typescript", typescript.LanguageTypescript())
}
func (PythonAnalyzer) Analyze(p string, b []byte) (Analysis, error) {
	return analyze(p, b, "python", python.Language())
}
func ForPath(p string) SymbolAnalyzer {
	switch strings.ToLower(path.Ext(p)) {
	case ".js", ".jsx", ".mjs", ".cjs":
		return JavaScriptAnalyzer{}
	case ".ts", ".mts", ".cts":
		return TypeScriptAnalyzer{}
	case ".tsx":
		return TypeScriptAnalyzer{TSX: true}
	case ".py", ".pyi":
		return PythonAnalyzer{}
	}
	return nil
}
func Key(s schema.Symbol) string   { b, _ := canon.Marshal(s); return string(b) }
func Same(a, b schema.Symbol) bool { return Key(a) == Key(b) }

var kinds = map[string]string{
	"function_declaration": "function", "generator_function_declaration": "function",
	"function_definition": "function", "class_definition": "class", "class_declaration": "class",
	"abstract_class_declaration": "class", "method_definition": "method", "abstract_method_signature": "method",
	"method_signature": "method", "function_signature": "function", "interface_declaration": "interface",
	"type_alias_declaration": "type", "enum_declaration": "enum", "public_field_definition": "field",
	"property_signature": "field",
}

type declNode struct {
	node     *sitter.Node
	symbol   schema.Symbol
	children []int
}

func analyze(p string, src []byte, language string, ptr unsafe.Pointer) (Analysis, error) {
	p = norm.NFC.String(p)
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ptr)); err != nil {
		return Analysis{}, err
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return Analysis{Fallback: "parser_unavailable"}, nil
	}
	defer tree.Close()
	root := tree.RootNode()
	if root.HasError() {
		return Analysis{Fallback: "parser_error"}, nil
	}
	nodes := []declNode{{node: root, symbol: schema.Symbol{Path: p, Language: language, Name: "<module>", Kind: "module"}}}
	owners := map[uintptr]int{root.Id(): 0}
	unmapped := false
	var visit func(*sitter.Node, []string, int)
	visit = func(n *sitter.Node, scope []string, parent int) {
		kind := kinds[n.Kind()]
		name := n.ChildByFieldName("name")
		if n.Kind() == "variable_declarator" {
			v := n.ChildByFieldName("value")
			if len(scope) == 0 || v != nil && (v.Kind() == "arrow_function" || v.Kind() == "function_expression") {
				kind = "value"
				if v != nil && (v.Kind() == "arrow_function" || v.Kind() == "function_expression") {
					kind = "function"
				}
			}
		}
		if language == "python" && n.Kind() == "assignment" && len(scope) == 0 {
			kind = "value"
			name = n.ChildByFieldName("left")
		}
		if kind != "" && (name == nil || name.Kind() != "identifier" && name.Kind() != "type_identifier" && name.Kind() != "property_identifier") {
			unmapped = true
		}
		if kind != "" && name != nil && (name.Kind() == "identifier" || name.Kind() == "type_identifier" || name.Kind() == "property_identifier") {
			if language == "python" && kind == "function" && isClassMethod(n) {
				kind = "method"
			}
			scope = append(slices.Clone(scope), name.Utf8Text(src))
			idx := len(nodes)
			nodes[parent].children = append(nodes[parent].children, idx)
			nodes = append(nodes, declNode{node: n, symbol: schema.Symbol{Path: p, Language: language, Name: strings.Join(scope, "."), Kind: kind}})
			owners[n.Id()] = idx
			parent = idx
		}
		for i := range n.NamedChildCount() {
			visit(n.NamedChild(i), scope, parent)
		}
	}
	visit(root, nil, 0)
	for _, d := range nodes {
		if len(d.symbol.Name) > 1024 {
			return Analysis{Fallback: "locator_too_long"}, nil
		}
	}
	counts := map[string]int{}
	for i := range nodes {
		d := &nodes[i]
		counts[Key(d.symbol)]++
	}
	r := Analysis{Declarations: []Declaration{}, References: []Reference{}, Imports: imports(root, src, language)}
	if unmapped {
		r.Fallback = "unresolved_declaration"
	}
	seen := map[string]int{}
	for i := range nodes {
		d := &nodes[i]
		key := Key(d.symbol)
		if counts[key] > 1 {
			seen[key]++
			dis := strconv.Itoa(seen[key])
			d.symbol.Disambiguator = &dis
			r.Fallback = "ambiguous_declaration"
		}
		// Mask nested declarations. Editing one method does not write its class
		// or an enclosing function; a changed class header still writes the class.
		var text strings.Builder
		if parent := d.node.Parent(); parent != nil {
			if parent.Kind() == "decorated_definition" {
				text.Write(src[parent.StartByte():d.node.StartByte()])
			}
			if d.node.Kind() == "variable_declarator" && parent.ChildCount() > 0 {
				text.WriteString(parent.Child(0).Kind())
			}
		}
		pos := d.node.StartByte()
		for _, idx := range d.children {
			child := nodes[idx]
			text.Write(src[pos:child.node.StartByte()])
			text.WriteString("<declaration>")
			pos = child.node.EndByte()
		}
		text.Write(src[pos:d.node.EndByte()])
		r.Declarations = append(r.Declarations, Declaration{d.symbol, canon.Hash([]byte(text.String())), d.node.StartByte(), d.node.EndByte()})
	}
	scopes := bindingScopes(root, src, language, owners)
	var refs func(*sitter.Node, int, *bindingScope)
	refs = func(n *sitter.Node, owner int, scope *bindingScope) {
		if idx, ok := owners[n.Id()]; ok {
			owner = idx
		}
		if local := scopes[n.Id()]; local != nil {
			scope = local
		}
		kind := n.Kind()
		property := false
		if parent := n.Parent(); parent != nil && parent.Kind() == "attribute" {
			if attr := parent.ChildByFieldName("attribute"); attr != nil && attr.Id() == n.Id() {
				property = true
			}
		}
		if !property && (kind == "identifier" || kind == "type_identifier" || kind == "member_expression" || kind == "attribute") {
			name := n.Utf8Text(src)
			if len(name) < 1024 {
				symbol := nodes[owner].symbol
				first, _, _ := strings.Cut(name, ".")
				shadowed := scope.shadows(first, language)
				if language == "python" && symbol.Kind == "method" && strings.HasPrefix(name, "self.") {
					shadowed = false
				}
				r.References = append(r.References, Reference{Source: symbol, Name: name, Confidence: "unresolved", Shadowed: shadowed})
			}
		}
		for i := range n.NamedChildCount() {
			refs(n.NamedChild(i), owner, scope)
		}
	}
	refs(root, 0, nil)
	return r, nil
}
func isClassMethod(n *sitter.Node) bool {
	p := n.Parent()
	if p != nil && p.Kind() == "decorated_definition" {
		p = p.Parent()
	}
	return p != nil && p.Kind() == "block" && p.Parent() != nil && p.Parent().Kind() == "class_definition"
}

func imports(root *sitter.Node, src []byte, language string) []Import {
	out := []Import{}
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Kind() == "import_statement" && language != "python" {
			mod := n.ChildByFieldName("source")
			if mod != nil {
				module := strings.Trim(mod.Utf8Text(src), "\"'")
				var bindings func(*sitter.Node)
				bindings = func(b *sitter.Node) {
					if b.Kind() == "import_specifier" {
						name, alias := b.ChildByFieldName("name"), b.ChildByFieldName("alias")
						if name != nil {
							local := name.Utf8Text(src)
							if alias != nil {
								local = alias.Utf8Text(src)
							}
							out = append(out, Import{local, name.Utf8Text(src), module})
						}
						return
					}
					if b.Kind() == "namespace_import" {
						for i := range b.NamedChildCount() {
							c := b.NamedChild(i)
							if c.Kind() == "identifier" {
								out = append(out, Import{c.Utf8Text(src), "*", module})
							}
						}
						return
					}
					if b.Kind() == "identifier" {
						out = append(out, Import{b.Utf8Text(src), "default", module})
						return
					}
					for i := range b.NamedChildCount() {
						bindings(b.NamedChild(i))
					}
				}
				if clause := n.NamedChild(0); clause != nil && clause.Kind() == "import_clause" {
					bindings(clause)
				}
			}
			return
		}
		if language == "python" && (n.Kind() == "import_from_statement" || n.Kind() == "import_statement") {
			module := ""
			if m := n.ChildByFieldName("module_name"); m != nil {
				module = m.Utf8Text(src)
			}
			for i := range n.NamedChildCount() {
				b := n.NamedChild(i)
				if m := n.ChildByFieldName("module_name"); m != nil && m.Id() == b.Id() {
					continue
				}
				name, local := b.Utf8Text(src), b.Utf8Text(src)
				if b.Kind() == "aliased_import" {
					name = b.ChildByFieldName("name").Utf8Text(src)
					local = b.ChildByFieldName("alias").Utf8Text(src)
				}
				if module == "" {
					out = append(out, Import{local, "*", name})
				} else {
					out = append(out, Import{local, name, module})
				}
			}
			return
		}
		for i := range n.NamedChildCount() {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
	return out
}

func cleanPath(p string) error {
	if p == "" || p == "." || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) || path.IsAbs(p) || path.Clean(p) != p || strings.HasPrefix(p, "../") {
		return fmt.Errorf("invalid repository path %q", p)
	}
	return nil
}
