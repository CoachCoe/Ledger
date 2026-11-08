// Package mockagent is a deterministic external-agent example. It knows only
// the published MCP/JSON contract, with no core service or model dependency.
package mockagent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

type Client struct{ Session *mcp.ClientSession }

// Call validates the returned envelope, then unwraps the durable retry result.
func (c Client) Call(ctx context.Context, op, sid string, input any) (map[string]any, error) {
	args := map[string]any{"session_id": sid, "input": input}
	if !api.ReadOnly(op) {
		args["operation_id"] = schema.NewID(schema.PrefixOperation)
	}
	r, err := c.Session.CallTool(ctx, &mcp.CallToolParams{Name: op, Arguments: args})
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(r.StructuredContent)
	if err != nil {
		return nil, err
	}
	if err := api.ValidateResponse(op, b); err != nil {
		return nil, fmt.Errorf("%s response: %w", op, err)
	}
	var envelope struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
		Error  *apierr.Error  `json:"error"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return nil, err
	}
	if !envelope.OK {
		return nil, envelope.Error
	}
	if r.IsError {
		return nil, fmt.Errorf("%s: success marked as error", op)
	}
	if _, ok := envelope.Result["receipt"]; ok {
		return envelope.Result["result"].(map[string]any), nil
	}
	return envelope.Result, nil
}

// Lifecycle creates an intent, edits/proposes from an author session, runs a
// pinned check from an independent verifier, records selection and queries why.
// The repository must already be initialized with a "unit" check definition.
func (c Client) Lifecycle(ctx context.Context) (map[string]any, error) {
	a, err := c.Call(ctx, "session.create", "", map[string]any{"agent_name": "external-mock", "allow": []string{"mock.ts"}})
	if err != nil {
		return nil, err
	}
	sid := a["session_id"].(string)
	i, err := c.Call(ctx, "intent.create", "", map[string]any{"description": "Provider-free mock lifecycle", "criteria": []any{map[string]any{"id": "unit", "statement": "fixture check passes", "verification": map[string]any{"kind": "check", "check": "unit"}}}})
	if err != nil {
		return nil, err
	}
	w, err := c.Call(ctx, "workspace.fork", sid, map[string]any{"intent_id": i["intent_id"], "capability_id": a["capability_id"]})
	if err != nil {
		return nil, err
	}
	if _, err := c.Call(ctx, "workspace.write", sid, map[string]any{"workspace_id": w["workspace_id"], "path": "mock.ts", "content": "export function mock(){return 42;}\n", "expected_hash": nil}); err != nil {
		return nil, err
	}
	p, err := c.Call(ctx, "workspace.propose", sid, map[string]any{"workspace_id": w["workspace_id"]})
	if err != nil {
		return nil, err
	}
	v, err := c.Call(ctx, "session.create", "", map[string]any{"participant_kind": "service", "agent_name": "independent-mock-verifier"})
	if err != nil {
		return nil, err
	}
	status, err := c.Call(ctx, "intent.status", "", map[string]any{"intent_id": i["intent_id"]})
	if err != nil {
		return nil, err
	}
	criteria := status["intent"].(map[string]any)["criteria"].([]any)
	hash := criteria[0].(map[string]any)["verification"].(map[string]any)["definition_hash"]
	e, err := c.Call(ctx, "evidence.run", v["session_id"].(string), map[string]any{"manifest": p["manifest"], "commit": p["commit"], "check_definition": hash})
	if err != nil {
		return nil, err
	}
	d, err := c.Call(ctx, "selection.record", "", map[string]any{"manifest": p["manifest"], "commit": p["commit"], "evidence": []any{e["attestation"]}})
	if err != nil {
		return nil, err
	}
	why, err := c.Call(ctx, "history.why", "", map[string]any{"symbol": map[string]any{"path": "mock.ts", "language": "typescript", "name": "mock", "kind": "function", "disambiguator": nil}})
	if err != nil {
		return nil, err
	}
	return map[string]any{"proposal": p, "evidence": e, "selection": d, "why": why}, nil
}
