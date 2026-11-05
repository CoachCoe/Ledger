# ADR 0001: Go as the implementation language

- Status: Accepted
- Date: 2026-11-05

## Context

Ledger Phase 1 is a local control plane made of a core library, a per-repository background process (`ledgerd`), a CLI, and an MCP server. It must sign and hash records, run concurrent workspace observers, parse JavaScript, TypeScript, and Python, speak MCP, and install on developer machines as infrastructure software.

## Decision

Implement Ledger in Go (toolchain 1.27 or later), shipped as a single `ledger` binary; `ledgerd` runs as `ledger daemon`. cgo is used only for tree-sitter parsing and, on macOS, FSEvents file watching.

Supported Phase 1 platforms are macOS (arm64, amd64) and Linux (arm64, amd64).

## Alternatives considered

- **TypeScript on Node.js.** Best ecosystem for the MCP SDK, tree-sitter (WebAssembly build, no native toolchain), and JSON Schema type generation. Rejected because distributing a daemon to customer machines then requires a Node runtime or a bundled single-executable build, and the security-critical core benefits from Go's standard-library cryptography, race detector, and native fuzzing.
- **Rust.** Strong native tree-sitter and file-watching support. Rejected for Phase 1 on development speed; it offers little over Go for this workload.

## Evidence (Stage 0 spikes)

Each risk that could have reversed this decision was measured before any product code:

| Risk | Result | ADR |
|---|---|---|
| tree-sitter under cgo | JS, TS, TSX, and Python declarations and edit-to-symbol mapping work; binaries build and run on macOS arm64/amd64 and Linux arm64/amd64 | [0006](0006-builds-and-symbol-parsing.md) |
| File watching on macOS | FSEvents meets the 2-second target with 10 worktrees; `fsnotify` (kqueue) does not | [0003](0003-workspace-observation.md) |
| MCP Go SDK | Carries Ledger-authored schemas and structured errors over stdio | [0005](0005-mcp-adapter.md) |
| Canonical JSON | The standard library implements RFC 8785 and matches independent implementations | [0002](0002-canonical-json.md) |
| Git primitives | All required behaviors confirmed on Git 2.25 through 2.54 | [0004](0004-git-usage.md) |

## Consequences

- Release builds need a C toolchain per target platform; Linux release binaries are statically linked (ADR 0006).
- JSON Schemas are the source of truth for records and API messages; Go types are hand-written and checked against the schemas by round-trip tests rather than generated.
