# ADR 0003: Workspace observation

- Status: Accepted
- Date: 2026-11-05

## Context

Agents edit workspace files with their own tools, so `ledgerd` must observe each workspace's write set: every path that differs from the workspace's fixed base commit, including untracked non-ignored files, deletions, renames (both paths), mode changes, symlinks, and anything the agent committed itself. The M1 target is detection within 2 seconds with the watcher enabled, across at least 10 concurrent workspaces of a real JavaScript/TypeScript project with `node_modules` installed.

## Decision

**The write set is computed with a persistent private index per workspace**, stored under `.ledger/runtime/`:

1. On first use, `GIT_INDEX_FILE=<private> git read-tree <base>`.
2. On each observation, `GIT_INDEX_FILE=<private> git add -A`, then `git diff-index --cached --no-renames --name-status -z <base>`.

The index persists between observations, so Git's stat cache means only changed files are re-hashed. `git status` is supplementary information and is never the source of the write set.

**A file watcher only triggers observations, debounced at 500 ms.** The platform determines the watcher:

| Platform | Watcher |
|---|---|
| macOS | FSEvents (`github.com/fsnotify/fsevents`, cgo): one recursive stream per workspace root, with events under ignored directories and `.git` filtered out |
| Linux | inotify (`github.com/fsnotify/fsnotify`) on every non-ignored directory, adding watches as new directories appear |

A 10-second poll is the fallback for missed events, and all workspaces are re-observed when `ledgerd` starts.

**Git's file-system monitor is not used.** It gave no speedup with a private index and would add one daemon per workspace.

## Alternatives rejected

- **`fsnotify` on macOS.** It uses kqueue there, which opens a descriptor for every watched file and directory: 35,670 descriptors for 10 workspaces. It also missed the latency target (p95 2.9 s, max 4.0 s).
- **Polling without a watcher.** It meets the latency target at a 500 ms interval, but on macOS it costs about 3.5 CPU cores continuously for 10 workspaces, even when no one is editing.
- **`git status` as the write set.** It reports changes against `HEAD`, not the fixed base, so an agent's own commit would hide its changes.

## Evidence (Stage 0 spike S2)

Test subject: the NestJS repository (2,743 tracked files in 796 directories), with each of 10 detached worktrees carrying its own ignored `node_modules` (50,652 files, 547 MB). Simulated agents edited a fresh tracked file, deleted a tracked file, or created a file in a new directory every 1–3 s, for 60 s per run.

Write-set computation (warm private index, one edit):

| | One workspace, p50 | 10 workspaces at once, p50 / p95 |
|---|---|---|
| macOS 26.7, Apple Git 2.54, M-series, 10 cores | 113 ms | 327 / 411 ms |
| Linux (Docker, arm64, 10 cores), Git 2.39 | 13 ms | 42 / 55 ms |

Edit-to-detection latency, 10 workspaces, 500 ms debounce:

| Platform and watcher | p50 / p95 / max | Watch descriptors | CPU per minute of activity |
|---|---|---|---|
| macOS, FSEvents | 631 / 679 / 914 ms | 0 | 37 s |
| macOS, fsnotify (kqueue) | 1.94 / 2.92 / 4.00 s ❌ | 35,670 | 36 s |
| macOS, 500 ms poll | 499 / 727 / 831 ms | 0 | 215 s ❌ |
| Linux, fsnotify (inotify) | 520 / 537 / 544 ms | 10 | 9 s |
| Linux, 500 ms poll | 284 / 511 / 546 ms | 0 | 49 s |

In every run, edits not yet detected when the run ended (at most 4 of about 280) were made inside the final debounce window.

## Consequences

- D0 review found unsynchronized appends to the spike's shared latency sample slice. The spike now synchronizes sample collection/reporting, with a concurrent regression test under `-race`. The table above is the historical Stage 0 measurement, not a rerun of the corrected collector. Repeat the real-project 10-workspace benchmark with the corrected collector before using it as M1 acceptance evidence.

- The latency floor is the debounce interval plus one write-set computation. The default 500 ms debounce leaves more than a second of headroom against the 2-second target on both platforms.
- macOS observation costs about 8× more CPU than Linux. That cost comes from spawning Git processes and from filesystem metadata calls, and it scales with edit activity, not with repository size. If it matters at larger scale, the next step is to keep the Git index in-process instead of spawning two Git commands per observation.
- Default inotify limits are high on current distributions (1,048,576 watches here). Ledger needs one watch per non-ignored directory, and reports a structured error rather than silently degrading if a limit is hit.
- Creating a workspace for a JavaScript project is dominated by installing dependencies, not by Git. Cloning `node_modules` took 16 s per workspace on macOS with APFS clones and about 1.5 s on Linux. This informs speculation-run costs (M5) and the benchmark.
