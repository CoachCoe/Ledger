# ADR 0010: Fixed-base proposal revisions and exact snapshot authorization

- Status: Accepted
- Date: 2026-11-05

## Context

D0 records the revised Phase 1 build prompt's execution contracts before M0. Git remains canonical for code; signed records govern provenance. This decision specifies the existing local Phase 1 scope.

## Decision

Workspace creation fixes the base commit and stable change id. Worktrees are detached at the base; proposal branches are never checked out. Each proposal commit has exactly one parent, that same base, including every rework. The manifest contains base and resulting tree hashes but never the commit hash. Create its signed/hash-addressed bytes before the commit that embeds it.

Use a private index to capture tracked and non-ignored untracked files without changing the user index. Observation uses a persistent index (ADR 0003); proposal capture must be isolated/serialized against observer use. Detect capture-time edits and retry boundedly or return a structured concurrent-edit error. This is not an atomic filesystem snapshot. After tree creation, later edits cannot change that tree or the signed manifest.

Authorize the exact base-to-captured-tree diff immediately before signing, including deletes, both rename paths, modes, changed symlink paths and resolved targets, and agent-made commits. Recheck whole-chain expiry. Never authorize only cached observation paths. Reject unsupported submodule changes explicitly. `may-add-dependencies: false` denies additions/modifications/deletions/renames of recognized manifests and lockfiles; M2 documents at least JS/TS and Python names and limitations. Before M2, only whole-repository capabilities are exposed.

Build commits with `commit-tree` and the four trailers `Ledger-Change`, `Ledger-Intent`, `Ledger-Session`, `Ledger-Manifest`. Verify manifest trailer, exact tree, and sole base parent separately, plus trailer identities against the manifest. Branch updates use CAS from the frozen expected tip; never overwrite an external advance. Retain every revision under create-only `refs/ledger/proposals/<manifest-hash>` before announcing proposal success. Journal retention/branch/receipt publication and recover without guessing a new commit. A conflicting existing retention ref is an error, not an overwrite.

Evidence and selection name an exact manifest/commit pair. Replacing a proposal branch cannot retarget them; retention survives reflog expiry and GC. These refs and reachable objects are included in portable bundles. Sharing documentation may show `git push origin refs/ledger/records` and `git push origin 'refs/ledger/proposals/*:refs/ledger/proposals/*'`, with corresponding explicit fetch refspecs. Phase 1 adds no network orchestration. Ledger never updates protected branches or authorizes a merge.

## Alternatives considered

Parenting reworks to prior proposals changes the diff/authority basis. Checked-out proposal branches leave stale indexes. Hashing the embedding commit in the manifest creates a cycle.

## Consequences and validation

M1.4–M1.7, M2.5, M3.6: two reworks, all trailers/bindings, independent user index, concurrent edits, stale authorization, unsupported submodule, CAS conflict, GC, and offline bundle retention.

These are acceptance obligations, not claims that product code already implements them. The untracked milestone plans assign each case to its implementation step.
