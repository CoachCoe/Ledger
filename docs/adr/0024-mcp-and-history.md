# ADR 0024: MCP tools, portable history and runtime observations

- Status: Accepted
- Date: 2026-11-08

## Decision

`ledger mcp` uses the official MCP Go SDK v1.8.0 low-level tool API and
stdio. It forwards to `ledgerd` through the same auto-starting client as the
CLI. It holds no signer keys or independent core state. The request schema's
operation enum is the tool inventory. Tools use the operation name, such as
`intent.create`; their arguments are `{operation_id, session_id, input}`.
The adapter supplies API version 1 and the operation name. Read operations
may omit operation_id; writes require a caller-supplied stable id.

The existing API schemas contain relative cross-document references. MCP
clients cannot fetch the `schemas.ledger.invalid` origin. The adapter therefore
bundles their transitive documents under `$defs`, rebases `$ref` pointers and
removes nested `$id` scope boundaries. Validation constraints and reference
siblings are preserved. This refines ADR 0005's unchanged-schema wording:
schemas retain their semantics, rather than byte identity with repository
files. Tool inputs add the shared identity wrapper; outputs are the existing
API v1 response envelope, including api_version. The bundled schemas reach
the client unchanged. Arguments pass canonical JSON policy, bundled input
validation and core API validation; domain and validation errors remain coded
structuredContent with isError, plus the same JSON in text content.

History queries read a single hash-ordered snapshot of committed SQLite
records, which can be rebuilt from portable receipt groups. Pending files and
runtime dependency tables are excluded. Results carry record hashes, kinds
and original payloads. `why` returns all manifests writing the precise symbol
and their intent records, including proposals that were never selected.
`attempts` matches target symbols, or manifest files for a path query. A
never-proposed attempt with no recorded target cannot be invented as a hit.
`dependents` returns written symbols in selected manifests that also read the
target, together with the manifest and selection hashes. Its confidence is
`manifest_cooccurrence`: portable manifests do not retain an exact per-source
call graph. No dynamic dependency claim is made, and missing edges do not
establish safety. Historical facts retain forks rather than choosing a winner
by timestamp. Queries do not assert trust or replace independent verification.
Coverage explicitly names the indexed committed-record scope, receipt members
missing from the records ref, pending records, parse problems, and current
subject/operation conflicts. Independent verification is labeled not_performed;
unknown code history or unindexed facts are not evidence of an empty complete
history. Manifests retain the exact base/tree revision being cited.

Root-only `system.recover`, `system.reindex`, `bundle.export`, `bundle.verify`
and `bundle.import` expose repository maintenance through the same core API.
They reuse the existing recovery and offline verification libraries. Original
standalone bundle report schemas remain stable; `.response.json` schemas wrap
them for the versioned daemon/MCP envelope. Maintenance operations are
idempotent reconciliation actions, not signed logical mutation receipts;
retries rerun reconciliation and may report different work counts. Bundle
paths resolve relative to the served repository. CLI offline verification
remains available without the daemon or root key. Import never grants trust.

`coordination.nearby` refreshes observation, filters active open/proposed
workspaces with live authority, and expires leases. It matches symbol reads,
writes and conservative file fallbacks, or exact normalized file paths.
It returns runtime:true, unsigned:true, observed_at and snapshot_digest. The
digest is SHA-256 of canonical result JSON before adding snapshot_digest.
This is an observation of local mutable state, not a signed portable record
or an atomic repository-wide filesystem snapshot.

## MCP-only editing

The build prompt requires an edit step exclusively through MCP tools; earlier
milestones expose worktrees for an external editor but have no edit operation.
Add `workspace.write`, a small optional `workspace.TextWriter` provider seam,
rather than a shell-execution tool or a model-specific editor. It replaces one
regular UTF-8 text file, bounded to 65,536 Unicode characters (at most 256 KiB).
The caller must be the bound active local session of an open workspace. The
same capability chain governs paths and dependency files. Traversal,
noncanonical path spelling, metadata components and existing symlinks are
rejected; replacement uses os.Root, an exclusive temporary file, fsync and
rename, preserving an existing file's permission bits. An expected SHA-256 is
required for differing existing bytes; null means create. Desired bytes
already present are an idempotent successful observation.

Per-operation unsigned runtime journals retain the request digest and completed
response, without source text or keys. A reused id with different input is an
operation_conflict. Completed retries return the frozen observation without
rewriting later edits, even after the signing key is lost. Pending retries
recheck current authority and expected content; restart never resumes edits
with lost session keys. Runtime journals are local and not exported; durable
proposal receipts remain the record of code changes. Observation and editing
share a workspace lock so normal captures cannot include an in-flight temp
file. An abrupt process/OS failure inside file replacement can leave a
`.ledger-edit-*` temporary file, which is preserved as ordinary workspace
content for inspection; Ledger does not delete an unrecognized file or claim
production sandbox isolation. Direct disk edits can still race cooperative
checks, and proposals independently authorize their frozen tree.

## Acceptance and limits

`internal/mcp` tests a real stdio subprocess with automatic daemon startup,
all tool contracts and coded errors. The nine named `TestAC204_Phase1`
assertions cover scopes, direct-write warnings, overlap, three attempts,
independent exact-tree evidence, ordinary Git branches, queries, an external
provider-free mock agent and offline bundle verification without source Git
or Ledger directories. The example client uses only MCP/JSON APIs for the
create/fork/edit/propose/evidence/select/query lifecycle.

`demos/m6.sh` and `demos/phase1.sh` run those assertions and the history/retry
fixtures. These are engineering acceptance, not commercial evaluation B.1.
Customer evidence C.2/C.3 and release gate R.1 remain separate owner work.
No model inference, provider SDK, hosted service, semantic merge or acceptance
authorization is added.
