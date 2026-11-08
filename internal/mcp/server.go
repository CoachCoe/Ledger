// Package mcp adapts Ledger's versioned core protocol to the official MCP SDK.
// It owns no state or signing keys; production calls use the repository daemon.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
)

type Caller func(context.Context, api.Request) (any, error)

func New(call Caller) (*mcp.Server, error) {
	ops, err := api.Operations()
	if err != nil {
		return nil, err
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "ledger", Version: "1"}, nil)
	for _, op := range ops {
		in, out, err := api.ToolSchemas(op)
		if err != nil {
			return nil, err
		}
		compiler := jsonschema.NewCompiler()
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(in))
		if err != nil {
			return nil, err
		}
		id := "https://schemas.ledger.invalid/v1/api/" + op + ".tool.json"
		if err := compiler.AddResource(id, doc); err != nil {
			return nil, err
		}
		input, err := compiler.Compile(id)
		if err != nil {
			return nil, err
		}
		s.AddTool(&mcp.Tool{Name: op, Description: "Ledger API v1: " + op + ". Empty session_id acts as local root. Mutations require a stable operation_id for retries.", InputSchema: in, OutputSchema: out}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			raw, err := canon.Canonical(req.Params.Arguments)
			if err != nil {
				err = apierr.New("invalid_json", "%v", err)
			}
			if err == nil {
				v, e := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				err = e
				if err == nil {
					err = input.Validate(v)
				}
				if err != nil {
					err = apierr.New("schema_invalid", "%v", err)
				}
			}
			var args map[string]json.RawMessage
			if err == nil {
				err = json.Unmarshal(raw, &args)
			}
			var result any
			if err == nil {
				// Preserve unknown members for core request validation.
				args["api_version"] = json.RawMessage("1")
				args["operation"], _ = json.Marshal(op)
				if _, ok := args["operation_id"]; !ok && api.ReadOnly(op) {
					args["operation_id"] = json.RawMessage(`""`)
				}
				raw, err = json.Marshal(args)
				if err == nil {
					var r api.Request
					r, err = api.Parse(raw)
					if err == nil {
						result, err = call(ctx, r)
					}
				}
			}
			body := api.Reply(result, err)
			b, encodeErr := canon.Marshal(body)
			if encodeErr != nil {
				return nil, encodeErr
			}
			return &mcp.CallToolResult{IsError: !body.OK, StructuredContent: json.RawMessage(b), Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
		})
	}
	return s, nil
}
