# ADR 0025: Native content store

- Status: Accepted (P2-M0 go decision, 2026-11-10)
- Date: 2026-11-10

## Context

Phase 2 replaces Git as Ledger's storage and history foundation. Ledger gets
its own content store (this ADR), a per-workspace operation log and
copy-on-write workspaces ([ADR 0026](0026-operation-log-and-cow-workspaces.md)),
records that bind native objects ([ADR 0027](0027-record-schema-v2.md)), and a
bridge that keeps Git only for importing repositories and mirroring results to
Git hosts and CI ([ADR 0028](0028-git-bridge.md)). The governance layer carries
over unchanged: canonical JSON (ADR 0002), the signed envelope (ADR 0007, with
a v2 signing prefix), root and session identity (ADR 0008), receipts and
recovery (ADR 0009), lifecycle replay (ADR 0011), evidence and selection
semantics (ADR 0012), capability enforcement (ADR 0019), speculation (ADR 0023)
and the MCP adapter (ADR 0024).

Several of Phase 1's hardest constraints come from Git rather than the
problem: worktree creation is serialized (ADR 0004), so speculation runs its
evidence checks one after another (ADR 0023); observation runs `git add -A`
against a private index (ADR 0003); a proposal is a commit kept alive by
`refs/ledger/proposals/*` so garbage collection cannot delete it (ADR 0010);
symbol analysis is an advisory cache keyed by Git blob SHA (ADR 0006); and
every record binds Git hashes, so the provenance model cannot outgrow Git's
object model.

Ledger needs a store for code that it controls: immutable, content-addressed,
byte-exact for every file, and able to hold symbol structure as part of
history instead of as a cache beside it. Records must be able to name its
objects directly, without a Git commit in between.

## Decision

**File bytes stay the source of truth; symbols are derived, immutable
objects.** The store has four object kinds:

| Kind | Holds | Identity |
|---|---|---|
| `blob` | Exact file bytes, no normalization | SHA-256 of the bytes |
| `chunked_blob` | Ordered chunk list for files over 4 MiB, cut by content-defined chunking | SHA-256 of its canonical chunk list |
| `tree` | Sorted directory entries: name, entry type (`file`, `executable`, `symlink`, `tree`, `gitlink`), object id | SHA-256 of its canonical encoding |
| `symbol_map` | For one blob at one path and one analyzer version: declarations with locator, byte range and span hash, static references, fallback reasons | SHA-256 of its canonical encoding |

- **Hashing.** Object id = SHA-256 over the prefix `ledger-object-v2\n`, the
  kind and the canonical bytes. Ids are lowercase hex, as in Phase 1. The
  repository creation record names the algorithm, so a later change is a new
  version rather than a silent switch.
- **Encoding.** Trees and symbol maps use RFC 8785 canonical JSON through
  `internal/canon`, so one verified encoder covers records and objects. Entry
  names are NFC-normalized and sorted by UTF-8 bytes, with the case policy
  detected at init (ADR 0019 path rules).
- **Symbol maps are pinned to an analyzer.** A map names its analyzer and
  grammar versions. A new analyzer writes new maps; old ones stay, so a
  manifest's symbol claims can always be recomputed exactly as they were made.
- **Span hashes give symbol revisions.** Each declaration carries the hash of
  its own byte range. "Did `invoice.total` change?" becomes a comparison of
  two hashes, not a reparse.
- **Unsupported files have no map.** They are versioned exactly and use the
  file-level fallbacks Phase 1 already labels.
- **Layout.** Loose objects at `.ledger/store/objects/<aa>/<hash>`, each
  written to a temporary file in its fan-out directory and renamed into
  place. Objects are stored uncompressed. Pack files are deferred until
  measurement shows they are needed. SQLite stays an index that
  `ledger reindex` can rebuild.
