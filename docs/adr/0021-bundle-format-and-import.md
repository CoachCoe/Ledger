# ADR 0021: Bundle format, verification and import

Status: accepted (M3.6). Extends ADR 0013.

## Decisions

1. **Format.** A bundle is an uncompressed tar: `bundle.json` (first entry; versioned inventory validated by `schemas/v1/bundle_inventory.json`), `records/<aa>/<62 hex>.json`, and optional `git/objects.bundle`. Tar is read with the standard library, needs no extraction to disk, and its entry types can be restricted strictly.
2. **Git part.** Created from a throwaway bare repository holding the retained `refs/ledger/proposals/<manifest>` refs plus a synthetic `refs/ledger/records` commit listing exactly the exported records. It has no prerequisites, so `git bundle verify` succeeds in an empty repository. This deviates from bundling the source `refs/ledger/records`: the source ref may contain records outside the selected closure.
3. **Closure.** Manifest, author and verifier sessions and capability chains, root and session public keys, workspace, bindings, lifecycle events, intent, check definitions, complete receipt groups, evidence, selections and competitors. A missing dependency fails `export_incomplete`; `--allow-partial` sets `complete:false`, which never verifies as complete.
4. **Reads files, not SQLite.** `verify` and `export` read the records ref or `.ledger/objects`; they never call `record.Open`. Import uses `record.Store.ImportRecords`, which reuses the recovery path (write missing files, CAS append to the records ref, index, conflict scan), so import equals fetch plus reindex.
5. **Integrity versus trust.** Integrity: invalid beats unavailable beats verified. A missing record, key, receipt or commit is unavailable, never invalid; conflicting creations are reported as conflicts. Trust is `unknown` unless integrity is verified, `trusted` only if every root is on the local list (`--trust`, `--trust-file`, default `$XDG_CONFIG_HOME/ledger/trusted-roots.json`). Keys inside a bundle never add trust. `--strict` returns `verification_failed`.
6. **Staging.** Names are allow-listed; links, extended headers, duplicates, case aliases, oversize declared or actual sizes and a wrong entry order are rejected before anything is published. Git objects are unpacked only into a bare repository inside a private staging directory with global config disabled, hooks off and `transfer.fsckObjects`. Staging is removed on every exit.
7. **Import.** Verified-and-complete is required (`bundle_invalid`, `bundle_incomplete`). Only `refs/ledger/proposals/<manifest>` is created, create-only; a different local commit is reported `ref_conflict`. No key, trust, config, hook, session liveness or code branch is imported. Reimport changes nothing (`already_present`). Divergent histories are kept and reported; no side is chosen.
8. **Versions.** Unknown `bundle_version` fails `unsupported_bundle_version`; unknown envelope `schema_version` fails `unsupported_schema_version`.
9. **Foreign roots (risk in plan decision 15).** Confirmed by `TestAC115_ForeignRootImport`: replay accepts groups signed by a root other than the importing repository's, which stays history only; local operations remain signed by the local root. No relaxation of replay was needed.
10. **Publisher interface.** `verify` cannot import `record`, so `verify.Import` takes a `Publisher`; the CLI adapts `ImportRecords`.

## Deviation
Verification of manifests reports authority via the existing `graph` code (refactored into `newGraph`/`manifest` without changing `Manifest`). Attestations and selections are additionally recomputed; a failed check is an evidence result, not an integrity failure.
