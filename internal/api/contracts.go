package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/schemas"
)

// requestSchema is the subset of v1/api/request.json that names operations.
type requestSchema struct {
	Properties struct{ Operation struct{ Enum []string } }
	AllOf      []struct {
		If struct {
			Properties struct {
				Operation struct{ Not struct{ Enum []string } }
			}
		}
	}
}

func loadRequestSchema() (requestSchema, error) {
	var doc requestSchema
	b, err := schemas.FS.ReadFile("v1/api/request.json")
	if err != nil {
		return doc, err
	}
	return doc, json.Unmarshal(b, &doc)
}

// readOnly is the request schema's list of operations exempt from the
// operation id requirement. The schema is embedded, so failing to read it is
// a build defect.
var readOnly = func() map[string]bool {
	doc, err := loadRequestSchema()
	if err != nil || len(doc.AllOf) != 1 {
		panic(fmt.Sprintf("api: embedded request schema: %v", err))
	}
	ops := map[string]bool{}
	for _, op := range doc.AllOf[0].If.Properties.Operation.Not.Enum {
		ops[op] = true
	}
	return ops
}()

// Operations is the versioned protocol's authoritative operation list.
func Operations() ([]string, error) {
	doc, err := loadRequestSchema()
	if err != nil {
		return nil, err
	}
	slices.Sort(doc.Properties.Operation.Enum)
	return doc.Properties.Operation.Enum, nil
}

// ToolSchemas packages Ledger's schemas without external fetches. A tool's
// name supplies operation; api_version is fixed by this adapter. Retry
// identities remain explicit, so reconnects cannot duplicate mutations. The
// session is not an argument: the adapter supplies the one it is bound to.
func ToolSchemas(op string) (json.RawMessage, json.RawMessage, error) {
	in := map[string]any{"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"operation_id": map[string]any{"type": "string"},
			"input":        map[string]any{"$ref": op + ".json"}},
		"required": []string{"input"}}
	if !ReadOnly(op) {
		in["required"] = []string{"operation_id", "input"}
		in["properties"].(map[string]any)["operation_id"] = map[string]any{"$ref": "../common.json#/$defs/operation_id"}
	}
	a, err := bundle(in, "v1/api/"+op+".tool.json")
	if err != nil {
		return nil, nil, err
	}
	b, err := bundle(map[string]any{"$ref": responseName(op) + ".json"}, "v1/api/"+op+".tool-output.json")
	return a, b, err
}

func responseName(op string) string {
	if strings.HasPrefix(op, "bundle.") {
		return op + ".response"
	}
	return op + ".output"
}

// ValidateResponse checks the complete API envelope. The original standalone
// bundle report schemas remain usable by offline verification clients.
func ValidateResponse(op string, raw []byte) error { return schema.ValidateAPI(responseName(op), raw) }

func bundle(root map[string]any, base string) (json.RawMessage, error) {
	defs := map[string]any{}
	var walk func(any, string) error
	walk = func(v any, doc string) error {
		switch node := v.(type) {
		case map[string]any:
			delete(node, "$id")
			if ref, ok := node["$ref"].(string); ok {
				u, err := url.Parse(doc)
				if err != nil {
					return err
				}
				r, err := url.Parse(ref)
				if err != nil {
					return err
				}
				target := u.ResolveReference(r)
				if target.IsAbs() {
					return fmt.Errorf("external schema reference %s", ref)
				}
				target.Path = strings.TrimPrefix(target.Path, "/")
				key := strings.ReplaceAll(target.Path, "/", "_")
				node["$ref"] = "#/$defs/" + key
				if target.Fragment != "" {
					node["$ref"] = node["$ref"].(string) + strings.TrimPrefix(target.Fragment, "#")
				}
				if _, loaded := defs[key]; !loaded {
					raw, err := schemas.FS.ReadFile(target.Path)
					if err != nil {
						return err
					}
					var nested any
					if err := json.Unmarshal(raw, &nested); err != nil {
						return err
					}
					defs[key] = nested // register before walking recursive references
					if err := walk(nested, target.Path); err != nil {
						return err
					}
				}
			}
			for k, child := range node {
				if k != "$ref" {
					if err := walk(child, doc); err != nil {
						return err
					}
				}
			}
		case []any:
			for _, child := range node {
				if err := walk(child, doc); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, base); err != nil {
		return nil, err
	}
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = "https://schemas.ledger.invalid/" + base
	root["$defs"] = defs
	return json.Marshal(root)
}
