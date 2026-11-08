# ADR 0022: Symbol coordination and advisory leases

Status: implemented. Extends ADR 0014.

## Owner continuation

On 2026-11-07 the project owner requested M4 unattended after the M3 review and fixes. This authorizes engineering continuation past the previous stop-before-M4 instruction. It does not assert that customer conversations occurred or complete the separate C.2/C.3 evidence gates.

## Design

Use the official tree-sitter Go binding and JavaScript, TypeScript/TSX and Python grammars. Symbol identity is the normalized path, language, lexical name, kind and optional disambiguator; content revisions are separate. Ambiguous declarations, parser failures and unsupported files produce labeled uncertainty and file-level overlap. Reads describe static references from changed code and declared context, never actual agent reading.

Analyze exact Git blobs for observations and frozen proposals. Imports resolve only within the repository; unresolved or dynamic references never establish independence. Runtime analyses, dependency edges, notifications and leases are local unsigned state, excluded from exported records. Immutable manifests carry derived symbol locators.

Overlap and lease warnings are symmetric, deduplicated and recipient-owned. Leases are advisory, expire automatically and are removed after session loss; no portable history revives them. Reindex discards runtime delivery state and observation regenerates current warnings.

The blob cache is bounded to 4096 entries and source parsing to 4 MiB per blob. Git tree metadata rejects oversized sources before their contents are fetched; remaining blob reads are batched at 8 MiB and 128 entries. Unchanged gitlinks do not prevent analysis of other files. Paths that normalize to the same locator produce a file-level ambiguity warning.

Locators with duplicate names retain ordinal disambiguators plus a file-level ambiguity warning. Declaration ownership and direct-child masking follow the syntax tree, avoiding repeated scans of every declaration. Changing one method does not write every other method. Lexical parameters, local variables, destructuring and enclosing closures prevent shadowed names from resolving to imports; type annotations and default expressions remain references. Surviving declarations contribute the candidate's reads, while deleted declarations retain their base references. There is no dynamic observation or complete dependency claim.

Lease scopes are literal authorized paths or symbol locators, with at most 24 hours of lifetime. Runtime operation ids deterministically identify retries; runtime state is intentionally discarded by reindex. Failed enqueue/ack/lease boundaries are retryable, including faults immediately after the SQLite commit.

M4 tightens notification polling to return only unacknowledged items, matching the stated contract. M2 previously returned acknowledged items with their timestamp. Stored acknowledgments and idempotent single-item ack remain; M4 also adds atomic batch acknowledgment. Existing M2 tests and CLI assertions now check suppressed redelivery and persisted acknowledgment separately.

Pending queues are filtered in SQLite through a partial index, so polling does not load the recipient's acknowledged history.

## Validation

`symbols.TestAC007_020_181_221_Analyzers`, `TestAC117_LocatorEditsAndAmbiguity`, `coord.TestAC003_184_OverlapTruthTable`, the M4 tests in `internal/core/coordination_test.go`, `daemon.TestAC187_WatcherNotificationsWithinFiveSeconds`, and `demos/m4.sh` cover syntax/mapping/dependencies, frozen manifests, symmetric notifications, restart/reindex and lease/queue fault boundaries. The real watcher test polls as root, so polling cannot itself refresh a session's workspace.

`make test` serializes package execution (`-p 1`), keeping unrelated process-heavy fixture suites from competing with observer deadline assertions. The race detector and concurrency within each test remain enabled; the two- and five-second assertions are unchanged.

Quality-review regressions in `internal/symbols/*review_test.go` and `internal/core/coordination_review_test.go` cover shadowed imports, type/default references, removed dependencies, normalization collisions, oversized sources and unchanged submodules. See [the analyzer benchmark](../benchmarks/m4-symbols.md).
