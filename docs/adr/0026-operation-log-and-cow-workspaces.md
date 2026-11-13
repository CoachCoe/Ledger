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
  is honored in bridged repositories.
- **Dependency trees are shared read-only.** An ignored dependency tree such
  as `node_modules` is not cloned per workspace. It is captured once under
  `.ledger/runtime/deps/<key>`, where the key is the hash of the blob ids of
  the dependency manifests and lock files that produced it (the files
  `capability.Dependency` recognizes), with write permission removed.
  A workspace's own `node_modules` is a private, writable directory whose
  packages (and `.bin`) are links into that copy and whose other hidden
  entries are private copies, because tools such as Vite write caches and
  temporary files directly inside `node_modules`. Node still resolves each
  package's dependencies through the shared copy's real path. The directory
  is prepared once per key and cloned per workspace, so creating a
  workspace costs about the tracked tree. The first capture comes from the
  default workspace when its manifests match the base. Dependency
  directories are never part of a snapshot. Changing a dependency manifest already requires
  `may_add_dependencies` (ADR 0019); a workspace that does so gets a
  private, writable clone in place of its link, so no workspace can alter
  another's dependencies. The clone is made before an API write to a
  manifest, on the first snapshot that observes a manifest changed by the
  agent's own tools, or when the agent asks with `workspace.deps.detach`.
  An install that races an observed change fails on the read-only tree and
  succeeds when retried.
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
- **P2-AC-016:** 50 workspaces of the NestJS fixture with `node_modules`
  installed are created in under 5 seconds on APFS, and on a filesystem
  without reflinks each uses under 10% of the disk a copied `node_modules`
  would; the fixture's build and tests pass in each, a write into the
  shared tree fails, and changing `package.json` in one workspace leaves
  every other workspace's dependencies unchanged. The read-only protection
  does not bind a process running as root.

## Evidence (P2-M0 spike)

Measured with the spike described in [ADR 0025](0025-native-content-store.md),
on the same NestJS fixture with its 1,621-package, 548 MB, 50,652-file
`node_modules` installed.

| | macOS (APFS, `clonefile`) | Linux (overlayfs, full copy) |
|---|---:|---:|
| Materialize the base tree from the store, once | 433 ms | 139 ms |
| One workspace from the base tree | 34 ms, about 1.2 MB | 108 ms, about 19 MB |
| 50 workspaces, sequential (Linux: 20) | 1.98 s | 2.3 s |
| 50 workspaces, 10 at a time (Linux: 20) | 2.05 s | 0.87 s |
| One workspace including `node_modules` | 1.4 s, about 20 MB | 4.8 s, about 583 MB |
| 50 workspaces including `node_modules` (Linux: 20) | 71 s, about 1 GB | 93 s, about 11.7 GB |
| One `git worktree add` | 528 ms, about 18 MB | 128 ms, about 20 MB |

- P2-AC-011 holds for the tracked tree: 50 APFS workspaces in under two
  seconds, with disk use growing only by metadata until files are edited.
- `clonefile` creation does not get faster in parallel on APFS; the total
  time is the same at one or ten at a time.
- Ten concurrent `git worktree add` calls all succeeded with Git 2.54; the
  Phase 1 serialization is Ledger's own lock (ADR 0004), not Git's.
- Cloning ignored dependency trees dominates workspace cost on both
  platforms: 40 times the tracked tree on APFS, and a full copy per workspace
  on filesystems without reflinks. Cloning `node_modules` with each
  workspace would cost about 1.4 seconds per workspace on APFS and would
  make 50-attempt speculation on ext4 a 30 GB operation. Dependency trees
  are therefore shared read-only rather than cloned, and btrfs or XFS is not
  required for large runs.

## Evidence (P2-M1 benchmark)

`tools/workspacebench` on the same fixture and hosts, with dependencies
shared as described above. On Linux it ran as an unprivileged user, since
root ignores the read-only modes.

| | macOS (APFS) | Linux (overlayfs, full copy) |
|---|---:|---:|
| 50 workspaces, one at a time | 3.05 s, p95 78 ms each | 9.6 s, p95 240 ms each |
| 50 workspaces, ten at a time | | 6.7 s |
| Disk for 50 workspaces | about 83 MB | about 1.4 GB |
| First workspace: materialize base, capture `node_modules` | 9.5 s | 4.4 s |
| Edit detection, ten watched workspaces (P2-AC-010) | p95 1.2 s | p95 1.0 s |
| Restore to an earlier entry (P2-AC-012) | 0.40 s | 0.28 s |

- Sharing cuts a Linux workspace from about 583 MB and 4.8 s to about 28 MB
  and 190 ms; what remains is the full copy of the tracked tree.
- The fixture's TypeScript build and `vitest` run pass in workspaces on the
  shared tree. Linking all of `node_modules` to the read-only copy failed:
  Vite writes `node_modules/.vite-temp`, which is why the directory itself
  is private.
