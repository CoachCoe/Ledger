package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func TestAC006_026_197_ToolContractsAndStructuredErrors(t *testing.T) {
	ctx := context.Background()
	calls := 0
	s, err := New(func(_ context.Context, r api.Request) (any, error) {
		calls++
		if r.Version != 1 || r.Operation != "intent.create" {
			t.Fatal(r)
		}
		return nil, apierr.New("intent_not_leaf", "example domain error").With("intent_id", "example")
	})
	if err != nil {
		t.Fatal(err)
	}
	st, ct := sdk.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != len(ops) {
		t.Fatalf("tools=%d operations=%d", len(tools.Tools), len(ops))
	}
	for _, tool := range tools.Tools {
		in, out, err := api.ToolSchemas(tool.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, pair := range [][2]any{{in, tool.InputSchema}, {out, tool.OutputSchema}} {
			a, _ := json.Marshal(pair[0])
			b, _ := json.Marshal(pair[1])
			var x, y any
			_ = json.Unmarshal(a, &x)
			_ = json.Unmarshal(b, &y)
			a, _ = json.Marshal(x)
			b, _ = json.Marshal(y)
			if string(a) != string(b) {
				t.Fatalf("%s schema changed in transit", tool.Name)
			}
		}
		compiler := jsonschema.NewCompiler()
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		id := "https://schemas.ledger.invalid/v1/api/" + tool.Name + ".tool-output.json"
		if err := compiler.AddResource(id, doc); err != nil {
			t.Fatal(err)
		}
		if _, err := compiler.Compile(id); err != nil {
			t.Fatalf("%s output is not self-contained: %v", tool.Name, err)
		}
	}
	good := map[string]any{"session_id": "", "operation_id": schema.NewID(schema.PrefixOperation), "input": map[string]any{"description": "test", "criteria": []any{map[string]any{"id": "review", "statement": "review", "verification": map[string]any{"kind": "judgment", "reviewer": "human"}}}}}
	for _, tc := range []struct {
		args any
		code string
	}{
		{nil, "schema_invalid"},
		{map[string]any{"session_id": "", "input": map[string]any{}}, "schema_invalid"},
		{map[string]any{"session_id": "", "operation_id": schema.NewID(schema.PrefixOperation), "input": map[string]any{"surprise": 1}}, "schema_invalid"},
		{good, "intent_not_leaf"},
		{json.RawMessage(`{"session_id":"","session_id":"","input":{}}`), "invalid_json"},
		{map[string]any{"api_version": 2, "session_id": "", "operation_id": schema.NewID(schema.PrefixOperation), "input": good["input"]}, "schema_invalid"},
	} {
		r, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "intent.create", Arguments: tc.args})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(r.StructuredContent)
		if err := schema.ValidateAPI("intent.create.output", b); err != nil {
			t.Fatal(err)
		}
		var out api.Response
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		if !r.IsError || out.Error == nil || out.Error.Code != tc.code {
			t.Fatalf("want %s: %s", tc.code, b)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid requests reached core: %d", calls)
	}
	if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "unknown", Arguments: map[string]any{}}); err == nil {
		t.Fatal("unknown tool must be protocol error")
	}
}
