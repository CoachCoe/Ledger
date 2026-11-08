# ADR 0005: MCP adapter on the official Go SDK's low-level tool API

- Status: Accepted
- Date: 2026-11-05

M6 implementation refinement: [ADR 0024](0024-mcp-and-history.md) specifies
self-contained reference bundling, identity wrappers and the API v1 envelope.

## Context

MCP is one transport over Ledger's versioned core API. Tool inputs and outputs must use Ledger's own JSON Schemas, and failures must reach agents as structured, coded errors they can act on, not prose.

## Decision

- Use `github.com/modelcontextprotocol/go-sdk` (tested at v1.8.0), stdio transport, exposed as `ledger mcp`.
- Register tools with the low-level `Server.AddTool`, not the generic `mcp.AddTool`. Each tool's `InputSchema` and `OutputSchema` are Ledger's schema documents, passed through unchanged as `json.RawMessage`.
- Ledger validates arguments itself with `github.com/santhosh-tekuri/jsonschema/v6`, the same validator used for records.
- Every tool result is an envelope, either `{"ok": true, "result": …}` or `{"ok": false, "error": {"code", "message", "details"}}`. It is returned as `structuredContent`, with a JSON text copy in `content`. `isError` is set on failures. The output schema is a `oneOf` over the two envelope shapes.
- Unknown tools and transport failures remain MCP protocol errors.
- Schemas use JSON Schema draft 2020-12, and each has a stable `$id`. Without an `$id`, validation messages include local file paths.

## Alternatives considered

- **The SDK's generic `mcp.AddTool`.** It infers schemas from Go types and validates automatically, but a returned error reaches the client only as plain text (`{"content":[{"type":"text","text":"unknown_check"}],"isError":true}`), which loses the error code and details. Its validator also supports only draft 2020-12.

## Evidence (Stage 0 spike S3)

- Over both in-memory and stdio subprocess transports, the client received Ledger's input and output schemas unchanged, including `$defs`, `$ref`, `oneOf`, `const`, and `additionalProperties: false`.
- A success, a schema violation with all failing fields listed, an unexpected extra field, and a domain error (`unknown_check`) all arrived as structured content that validates against the output schema.
- Calling an unknown tool returned a protocol error.

## Consequences

- MCP tool definitions are generated from the API schemas, so the MCP surface, the CLI, and the core API cannot drift apart.
- Ledger owns input validation, so the core API and the MCP adapter apply identical rules.
