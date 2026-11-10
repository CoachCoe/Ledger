# ADR 0025: Native content store

- Status: Proposed
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
| `symbol_map` | For one blob and one analyzer version: declarations with locator, byte range and span hash, static references, fallback reasons | SHA-256 of its canonical encoding |

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
- **Layout.** Loose objects at `.ledger/store/objects/<aa>/<hash>`, written
  with `internal/atomicfile` (temporary file, fsync, rename, directory fsync).
  Pack files are deferred until measurement shows they are needed. SQLite
  stays an index that `ledger reindex` can rebuild.
- **Retention replaces refs.** An object reachable from any signed record or
  any live operation log head is never collected. This replaces
  `refs/ledger/proposals/*` as the way evidence stays resolvable.

## Alternatives considered

- **Git's object database as the backend, native everything else.** This is
  how Jujutsu began, and it is the lower-risk path. Rejected as the target
  because records would keep binding SHA-1 Git hashes, symbol maps would be
  opaque blobs to Git, and the bridge must translate anyway. It remains the
  fallback if the store spike shows the native store far slower than Git on
  the NestJS tree; the operation log and v2 records would then sit on Git's
  object database and store both hashes.
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
