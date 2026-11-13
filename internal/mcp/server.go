// Package mcp adapts Ledger's versioned core protocol to the official MCP SDK.
// It owns no state or signing keys; production calls use the repository daemon.
// Each server is bound to one session and signs nothing as root.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
)

type Caller func(context.Context, api.Request) (any, error)

var sessionID = regexp.MustCompile(`^ses_[0-9a-v]{26}$`)

// New serves every operation of api_version as a tool acting as session. An
// empty session is refused: the daemon treats it as the local root.
func New(session string, version int, call Caller) (*mcp.Server, error) {
	if !sessionID.MatchString(session) {
		return nil, apierr.New("usage", "ledger mcp requires --session with a session id; create one with ledger session create")
	}
	sid, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}
	ops, err := api.Operations(version)
	if err != nil {
		return nil, err
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "ledger", Version: strconv.Itoa(version)}, nil)
	for _, op := range ops {
		in, out, err := api.ToolSchemas(version, op)
		if err != nil {
			return nil, err
		}
		compiler := jsonschema.NewCompiler()
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(in))
		if err != nil {
			return nil, err
		}
		id := fmt.Sprintf("https://schemas.ledger.invalid/v%d/api/%s.tool.json", version, op)
		if err := compiler.AddResource(id, doc); err != nil {
			return nil, err
		}
		input, err := compiler.Compile(id)
		if err != nil {
			return nil, err
		}
		s.AddTool(&mcp.Tool{Name: op, Description: fmt.Sprintf("Ledger API v%d: %s, acting as session %s. Mutations require a stable operation_id for retries.", version, op, session), InputSchema: in, OutputSchema: out}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
				args["api_version"] = json.RawMessage(strconv.Itoa(version))
				args["operation"], _ = json.Marshal(op)
				args["session_id"] = sid
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
			body := api.ReplyVersion(version, result, err)
			b, encodeErr := canon.Marshal(body)
			if encodeErr != nil {
				return nil, encodeErr
			}
			return &mcp.CallToolResult{IsError: !body.OK, StructuredContent: json.RawMessage(b), Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
		})
	}
	return s, nil
}
