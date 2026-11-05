# ADR 0012: Pinned check definitions, evidence sets, and selection

- Status: Accepted
- Date: 2026-11-05

## Context

Evidence is only meaningful if it names exactly what was tested and how. Selection must not hide failures or be mistaken for acceptance.

## Decision

Intent creation requires at least one criterion; checks must exist in `.ledger/checks.json`. Store immutable check definitions and pin their hashes at creation. Editing that file cannot change existing requirements: create a superseding intent. Definitions cover category, optional setup, command, working directory, environment allowlist, timeout, and bounded output policy. M1 implements loading/pinning; it does not need M3 execution.

Evidence APIs freeze manifest hash, bound commit, and definition hash. A change-id convenience lookup resolves once and returns that binding. Verify all commit bindings before execution, then use a fresh detached clean verification worktree, never the author workspace. Check tracked content before setup and after setup/check; mutation fails even if exit is zero. Generated untracked/ignored dependencies are allowed and described.

Use a distinct authenticated verifier session. Reduce environment to the definition allowlist; record OS/architecture, toolchain versions, variable names/value digests, lockfile hashes, configuration, and an environment hash. Do not export secret values. Record executed tree/commit, category/criterion ids, setup/check statuses and durations separately, bounded output/digest, times, explicit result/reason, and `isolation: "none"`. Setup failure, nonzero exit, timeout, cancellation, tracked mutation yield failed attestations if signing remains possible. Key loss yields an interrupted operation, never fabricated passing evidence. Kill child process groups and clean up or explicitly retain debug worktrees.

Applicable evidence must match exact revision/definition/criterion/category, independent verifier and authenticated participant kind, with valid execution preconditions. Human judgment uses a root-authenticated human verifier session. Independence does not establish correctness or independent reasoning. Checks run with the local user's filesystem/process/network authority; Phase 1 supplies no isolation.

Compute criteria over an explicit evidence set: none applicable→outstanding (empty deciding list); any applicable failure→failed; mixed pass/fail→failed with conflict detail; only applicable passes→satisfied. Return known contrary evidence separately, even if omitted from the submitted set. Another definition/revision or caller-supplied reviewer label cannot satisfy a criterion.

A signed selection names manifest/commit, the exact evidence set, recomputed criterion states/deciding hashes/conflicts/outstanding items, known contrary evidence, selector, time, and scoring inputs/competitors when relevant. Reject fabricated satisfied status and cross-revision references. Selection never updates mainline, constitutes acceptance, or rewrites a proposal commit. Speculation requires every check criterion satisfied; judgment remains outstanding unless applicable evidence exists.

## Alternatives considered

Running in the mutable author workspace or resolving a change id after starting a check changes what was tested. Latest-pass-wins hides contrary evidence. Caller-provided reviewer kinds are unauthenticated.

## Validation

M1.3, M3.1–M3.5: changed definitions/revisions, author edits/evidence, three binding mismatches, process failures/cleanup, tracked mutation, explicit-set truth table, contrary evidence, reviewer kinds, and selection without merge.
