# Agent integration (API v1)

Run `ledger mcp --session ses_…` with the initialized repository as its
working directory. It speaks MCP over stdin/stdout; diagnostics go to stderr.
EOF closes the adapter; the repository daemon remains available for other
clients. The adapter starts `ledgerd` on demand, sharing the CLI's socket and
signing state. Only one repository is served by each process. No network
listener is opened.

Each adapter is bound to one session, and every tool call acts as that
session. `--session` is required: there is no root mode over MCP. The local
root creates the session with the CLI (`ledger session create`) and gives the
agent an adapter bound to it. Root-only operations (`session.create`,
`session.recover`, `capability.issue`, `speculation.start`/`evaluate`/`score`/`cancel`/`resume`,
`system.recover`/`reindex`, `bundle.*` and `daemon.stop`) return
`unauthorized` over MCP; run them with the CLI.

Example client configuration (substitute an absolute binary path and the
session id, and use the MCP host's working-directory setting):

```json
{"mcpServers":{"ledger":{"command":"/absolute/path/to/ledger","args":["mcp","--session","ses_0123456789abcdefghijklmnop"]}}}
```

Use tools/list to discover the complete operation inventory and self-contained
JSON Schema input/output contracts. A tool call has this shape:

```json
{"name":"workspace.propose","arguments":{"operation_id":"op_0123456789abcdefghijklmnop","input":{"workspace_id":"wsp_0123456789abcdefghijklmnop"}}}
```

These are illustrative identifiers; allocate fresh random 128-bit lowercase
base32 ids for real operations. For durable mutations retain the exact
operation_id and arguments until success, and retry them unchanged after a
disconnect. A changed request needs a new id. Read tools may omit operation_id.
Arguments have no session_id: the adapter supplies its bound session, and a
call that names one is rejected with schema_invalid. This local cooperative
interface is not an untrusted-agent security sandbox: an agent with shell
access can still run the CLI, which acts as root. A capability's scope is
checked on frozen proposal files; direct disk access remains possible.

Tool results carry `api_version:1` and either `{ok:true,result:…}` or
`{ok:false,error:{code,message,details?}}`. The JSON appears as structuredContent
and as text content. Failures set isError; unknown tools are MCP protocol
errors. Durable mutations wrap their result with operation_id, receipt,
members and replayed. Inspect the coded failure instead of parsing prose.
An operation can have committed despite a cleanup error; retain any
committed_result or committed flag and resolve status before changing inputs.
Cancelling evidence.run disconnects its check and records a failed result
when the verifier key is available. Speculation evaluation continues after
client disconnect; use speculation.cancel explicitly.

For an ordinary agent lifecycle:

1. The local root creates a scoped session and an intent with the CLI, and
   starts an adapter bound to the session. workspace.fork returns a detached
   fixed-base worktree and a workspace id.
2. Edit with an external editor or workspace.write. The latter accepts
   workspace_id, path, content and expected_hash (null to create, SHA-256 of
   current bytes to replace). It supports regular text files only, and returns
   an unsigned runtime observation, not an attestation. A completed same-id
   retry returns its original response; a pending edit needs active authority.
3. workspace.propose freezes code and publishes a normal Git branch, manifest
   hash and commit. Do not rewrite the proposal to attach later evidence.
4. A distinct verifier session, through its own adapter, calls evidence.run using manifest, commit and
   the intent's pinned check-definition hash. evidence.isIndependent reports
   the verifier relationship; the execution isolation field remains "none".
5. The local root (through the CLI) or another authorized selector calls selection.record with
   exact manifest/commit and explicit evidence hashes. Selection never merges.

For parallel strategies, the root's `ledger speculation start` returns scoped
session/workspace handoffs. External agents, each through an adapter bound to
its attempt session, edit those worktrees, then speculation.submit freezes
each candidate. The root evaluates, supplies scores if requested, and inspects
the retained winner and signed loser attempt records. Ledger runs no model.

`history.why` and `history.dependents` take a full symbol locator (`path`,
`language`, `name`, `kind`, `disambiguator`). `history.attempts` and
`coordination.nearby` take exactly one of symbol or path. Paths refer to exact
repository-relative files; they are not globs or directory searches. History
returns portable record hashes and payloads. Dependents are advisory manifest
co-occurrences, not exact or complete call graphs. Nearby reports live reads,
writes, conservative file fallbacks and leases, including observation time.
Check runtime:true and unsigned:true; verify snapshot_digest by removing that
field, canonicalizing the remaining JSON and taking SHA-256. A snapshot digest
does not confer durable provenance, identity trust or atomic filesystem capture.

See [the mock client](../../examples/mock-agent/main.go) and
[its lifecycle](../../internal/mockagent/client.go). Build it from the Ledger
source with `go build -o /tmp/ledger-mock ./examples/mock-agent`, then run
`/tmp/ledger-mock /absolute/path/to/ledger` in a disposable initialized repository
with a passing `unit` check in `.ledger/checks.json`. It creates mock.ts, runs
evidence, selects and queries: root steps through the CLI's JSON output, and
the author and verifier each through their own session-bound adapter. `demos/phase1.sh` supplies the
complete polyglot fixture and verifies all nine engineering assertions.

Repository maintenance (system.recover/reindex, bundle.export/verify/import)
is root-only and so is not available over MCP; use `ledger recover`,
`reindex`, `export`, `verify` and `import`. History coverage reports missing receipt members, pending records and
record parsing problems from the records ref, plus current indexed conflicts.
It explicitly says independent verification was not performed; use `ledger verify`
for integrity, trust and deeper unavailable-provenance findings.
