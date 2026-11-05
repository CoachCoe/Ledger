# ADR 0009: Operation receipts, durable visibility, and conflicts

- Status: Accepted
- Date: 2026-11-05

## Context

D0 records the revised Phase 1 build prompt's execution contracts before M0. Git remains canonical for code; signed records govern provenance. This decision specifies the existing local Phase 1 scope.

## Decision

Every mutating API carries an operation id and a frozen canonical request digest. The digest covers the versioned operation name and complete effective input, including resolved bindings/defaults; the retry path checks the original request identity before resolving mutable defaults again. Same id/same request returns its original result; different request is rejected. M1 API schemas define the precise request representation; M0 storage accepts an already frozen digest.

Keep the caller request identity and the frozen effective input in the local journal: comparing a retry must not re-resolve a moving change id or a newly edited default. Public receipt results return the frozen bindings. A request digest alone does not authorize any claimed result or replace its referenced evidence.

A signed receipt contains operation id, request digest, result, and the exact hashes of the operation's **other** records. It excludes its own hash. Result data cannot embed the receipt's hash; response metadata may return that hash alongside it. Every accepted non-receipt object belongs to a complete authenticated receipt group. Receipt validation checks all listed records, signer/actor authority, references, predecessor constraints, and the result bindings. A receipt cannot grant authority absent from its members' signer chains. Concurrent incompatible receipts for an operation id or incompatible creations/events remain explicit conflicts, even if signatures individually verify.

The receipt also records the claimed operation time used for authorization, including records such as manifests that have no independent timestamp. This time is signed and frozen with the request/result; a retry cannot refresh it to extend authority. Member lifecycle/execution timestamps must be consistent with the operation they describe. This supplies an auditable local time claim, not trusted chronology.

Receipt publication is the visibility boundary. Pending objects are durable bytes, not accepted lifecycle state. A receipt file is only a complete committed group when all members and dependencies are present and valid; mere file presence or a SQLite row is insufficient. Objects/receipts on disk and in the records ref are reconciled copies of provenance. Crash after durable receipt but before ref update commits the group and recovery repairs its ref; crash before receipt leaves members pending. Direct/offline readers apply the same complete-group rule to the records available to them.

The exclusive OS inter-process RecordStore lock spans authorization, entity uniqueness, lifecycle/binding predecessor checks, record publication, and receipt publication. Persist a local durable journal before Git/worktree side effects; include frozen result/member ids and expected/new refs. Prepare all required signed records, including receipt, before an irreversible publication phase that may outlive a key. A prepared journal is not itself a published receipt. Recovery can publish prepared signed bytes after checking preconditions; it cannot sign as a lost session or invent a success.

Write object/receipt files with same-directory temporary files, fsync, rename, and required directory fsyncs. Update `refs/ledger/records` with CAS. On external movement, union object sets and preserve both distinct ref tips as parents; avoid duplicate parents; retry boundedly. A records-ref CAS failure never overwrites an external advance. Preserve semantic forks; do not select by timestamps or enumeration order. Block affected subjects and dependent mutations and report the same conflict after reindex. SQLite indexes only complete committed groups and may lag durable provenance.

## Alternatives considered

Per-file atomic rename does not make multiple records atomic. SQLite transactions alone cannot make portable state authoritative. Ref union solves storage reconciliation, not semantic conflict resolution.

## Consequences and validation

M0.6–M0.10: multi-process writers, exact retries/mismatch, pending groups, missing members, moved ref/union parents, divergent lifecycle/creation/receipt conflicts, shuffled replay, and fault boundaries in docs/contracts/recovery.md.

These are acceptance obligations, not claims that product code already implements them. The untracked milestone plans assign each case to its implementation step.
