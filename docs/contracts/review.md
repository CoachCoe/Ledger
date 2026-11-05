# D0 review of the existing implementation

Reviewed on 2026-11-05: all four source documents in the ignored working-doc directory, ADRs 0001–0006, every Go source/test and shell script in `spikes/`, fixture source files, and module dependency declarations. The repository had no commits or product implementation. Spike binaries were not treated as source or independent release evidence.

## Findings and fixes

| Finding | Change | Verification |
|---|---|---|
| Ten observers append samples to one shared latency slice without a shared lock; ordinary package tests did not execute this path | Synchronize collection/reporting; add concurrent sample-count regression | `go test -race ./...`; historical throughput must be remeasured before M1 |
| Git scripts accept existing scratch directories and the stress script removes matching subdirectories | Require empty canonical absolute scratch roots; remove the unnecessary recursive deletion; use dedicated temporary Git config instead of repurposing HOME | Git behavior script: 26 passes on Apple Git 2.54; nonempty-root rejection; shell syntax check |
| Canonicalization reference tests silently skip missing fixtures; scanner/read failures and extra reference output could escape checking | Explicit scratch configuration makes missing fixtures fatal; check reads/scanner/response count; fuzz compares decoded structures directly | Safe-input tests and bounded native fuzzing; independent scratch inputs remain unavailable in this checkout |
| Edit-to-symbol mapping accepts reversed ranges and an insertion at a declaration's exclusive end | Reject reversed/outside half-open ranges; add boundary cases | Symbol tests under race detector |

Spike code remains intentionally ignored and disposable. Product code must promote only the needed behavior with permanent fixtures and meaningful tests. No production security or cross-platform acceptance is implied by these fixes.

## D0 deliverables

- ADRs 0007–0015 specify envelopes/signing, identifiers, authenticated sessions/actors, receipt visibility and retry/conflicts, proposals/retention, lifecycle/recovery, evidence/selection, portable import, symbols and speculation.
- `reference-graph.json` and its checker demonstrate an acyclic content-reference graph, stable-id resolution, complete receipt membership, pending visibility and predecessor replay. They are symbolic, not cryptographic/schema fixtures.
- `recovery.md` assigns before/after interruption tests for every external side effect.
- Ignored working plans map 251 bullet/numbered/prose requirements to steps and planned tests, with separate customer, benchmark and release gates. Passing tests remain mandatory at implementation time.

## Remaining implementation obligations

M0 must establish the module/CI, pinned dependencies, mandatory independent JCS vectors/signature fixtures, all schemas, secure root-key creation, durable RecordStore and replay/index tests. In particular, neither per-file atomic writes nor ref union alone establishes accepted multi-record lifecycle state: only validated complete receipt groups do. M1 must repeat platform watcher benchmarks with the fixed collector and implement real authenticated sessions before workspaces. The post-M3 customer checkpoint is unchanged.
