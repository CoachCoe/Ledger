# ADR 0007: Versioned signed records and identifier domains

- Status: Accepted
- Date: 2026-11-05

## Context

Ledger's records must hash and verify identically on every machine and survive export, so the byte-level envelope, signing input, and identifier rules must be fixed before any record is written.

## Decision

Every immutable record is exactly a versioned envelope with `schema_version: 1`, `kind`, `payload`, `signer`, and `signature`. `kind` selects a closed versioned schema. Unknown versions fail explicitly before interpretation. Keys/signatures use unpadded base64url; hashes and root fingerprints use lowercase SHA-256 hexadecimal. A fingerprint hashes the raw 32-byte Ed25519 public key, not its encoding.

The signing message is the literal UTF-8 bytes `ledger-record-v1\n` (ending in byte 0A), followed by RFC 8785 canonical bytes of the envelope with the `signature` member **absent**. Ed25519 signs that entire message. The object hash is SHA-256 of canonical bytes of the complete signed envelope. The hash is external to the object; neither the signature nor object id is computed over an envelope containing its own hash. ADR 0002's stricter safe-integer/Unicode policy applies before decoding or encoding can lose information. NFC applies to paths before record creation, never to JCS strings generally.

Entity ids (`int_`, `ses_`, `cap_`, `wsp_`, `chg_`, `run_`, operation ids) are independently generated opaque identifiers. Creation records contain their entity id. Manifests, attestations, events, bindings, and receipts are addressed by object hash. Displayed `object_hash` accompanies the envelope, never becomes a member of it. Stable-id references are resolved to exactly one authenticated creation hash; zero is unavailable, multiple distinct valid creations are a semantic conflict. Re-importing identical bytes does not create a second creation.

JSON object membership and arrays are signed exactly as stored: JCS sorts object keys, not arrays. Omitted, null, and empty are distinct schema choices. M0 schemas must settle each optional field explicitly. Independent fixtures must publish public key, unsigned canonical bytes, complete signed canonical bytes, signature, and object hash.

## Alternatives considered

Hashing unsigned payloads alone would leave kind/version/signer unbound. Self-hashed entity ids would introduce cycles. Ordinary JSON serialization disagrees with JCS.

## Validation

M0.2–M0.4: independent signatures/hashes, byte tampering, duplicate escaped keys, invalid Unicode, ±(2^53−1), UTF-16 ordering, no normalization, and unknown versions.