- **Durability is batched.** Each object gets a write-out-only flush (plain
  `fsync(2)` on darwin, `sync_file_range` on Linux), the touched directories
  are flushed the same way, and one full barrier (`F_FULLFSYNC`, `syncfs`)
  makes the whole batch durable before any record or operation log entry
  that names those objects is committed. This is Git's
  `core.fsyncMethod=batch`. A per-object full sync, as `internal/atomicfile`
  does for records, costs about 46 times as much on APFS (see Evidence). A
  crash before the barrier can leave unreferenced or truncated objects but
  no committed record that names them; reads check every object against its
  id, and recovery removes one that fails so the next write recreates it.
- **Retention replaces refs.** An object reachable from any signed record or
  any live operation log head is never collected. This replaces
  `refs/ledger/proposals/*` as the way evidence stays resolvable.

## Alternatives considered

- **Git's object database as the backend, native everything else.** This is
  how Jujutsu began, and it is the lower-risk path. Rejected as the target
  because records would keep binding SHA-1 Git hashes, symbol maps would be
  opaque blobs to Git, and the bridge must translate anyway. It was kept as
  the fallback in case the P2-M0 spike showed the native store far slower
  than Git on the NestJS tree; the spike showed the opposite.
- **Store parsed syntax trees instead of text.** Rejected: lossy for
  formatting and comments, tied to grammar versions, and useless for
  unsupported files.
- **Text only, with symbols computed on demand.** Rejected: that is Phase 1's
  advisory cache, and it makes symbol history unverifiable offline.

## Validation

- **P2-AC-001:** any file round-trips byte for byte, including CRLF, invalid
  UTF-8, empty files and files over 4 MiB.
- **P2-AC-002:** two paths that normalize to the same NFC name, or that collide
  under a case-insensitive policy, are rejected at write.
- **P2-AC-003:** writing the NestJS fixture tree from the observer benchmark
  (2,743 files) completes and is measured against Git's `write-tree` on the
  same tree.
- **P2-AC-004:** a one-line edit to a 50 MiB file stores new chunks for that
  region only.
- **P2-AC-005:** a symbol map recomputed by the pinned analyzer equals the
  stored one, byte for byte.

## Evidence (P2-M0 spike)

The spike stores the NestJS fixture from the observer benchmark (commit
`6e83cc8`, 2,743 tracked files, 8.3 MiB, 751 trees, 2,292 distinct blobs)
with this ADR's object format, using a copy of `internal/canon` for tree
encoding, and compares it with `git add -A` into a private index plus
`git write-tree` into an empty object directory. Times are medians of five
runs with a warm page cache. macOS is an M1 Max with 10 CPUs on APFS and
Git 2.54. Linux is arm64 in Docker on the same host, on overlayfs, with Git
2.39; the virtual disk makes its sync timings optimistic.

| Cold write of the whole tree | macOS | Linux |
|---|---:|---:|
| Native, full sync per object and directory | 31.9 s | 1.8 s |
| Native, batched (10 workers) | 0.69 s | 0.13 s |
| Native, no sync (10 workers) | 0.60 s | 0.10 s |
| Git, `core.fsync=loose-object`, `core.fsyncMethod=batch` | 4.31 s | 0.34 s |
| Git default (loose objects are not synced) | 2.35 s | 0.28 s |
| Git, `core.fsync=loose-object`, `core.fsyncMethod=fsync` | 31.1 s | 2.4 s |

- At equal durability the native store writes the tree about six times
  faster than Git on macOS and about two and a half times faster on Linux.
  Git also spends time on zlib compression; the uncompressed native store is
  8.8 MB against Git's 2.7 MB of loose objects.
- Every file round-trips byte for byte with its executable bit, and the root
  tree id is the same for every durability mode and worker count.
- After a one-file edit, a stat-cache snapshot (walk, rehash one blob,
  rewrite five trees) takes 60 ms at the median and 75 ms at p95 on macOS
  (Git: 98 ms), and 22 ms on Linux (Git: 14 ms).
- P2-AC-004: storing a 50 MiB file with content-defined chunking (minimum
  16 KiB, average 64 KiB, maximum 256 KiB) gives 699 chunks; a one-line edit
  in the middle stores one new chunk of 95 KB.

Not measured by the spike: a cold page cache, symbol-map persistence
(P2-AC-005), and rejection of colliding names (P2-AC-002). They are P2-M1
acceptance.
