package symbols

import sitter "github.com/tree-sitter/go-tree-sitter"

// Opaque lexical bindings must not be resolved as imports or outer declarations.
// Named declarations already have locators and remain resolvable by resolve.
type bindingScope struct {
	parent   *bindingScope
	opaque   map[string]bool
	function bool
	class    bool
}

func (s *bindingScope) shadows(name, language string) bool {
	insideFunction := false
	for ; s != nil; s = s.parent {
		skipClass := language == "python" && insideFunction && s.class
		if !skipClass && s.opaque[name] {
			return true
		}
		insideFunction = insideFunction || s.function
	}
	return false
}

func functionScope(kind string) bool {
	switch kind {
	case "function_declaration", "generator_function_declaration", "function_definition", "function_expression", "generator_function", "arrow_function", "method_definition", "function_signature", "abstract_method_signature", "method_signature":
		return true
	}
	return false
}

func bindingScopes(root *sitter.Node, src []byte, language string, owners map[uintptr]int) map[uintptr]*bindingScope {
	scopes := map[uintptr]*bindingScope{}
	var visit func(*sitter.Node, *bindingScope)
	visit = func(n *sitter.Node, scope *bindingScope) {
		kind := n.Kind()
		isFunction := functionScope(kind)
		isClass := kind == "class_definition" || kind == "class_declaration" || kind == "abstract_class_declaration"
		if scope == nil || isFunction || isClass || language != "python" && kind == "statement_block" {
			scope = &bindingScope{parent: scope, opaque: map[string]bool{}, function: isFunction, class: isClass}
			scopes[n.Id()] = scope
			if isFunction {
				for _, field := range []string{"parameters", "parameter"} {
					if params := n.ChildByFieldName(field); params != nil {
						patternBindings(params, src, scope.opaque)
					}
				}
				if _, named := owners[n.Id()]; !named {
					if name := n.ChildByFieldName("name"); name != nil {
						patternBindings(name, src, scope.opaque)
					}
				}
			}
		}
		if kind == "variable_declarator" {
			if _, named := owners[n.Id()]; !named {
				target := scope
				if parent := n.Parent(); parent != nil && parent.ChildCount() > 0 && parent.Child(0).Kind() == "var" {
					for target.parent != nil && !target.function {
						target = target.parent
					}
				}
				patternBindings(n.ChildByFieldName("name"), src, target.opaque)
			}
		}
		if language == "python" {
			switch kind {
			case "assignment", "augmented_assignment", "for_statement", "for_in_clause":
				if _, named := owners[n.Id()]; !named {
					patternBindings(n.ChildByFieldName("left"), src, scope.opaque)
				}
			}
		}
		for i := range n.NamedChildCount() {
			visit(n.NamedChild(i), scope)
		}
	}
	visit(root, nil)
	return scopes
}

// Only binding patterns count. Type annotations, property keys and default-value
// expressions are references, not bindings of every identifier they contain.
func patternBindings(n *sitter.Node, src []byte, out map[string]bool) {
	if n == nil {
		return
	}
	switch n.Kind() {
	case "identifier", "shorthand_property_identifier_pattern":
		out[n.Utf8Text(src)] = true
	case "required_parameter", "optional_parameter":
		if pattern := n.ChildByFieldName("pattern"); pattern != nil {
			patternBindings(pattern, src, out)
		} else {
			patternBindings(n.ChildByFieldName("name"), src, out)
		}
	case "default_parameter", "typed_default_parameter":
		patternBindings(n.ChildByFieldName("name"), src, out)
	case "assignment_pattern":
		patternBindings(n.ChildByFieldName("left"), src, out)
	case "pair_pattern":
		patternBindings(n.ChildByFieldName("value"), src, out)
	case "typed_parameter":
		typeNode := n.ChildByFieldName("type")
		for i := range n.NamedChildCount() {
			child := n.NamedChild(i)
			if typeNode == nil || child.Id() != typeNode.Id() {
				patternBindings(child, src, out)
			}
		}
	case "formal_parameters", "parameters", "object_pattern", "array_pattern", "tuple_pattern", "pattern_list", "rest_pattern", "list_splat_pattern", "dictionary_splat_pattern":
		for i := range n.NamedChildCount() {
			patternBindings(n.NamedChild(i), src, out)
		}
	}
}
