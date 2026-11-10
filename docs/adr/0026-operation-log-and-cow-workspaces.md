# ADR 0026: Operation log and copy-on-write workspaces

- Status: Proposed
- Date: 2026-11-10

## Context

Agents should not decide when to commit, and should be able to fork,
checkpoint and roll back freely. Phase 1 reconstructs a write set by diffing
against a fixed base through a private Git index. It records only the frozen
proposal, and pays for a full Git worktree per workspace.

## Decision

**Each workspace has an append-only, hash-chained operation log; a proposal
freezes its head.**

- **Entries.** `{workspace, seq, previous, kind, tree, at, trigger, session}`.
  Kinds: `snapshot` (observed change), `write` (API write with expected hash),
  `restore` (return to an earlier entry's tree), `rebase` (move to a new
  base), `propose` and `rework`. An entry is appended only when the tree
  differs from the previous entry.
- **Integrity without signing every edit.** Entries are unsigned, but each
  names the previous entry's hash. The v2 manifest pins `op_head`, so the
  manifest signature covers the whole log up to the proposal
  ([ADR 0027](0027-record-schema-v2.md)). Entries after the last proposal are
  local runtime state.
- **Triggers.** Phase 1 observation stays: FSEvents or inotify debounced at
  500 ms, calls by the workspace's session, and a 10-second fallback poll. A
  native stat cache (path, size, mtime, inode, blob id) replaces
  `git add -A`, so only changed files are rehashed.
- **Capture is still not atomic.** Proposal capture keeps the two-pass stable
  snapshot and bounded retry from ADR 0010.
- **Checkpoints and undo for agents.** `workspace.restore {op}` writes an
  earlier tree back into the workspace as a new `restore` entry; nothing is
  lost. `workspace.log` returns the entries with symbol deltas, so an agent
  can see what it changed and when.
- **Copy-on-write workspaces.** Each base tree is materialized once under
  `.ledger/runtime/trees/<tree>`. A workspace is a clone of it: `clonefile` on
  APFS, `FICLONE` reflinks on btrfs and XFS, a full copy elsewhere. This sits
  behind the existing `WorkspaceProvider` interface.
- **No repository-wide worktree lock.** Workspace creation and evidence
  checkouts no longer serialize, so speculation can run checks on every
  candidate in parallel, bounded by a worker limit.
- **Ignored files.** `.ledgerignore` uses gitignore syntax, and `.gitignore`
  is honored in bridged repositories. Large ignored trees such as
  `node_modules` are cloned with the workspace on CoW filesystems.
- **The human's directory is a workspace too.** It is the repository's default
  workspace, observed the same way, with entries attributed to a
  root-provisioned human session. People get `ledger describe` instead of
  `git commit`.

**Lines replace branches.** A line (for example `main`) is a named sequence of
signed `line_advance` records. Each one points at a selected manifest whose
base is the line's current head. Phase 2 allows fast-forward advances only; a
workspace on an old base must `rebase` first. A rebase succeeds when no path
changed on both sides, and otherwise returns the overlapping paths and symbols
as structured conflict data for Phase 3's structured merge.

## Alternatives considered

- **Keep Git worktrees under a native store.** Rejected: it keeps the creation
  lock, the per-worktree index and the full checkout cost.
- **A FUSE or virtual filesystem.** Rejected for Phase 2: it needs a macOS
  kernel extension or File Provider work and adds a failure mode for every
  file read. Revisit if clone costs dominate.
- **Sign every log entry with the session key.** Rejected as the default: it
  costs a signature per debounce window and adds no protection the hash chain
  and manifest pin do not already give.

## Validation

- **P2-AC-010:** ten concurrent workspaces of the NestJS fixture, with
  dependencies installed, detect edits within 2 seconds at p95, matching the
  Phase 1 observer target.
- **P2-AC-011:** creating 50 workspaces from one base takes under 5 seconds on
  APFS, and the disk space used grows only with edited files.
- **P2-AC-012:** `restore` to any earlier entry reproduces that tree exactly,
  and the log keeps every later entry.
- **P2-AC-013:** editing the log by hand breaks the chain, and verification of
  the manifest that pinned it fails.
- **P2-AC-014:** speculation with 5 candidates and 2 checks runs checks in
  parallel, with the same results as running them one at a time.
- **P2-AC-015:** a line refuses an advance whose base is not its head, and a
  rebase across a both-sides change returns conflict data instead of a result.
