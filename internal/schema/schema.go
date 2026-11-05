// Package schema validates Ledger records against the embedded JSON Schemas
// and defines the Go types for their payloads.
package schema

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/schemas"
)

// Version is the only envelope schema_version this build understands.
const Version = 1

const baseURL = "https://schemas.ledger.invalid/"

var compiled = sync.OnceValues(compileAll)

type schemaSet struct {
	envelope *jsonschema.Schema
	kinds    map[Kind]*jsonschema.Schema
	runtime  map[string]*jsonschema.Schema
	api      map[string]*jsonschema.Schema
}

func compileAll() (*schemaSet, error) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	var urls []string
	err := fs.WalkDir(schemas.FS, "v1", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := schemas.FS.ReadFile(p)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		urls = append(urls, baseURL+p)
		return c.AddResource(baseURL+p, doc)
	})
	if err != nil {
		return nil, err
	}
	s := &schemaSet{kinds: map[Kind]*jsonschema.Schema{}, runtime: map[string]*jsonschema.Schema{}, api: map[string]*jsonschema.Schema{}}
	for _, u := range urls {
		sch, err := c.Compile(u)
		if err != nil {
			return nil, err
		}
		rel := strings.TrimPrefix(u, baseURL+"v1/")
		name := strings.TrimSuffix(path.Base(rel), ".json")
		switch {
		case rel == "envelope.json":
			s.envelope = sch
		case strings.HasPrefix(rel, "api/"):
			s.api[name] = sch
		case strings.HasPrefix(rel, "runtime/"):
			s.runtime[name] = sch
		case rel != "common.json":
			s.kinds[Kind(name)] = sch
		}
	}
	for _, k := range Kinds {
		if s.kinds[k] == nil {
			return nil, fmt.Errorf("schema: no schema for kind %q", k)
		}
	}
	return s, nil
}

// ValidateEnvelope validates a complete record envelope, including its
// payload against the schema for its kind. raw must already satisfy the
// canonical JSON input policy.
func ValidateEnvelope(raw []byte) error {
	s, err := compiled()
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return invalid("envelope", err)
	}
	if m, ok := inst.(map[string]any); ok {
		if v, ok := m["schema_version"]; ok && fmt.Sprint(v) != fmt.Sprint(Version) {
			return apierr.New("unsupported_schema_version", "schema_version %v is not supported", v)
		}
	}
	if err := s.envelope.Validate(inst); err != nil {
		return invalid("envelope", err)
	}
	m := inst.(map[string]any)
	kind := Kind(m["kind"].(string))
	if err := s.kinds[kind].Validate(m["payload"]); err != nil {
		return invalid(string(kind), err)
	}
	return nil
}

// ValidateRuntime validates a runtime (unsigned, unexported) document.
func ValidateRuntime(name string, raw []byte) error {
	s, err := compiled()
	if err != nil {
		return err
	}
	sch := s.runtime[name]
	if sch == nil {
		return apierr.New("unknown_schema", "no runtime schema %q", name)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return invalid(name, err)
	}
	if err := sch.Validate(inst); err != nil {
		return invalid(name, err)
	}
	return nil
}

func invalid(what string, err error) error {
	return apierr.New("schema_invalid", "%s: %v", what, err)
}

// ValidateAPI validates a versioned API request, response, or tool input.
func ValidateAPI(name string, raw []byte) error {
	s, err := compiled()
	if err != nil {
		return err
	}
	sch := s.api[name]
	if sch == nil {
		return apierr.New("unknown_operation", "no API operation %q", name)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return invalid(name, err)
	}
	if err := sch.Validate(inst); err != nil {
		return invalid(name, err)
	}
	return nil
}
