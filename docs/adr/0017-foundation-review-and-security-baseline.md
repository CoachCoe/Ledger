# ADR 0017: Foundation replay, durability and security baseline

- Status: Accepted
- Date: 2026-11-05

## Context

Review of M0 reproduced failures at journal recovery, session authentication, reindex concurrency and key custody. The dependency scan also identified fixed vulnerabilities in the original toolchain and Unicode library.

## Decision

- Prepared journals are revalidated for canonical hashes, schemas, membership and signatures before recovery can publish effects or records. Object envelopes must agree with their stored bytes. Complete session authentication includes the matching root-issued capability on replay as well as local publication.
- Reindex clears derived tables transactionally in the existing database. It does not strand other processes on an unlinked database inode. If a user deletes/replaces the database, writers reconnect under the RecordStore lock. Subject enumeration uses one SQLite statement snapshot.
- Incompatible receipts block every affected subject, including members of the previously indexed receipt, and reproduce the same conflicts after reindex. A session may end itself; its receipt signer is checked before the group's transition ends it. Operation time must fall within the session's active authorization period.
- Root-key publications with identical public keys resolve to the same fingerprint authority even when their publication records differ. Session ids still require one authenticated creation, even when conflicting records reuse the same public key. Root-key `created_at` describes that publication record, not externally proven key-generation time.
- Keys must be private regular files in private directories outside Git repositories. Initialization resolves aliases, refuses silent root rotation/loss, and publishes metadata atomically. File publication syncs both contents and directory entries. Self-parenting and self-superseding intents are rejected; supersession references an existing intent.
- Initialization serializes its setup and freezes a random operation id per repository. If configuration or the derived index is lost, it recovers that id from accepted provenance. Deriving the id from the global root fingerprint alone would create incompatible receipts when the same human initializes two repositories.
- Require Go 1.27.3 or newer and `golang.org/x/text` v0.41.0. The prior scan reported standard-library advisories through Go 1.27.2 (including [GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809)) and Unicode advisories [GO-2026-5970](https://pkg.go.dev/vuln/GO-2026-5970) / [GO-2026-6629](https://pkg.go.dev/vuln/GO-2026-6629). Upgrade to fixed releases instead of suppressing results. CI runs the pinned official `govulncheck` v1.8.0 against the current database.

## Alternatives considered

Trusting a journal without checking its records would bypass the immutable store's acceptance boundary. Replacing an open SQLite file splits readers and writers across different databases. Restoring a root key silently changes identity. Treating duplicate session creations as one key hides a semantic conflict; treating identical root-key publications as different authorities breaks portable imports.

## Validation

Regression tests reproduce each failure before the fix. The multi-process test now publishes 2,162 objects across six writers. Race tests, lint, both milestone demos and module verification exercise the corrected implementation. The patched dependency scan reports no vulnerabilities. Full offline provenance authorization, evidence execution and portable bundle verification remain M3 responsibilities.
