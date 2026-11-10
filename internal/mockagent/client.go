// Package mockagent is a deterministic external-agent example. It knows only
// the published contracts, with no core service or model dependency: the
// local root acts through the CLI's JSON output, and each agent session
// through its own session-bound MCP server.
package mockagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Ledger runs the ledger binary in an initialized repository. A nil Env
// inherits the current environment.
type Ledger struct {
	Binary string
	Dir    string
	Env    []string
}

func (l Ledger) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, l.Binary, args...) //nolint:gosec // the caller supplies the Ledger executable, without a shell
	cmd.Dir = l.Dir
	cmd.Env = l.Env
	return cmd
}

// Root runs op as the local root through the CLI and unwraps the durable
// retry result.
func (l Ledger) Root(ctx context.Context, op string, input any) (map[string]any, error) {
	group, verb, ok := strings.Cut(op, ".")
	if !ok {
		return nil, fmt.Errorf("operation %q has no group", op)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	out, err := l.command(ctx, group, verb, string(raw)).Output()
	var reply struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
		Error  *apierr.Error  `json:"error"`
	}
	if e := json.Unmarshal(out, &reply); e != nil {
		return nil, fmt.Errorf("%s: %w (%w)", op, e, err)
	}
	if !reply.OK {
		if reply.Error == nil {
			return nil, fmt.Errorf("%s: failure without an error", op)
		}
		return nil, reply.Error
	}
	return unwrap(reply.Result), nil
}

// Connect starts an MCP server bound to session.
func (l Ledger) Connect(ctx context.Context, session string) (Client, error) {
	s, err := mcp.NewClient(&mcp.Implementation{Name: "ledger-external-mock", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: l.command(ctx, "mcp", "--session", session)}, nil)
	return Client{Session: s}, err
}

// Client is one MCP connection; every call acts as the session it is bound to.
type Client struct{ Session *mcp.ClientSession }

// Call validates the returned envelope, then unwraps the durable retry result.
func (c Client) Call(ctx context.Context, op string, input any) (map[string]any, error) {
	args := map[string]any{"input": input}
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
	return unwrap(envelope.Result), nil
}

func unwrap(result map[string]any) map[string]any {
	if _, ok := result["receipt"]; ok {
		return result["result"].(map[string]any)
	}
	return result
}

// Lifecycle creates an intent, edits/proposes from an author session, runs a
// pinned check from an independent verifier, records selection and queries why.
// The repository must already be initialized with a "unit" check definition.
func (l Ledger) Lifecycle(ctx context.Context) (map[string]any, error) {
	a, err := l.Root(ctx, "session.create", map[string]any{"agent_name": "external-mock", "allow": []string{"mock.ts"}})
	if err != nil {
		return nil, err
	}
	i, err := l.Root(ctx, "intent.create", map[string]any{"description": "Provider-free mock lifecycle", "criteria": []any{map[string]any{"id": "unit", "statement": "fixture check passes", "verification": map[string]any{"kind": "check", "check": "unit"}}}})
	if err != nil {
		return nil, err
	}
	author, err := l.Connect(ctx, a["session_id"].(string))
	if err != nil {
		return nil, err
	}
	defer func() { _ = author.Session.Close() }()
	w, err := author.Call(ctx, "workspace.fork", map[string]any{"intent_id": i["intent_id"], "capability_id": a["capability_id"]})
	if err != nil {
		return nil, err
	}
	if _, err := author.Call(ctx, "workspace.write", map[string]any{"workspace_id": w["workspace_id"], "path": "mock.ts", "content": "export function mock(){return 42;}\n", "expected_hash": nil}); err != nil {
		return nil, err
	}
	p, err := author.Call(ctx, "workspace.propose", map[string]any{"workspace_id": w["workspace_id"]})
	if err != nil {
		return nil, err
	}
	v, err := l.Root(ctx, "session.create", map[string]any{"participant_kind": "service", "agent_name": "independent-mock-verifier"})
	if err != nil {
		return nil, err
	}
	verifier, err := l.Connect(ctx, v["session_id"].(string))
	if err != nil {
		return nil, err
	}
	defer func() { _ = verifier.Session.Close() }()
	status, err := verifier.Call(ctx, "intent.status", map[string]any{"intent_id": i["intent_id"]})
	if err != nil {
		return nil, err
	}
	criteria := status["intent"].(map[string]any)["criteria"].([]any)
	hash := criteria[0].(map[string]any)["verification"].(map[string]any)["definition_hash"]
	e, err := verifier.Call(ctx, "evidence.run", map[string]any{"manifest": p["manifest"], "commit": p["commit"], "check_definition": hash})
	if err != nil {
		return nil, err
	}
	d, err := l.Root(ctx, "selection.record", map[string]any{"manifest": p["manifest"], "commit": p["commit"], "evidence": []any{e["attestation"]}})
	if err != nil {
		return nil, err
	}
	why, err := author.Call(ctx, "history.why", map[string]any{"symbol": map[string]any{"path": "mock.ts", "language": "typescript", "name": "mock", "kind": "function", "disambiguator": nil}})
	if err != nil {
		return nil, err
	}
	return map[string]any{"proposal": p, "evidence": e, "selection": d, "why": why}, nil
}
