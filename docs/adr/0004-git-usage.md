# ADR 0004: How Ledger uses Git

- Status: Accepted
- Date: 2026-11-05

## Context

Ledger drives Git through the `git` CLI for worktrees, snapshots, commits with trailers, compare-and-swap ref updates, retention refs, and portable bundles. These behaviors must hold on the Git versions customers run, and must not disturb a user's own Git state.

## Decision

1. **Minimum Git version is 2.25.** `ledger init` checks it. All behaviors below were confirmed on 2.25.1, 2.34.1, 2.39.5, and 2.54.0.
2. **Worktree creation is serialized.** Ledger holds a repository-wide lock around `git worktree add` and `git worktree remove`. Concurrent `git worktree add` calls intermittently fail with `fatal: failed to read .git/worktrees/<name>/commondir`.
3. **Workspaces are checked out detached at their base commit.** Proposal branches (`ledger/<intent>/<workspace>`) are never checked out in any worktree. Moving a branch that a worktree has checked out leaves that worktree reporting spurious staged changes, because its index still matches the old commit.
4. **Snapshots use a private index.** Ledger sets `GIT_INDEX_FILE` to a per-workspace index under `.ledger/runtime/`, runs `git add -A`, and runs `git write-tree`. The user's index, staged state, and working files are never touched.
5. **Proposal commits are built with `git commit-tree`** from the pre-computed tree, with the base commit as the only parent. The message's trailers are written with `git interpret-trailers`. The commit's tree is exactly the tree computed before the manifest was signed.
6. **Refs are updated with compare-and-swap** (`git update-ref <ref> <new> <expected-old>`). Creation uses the all-zero expected value, so it succeeds only if the ref does not exist.
7. **Every proposal revision is retained** under `refs/ledger/proposals/<manifest-hash>`, which keeps replaced revisions reachable through `git gc --prune=now` with reflogs expired.
8. **Portable bundles use `git bundle`.** A bundle of the retention refs verifies, imports, and passes `git fsck --strict` in a fresh repository with the source repository unavailable.
9. **Ledger never changes repository or user Git configuration.** Settings Ledger needs are passed per command with `git -c`.

## Evidence (Stage 0 spike S5)

- A 25-check script passed five times on each tested version. A 26th check, added afterwards and run on 2.54.0, confirmed that a rename appears as both its old and new paths in the base-relative change set (`git diff-index --no-renames`). Debian bullseye was not tested because its archived Git package was unavailable.
- Parallel `git worktree add` was stress-tested with 400 additions, 10 at a time:

| Git | Failed adds |
|---|---|
| 2.54.0 (macOS) | 3 of 400 |
| 2.39.5 (Linux) | 3 of 400 |
| 2.34.1 (Linux) | 0 of 400 |

- The private-index snapshot captured the working-tree content, not the staged content. It included non-ignored untracked files, excluded ignored files, and recorded symlinks (mode 120000) and executable-bit changes. It also included changes the agent had committed itself.

## Consequences

- Fork throughput is bounded by serialized worktree creation. Worktree creation is fast; installing dependencies dominates (ADR 0003).
- If an agent checks out a branch inside its workspace, that does not affect Ledger, because the change set is always computed against the fixed base commit.
