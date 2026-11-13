# ADR 0016: Record schema conventions

- Status: Accepted
- Date: 2026-11-05

## Context

ADR 0007 fixes the envelope and signing rules but leaves the payload shapes to the M0 schemas. Some choices there affect every record and every independent implementation, so they are recorded here.

## Decision

- **Schemas** live in `schemas/v1/`: one JSON Schema (draft 2020-12) per record kind, `envelope.json`, shared definitions in `common.json`, and unsigned runtime documents in `runtime/`. They are embedded in the binary. Every object is closed (`additionalProperties: false`).
- **Every payload member is required.** An optional value is an explicit `null`; an empty list is `[]`. Omitted, null, and empty therefore never mean the same thing, and a record has exactly one valid encoding for a given meaning.
- **The root public key is a record.** Kind `root_key` carries the public key and is signed by that key, with `signer` equal to its fingerprint. This lets the records ref carry every key needed to verify it. Its entity id is the fingerprint. Publishing a root key does not make it trusted (ADR 0013).
- **Entity ids** are a prefix (`int_`, `ses_`, `cap_`, `wsp_`, `chg_`, `run_`, `op_`) plus 26 lowercase base32 characters encoding 128 random bits.
- **Timestamps** are UTC RFC 3339 strings ending in `Z`; Ledger writes millisecond precision.
- **Principals** (`signer`, `actor`, `owner`) are a root fingerprint or a session id.
- **Operation receipts** list member hashes in ascending order. A receipt's `result` is a JSON object.
- **Runtime documents** (notifications, leases, dependency edges, run handoff and status) are validated by schema but never signed or exported.

## Alternatives considered

Optional members with omission would give two encodings for "no value" and require every implementation to agree on which one to emit. Keeping root keys only in local configuration would make the records ref unverifiable on another machine.

## Validation

`schemas/examples/v1/` and `schemas/examples/v2/` hold one signed example per kind of each schema version. Tests round-trip every example through the Go types and back to identical canonical bytes, reject each example with any member removed or an unknown member added, and verify signatures produced independently with Node's Ed25519 and a separate RFC 8785 implementation (`internal/envelope/testdata/`).
