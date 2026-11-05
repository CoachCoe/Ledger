# ADR 0011: Lifecycle replay, session loss, and workspace rebinding

- Status: Accepted
- Date: 2026-11-05

## Context

D0 records the revised Phase 1 build prompt's execution contracts before M0. Git remains canonical for code; signed records govern provenance. This decision specifies the existing local Phase 1 scope.

## Decision

Creation establishes intent `open`, session `active`, workspace `open`, and speculation run `created`. The first transition has null previous-event hash and from-state equal to the creation state. Later events name the previous event hash and all events name the authenticated creation hash. Under the RecordStore lock accept only the current predecessor/from-state and an authorized signer/actor, as one receipt group. Imported forks are conflicts, not an arbitrary replay order.

Intent transitions only open→closed (completed/abandoned/superseded; superseded names the replacement). Session transitions only active→ended. Workspace transitions are open→proposed/closed and proposed→open/selected/closed. Selected/closed are terminal. Proposal events bind the published revision in their operation result. Run states and frozen candidates follow ADR 0015. Deterministic replay uses dependencies/predecessors and receipt completeness, never filenames or timestamps to resolve branches.

On daemon restart, root-signed ended events with reason `daemon_restart` reconcile formerly active sessions whose keys were lost. Do not impersonate their keys. Root unavailable means recovery pending and blocked signing/mutation. Existing worktrees and user edits remain. Already fully prepared signed operations may complete under ADR 0009; reconcile their outcome before deriving ended-session state.

Explicit root-authorized recovery creates a new session and newly authorized capability, then a signed workspace-binding record with workspace id, predecessor binding hash (workspace creation hash initially), new session/capability, actor, timestamp, and reason. Root authenticates that recovery; no unrelated session may seize a workspace. Check predecessor and open/proposed state inside the lock. Rebinding selected/closed workspaces is forbidden. Concurrent rebindings accept one predecessor; imported divergent bindings are conflicts.

Ownership is creation plus committed bindings, reconstructible without SQLite. A new binding changes future authority, not historical manifest/evidence authors. Revalidate the entire base-relative snapshot under replacement authority at proposal; do not revive expired/delegated authority. Root-authorized recovery may inspect old notification queues while preserving original attribution. Ordinary restart retains unacknowledged notifications. Reindex reports discarded delivery state/leases, rebuilds locations from Git metadata where available, and regenerates current warnings; it cannot reconstruct edits reverted while stopped.

Worktree creation/removal uses a repository-wide serialized lock in a documented consistent ordering with RecordStore. Close is durable before cleanup. Cleanup verifies exact journaled workspace identity/path and never deletes unrelated worktrees or unrecorded user edits. Interrupted creation is inspectable and retryable; it must not delete an edited partial worktree. See the single recovery table for every side effect.

## Alternatives considered

Mutating creation records breaks signatures. Silent key rotation falsifies authorship. Selecting a lifecycle fork by time makes reindex/import nondeterministic.

## Consequences and validation

M0.9/M0.10, M1.2a/M1.4/M1.7, M2.4: stale predecessor, fork conflicts, lost keys/root unavailable, concurrent binding, historical authorship, terminal denial, preserved files/queues and reindex gaps.

These are acceptance obligations, not claims that product code already implements them. The untracked milestone plans assign each case to its implementation step.
