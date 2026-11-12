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

// Current is the envelope schema_version this build writes; Versions lists
// every one it reads.
const Current = 1

var Versions = []int{1, 2}

const baseURL = "https://schemas.ledger.invalid/"

var compiled = sync.OnceValues(compileAll)

type schemaSet struct {
	envelope map[int]*jsonschema.Schema
	kinds    map[int]map[Kind]*jsonschema.Schema
	runtime  map[string]*jsonschema.Schema
	api      map[int]map[string]*jsonschema.Schema
	bundle   *jsonschema.Schema
}

func compileAll() (*schemaSet, error) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	var urls []string
	err := fs.WalkDir(schemas.FS, ".", func(p string, d fs.DirEntry, err error) error {
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
	s := &schemaSet{envelope: map[int]*jsonschema.Schema{}, kinds: map[int]map[Kind]*jsonschema.Schema{1: {}, 2: {}}, runtime: map[string]*jsonschema.Schema{}, api: map[int]map[string]*jsonschema.Schema{1: {}, 2: {}}}
	for _, u := range urls {
		sch, err := c.Compile(u)
		if err != nil {
			return nil, err
		}
		dir, rel, _ := strings.Cut(strings.TrimPrefix(u, baseURL), "/")
		version := 1
		if dir == "v2" {
			version = 2
		}
		name := strings.TrimSuffix(path.Base(rel), ".json")
		switch {
		case rel == "envelope.json":
			s.envelope[version] = sch
		case rel == "bundle_inventory.json":
			s.bundle = sch
		case strings.HasPrefix(rel, "api/"):
			s.api[version][name] = sch
		case strings.HasPrefix(rel, "runtime/") && version == 1:
			s.runtime[name] = sch
		case rel != "common.json":
			s.kinds[version][Kind(name)] = sch
		}
	}
	for _, v := range Versions {
		if s.envelope[v] == nil {
			return nil, fmt.Errorf("schema: no envelope schema for version %d", v)
		}
		for _, k := range KindsFor(v) {
			if s.kinds[v][k] == nil {
				return nil, fmt.Errorf("schema: no version %d schema for kind %q", v, k)
			}
		}
		if len(s.kinds[v]) != len(KindsFor(v)) {
			return nil, fmt.Errorf("schema: version %d has schemas for kinds this build does not list", v)
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
	version := 1
	if m, ok := inst.(map[string]any); ok {
		if v, ok := m["schema_version"]; ok {
			switch fmt.Sprint(v) {
			case "1":
			case "2":
				version = 2
			default:
				return apierr.New("unsupported_schema_version", "schema_version %v is not supported", v)
			}
		}
	}
	if err := s.envelope[version].Validate(inst); err != nil {
		return invalid("envelope", err)
	}
	m := inst.(map[string]any)
	kind := Kind(m["kind"].(string))
	if err := s.kinds[version][kind].Validate(m["payload"]); err != nil {
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

// APIVersions lists the api_version values this build serves.
var APIVersions = []int{1, 2}

// ValidateAPI validates an api_version 1 request, response, or tool input.
func ValidateAPI(name string, raw []byte) error { return ValidateAPIVersion(1, name, raw) }

// ValidateAPIVersion validates a request, response, or tool input of one
// api_version.
func ValidateAPIVersion(version int, name string, raw []byte) error {
	s, err := compiled()
	if err != nil {
		return err
	}
	if s.api[version] == nil {
		return apierr.New("unsupported_api_version", "api_version %d is not supported", version)
	}
	sch := s.api[version][name]
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

// BundleVersion is the only bundle inventory version this build reads.
const BundleVersion = 1

// ValidateBundle validates a bundle inventory document. An inventory whose
// bundle_version is not BundleVersion fails with unsupported_bundle_version
// before any other interpretation.
func ValidateBundle(raw []byte) error {
	s, err := compiled()
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return invalid("bundle_inventory", err)
	}
	m, ok := inst.(map[string]any)
	if !ok {
		return invalid("bundle_inventory", fmt.Errorf("not an object"))
	}
	if v, ok := m["bundle_version"]; !ok || fmt.Sprint(v) != fmt.Sprint(BundleVersion) {
		return apierr.New("unsupported_bundle_version", "bundle_version %v is not supported", v)
	}
	if err := s.bundle.Validate(inst); err != nil {
		return invalid("bundle_inventory", err)
	}
	return nil
}
