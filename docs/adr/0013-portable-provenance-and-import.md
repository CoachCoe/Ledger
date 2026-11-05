# ADR 0013: Portable provenance closure and staged import

- Status: Accepted
- Date: 2026-11-05

## Context

D0 records the revised Phase 1 build prompt's execution contracts before M0. Git remains canonical for code; signed records govern provenance. This decision specifies the existing local Phase 1 scope.

## Decision

A versioned bundle inventory maps stable ids to exact authenticated creation hashes and lists object hashes, referenced public keys, receipts, and transitive provenance dependencies. Include lifecycle histories/bindings necessary to determine operation-time authority and committed state, not just a manifest's direct references. Include complete receipt member groups when one member is referenced. A receipt lists members but members never hash their receipt, so closure adds groups without introducing content-hash cycles.

Also include a self-contained Git bundle of bound commits and their reachable parent/tree/blob closure, records ref and proposal retention refs. No thin prerequisites in the original repository may be needed. Report size honestly; optimize only after correctness. SQLite, live worktree locations, queues, leases, journals, private keys, and local trust lists are excluded. Public session records are history, not restorable live sessions.

Verification uses available records/Git objects with no daemon, SQLite, source repository or network. Check envelope/schema/hash/signature, unique entity resolution, complete receipts, authorization at operation time, lifecycle/binding predecessors/conflicts, and each commit-binding condition. Integrity is verified/invalid/unavailable; trust is trusted/untrusted/unknown. Valid untrusted signatures can have verified integrity. Missing references/commits are unavailable, not invalid or verified. Known mismatches are invalid; expose component results if other dependencies are also missing. Semantic conflicts remain explicitly reported and prevent a clean accepted-state claim. Trust is unknown until integrity is established. Trust requires local explicit root configuration or `--trust`; bundle keys never grant it.

Import validates paths, bounded declared/actual sizes, supported schemas, hashes/signatures, reference closure and Git bindings in staging **before** publishing through RecordStore. Reject absolute/traversal/duplicate-entry/alias paths and symlink extraction escapes. Inspect Git data without executing checks/hooks; never overwrite code branches or import config/private keys/trust/runtime state. Establish inert staging before reading untrusted bundle Git objects. Partial bundles may be inspected as unavailable but cannot be reported as complete verified imports.

Duplicate import is idempotent. Semantic forks/incompatible creations/receipts are preserved and surfaced, blocking affected subjects. Ref updates use CAS/union semantics; recovery preserves externally advanced refs. Bundle import tests use a fresh repo after source `.ledger/` **and** source Git objects become inaccessible; verifying while the source is present does not prove portability.

## Alternatives considered

Records plus public keys alone cannot verify a commit/tree binding. Importing trust or runtime sessions would cross local authority boundaries. Blind archive extraction permits path traversal.

## Consequences and validation

M3.6: offline source-inaccessible verify/import twice, missing records/commits, traversal/oversize/unsupported schema, tamper, conflicting histories, trust separation, no branch/config/key/runtime overwrite.

These are acceptance obligations, not claims that product code already implements them. The untracked milestone plans assign each case to its implementation step.
