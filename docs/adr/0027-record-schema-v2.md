# ADR 0027: Record schema v2

- Status: Accepted (2026-11-12)
- Date: 2026-11-10

## Context

Six v1 schemas bind Git objects: `workspace`, `change_manifest`,
`evidence_attestation`, `selection_decision`, `speculation_run` and
`bundle_inventory`. Records are immutable and must verify forever, so v1
cannot be edited in place. ADR 0007 already requires unknown versions to fail
explicitly, which makes a clean second version possible.

## Decision

**`schema_version: 2` keeps the v1 envelope and changes only what binds
code.** Signing uses the literal prefix `ledger-record-v2\n`, then RFC 8785
bytes of the envelope without `signature`, exactly as ADR 0007 specifies for
v1.

| Record | v1 fields | v2 fields |
|---|---|---|
| `workspace` | `base_commit` | `base_tree`, optional `base_line_advance` |
| `change_manifest` | `base_commit`, `tree` | `base_tree`, `tree`, `op_head`, `symbol_maps`; symbol entries gain `revision` (span hash) |
| `evidence_attestation` | `commit`, `executed_tree` | `tree` only; the checked-out tree must equal it |
| `selection_decision` | `commit` | removed; the manifest names the tree |
| `speculation_run` | `base_commit` | `base_tree` |
| `bundle_inventory` | Git bundle part | store-object closure ([ADR 0028](0028-git-bridge.md) adds an optional Git part) |

New record kinds:

- **`repository`:** the root-signed creation record, with repository id, hash
  algorithm, case policy and analyzer set.
- **`line_advance`:** `{line, previous, manifest, selection, actor, at}`,
  signed by root or a session holding a new `may_advance` capability for that
  line. This is the explicit acceptance record that Phase 1 left to Git
  merges.
- **`bridge_mapping`:** `{native_tree, git_commit, git_tree, direction}`,
  signed by root (ADR 0028).
- **`foreign_change`:** a change that arrived through the bridge, signed by
  root with participant kind `bridge`. It claims no capability and cannot
  satisfy check criteria by itself.

**A native repository lists its accepted groups in a records head log**
instead of `refs/ledger/records`. Record files stay under `.ledger/objects`
as in ADR 0009, and `.ledger/records.log` holds one canonical JSON line per
accepted receipt, `{seq, previous, receipt}`, hash-chained like the
workspace operation log (ADR 0026). The entry is appended and synced under
the RecordStore lock after the receipt is durable and before the index is
updated, where Phase 1 updates the ref. Recovery cuts a torn final line,
appends groups that are accepted but unlogged (a crash between receipt and
log, or an import) in receipt hash order, and reports logged receipts whose
group is no longer complete as `record_missing` instead of dropping them.
A log that is edited, shrinks or does not chain fails with
`record_log_broken`. The log is unsigned: it detects crashes and lost
files, not a writer who rewrites both the log and the objects; signatures
and receipts remain the authority. There is no shared ref to merge: other
machines' records arrive through bundles and the bridge, and import
appends them.

**v1 stays verifiable for good.** `ledger verify` keeps the v1 code path. A
Phase 1 repository migrates by importing its Git history through the bridge,
which writes `bridge_mapping` records for every commit a v1 record names. No
v1 record is rewritten or re-signed. The index reads both versions, and new
operations write only v2.

**Capabilities stay as they are,** with one addition: `may_advance: [line]`,
defaulting to empty so no existing authority widens. The session-bound MCP
adapter (ADR 0024) lands first, so acceptance cannot be self-granted through
an empty `session_id`.

## Alternatives considered

- **Add native fields beside the Git ones in v1.** Rejected: v1 schemas are
  closed, and optional dual bindings would let a record satisfy either one.
- **Keep no head log; receipt files alone define what is accepted.**
  Rejected: a deleted receipt would silently remove its group, and
  incremental export would have to scan every object.
- **Re-sign v1 history as v2.** Rejected: it replaces original authorship with
  the migration's, against ADR 0011's rule that recovery never rewrites
  authors.
- **Keep acceptance outside Ledger.** Rejected: without Git merges there is
  nowhere else for it to live, and a signed acceptance is what auditors ask
  for.

## Validation

- **P2-AC-020:** published v2 fixtures (public key, unsigned bytes, signed
  bytes, signature, hash) match an independent implementation, as Phase 1's
  JS reference fixtures do.
- **P2-AC-021:** a v1 bundle from Phase 1's `demos/m3.sh` verifies unchanged
  after the upgrade.
- **P2-AC-022:** an attestation whose `tree` differs from its manifest's is
  rejected with `cross_revision_evidence`.
- **P2-AC-023:** a `line_advance` from a session without `may_advance` for that
  line, or with a stale `previous`, is rejected inside the RecordStore lock.
- **P2-AC-024:** a migrated Phase 1 repository answers `history.why` for a
  symbol across the v1 and v2 records.
