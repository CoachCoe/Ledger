# ADR 0002: Canonical JSON with the standard library's RFC 8785 implementation

- Status: Accepted
- Date: 2026-11-05

## Context

Every immutable record is identified by the SHA-256 of its RFC 8785 (JCS) canonical form, and signatures cover canonical bytes. Two implementations that disagree on a single byte produce different record ids, so canonicalization must match independent implementations exactly. Ordinary JSON serialization is not canonical: Go's `encoding/json` escapes `<`, `>`, `&`, U+2028, and U+2029, and sorts keys by UTF-8 bytes instead of UTF-16 code units.

## Decision

Use `encoding/json/jsontext` from the Go standard library (1.27+) for canonicalization, via `jsontext.Value.Canonicalize`, behind Ledger's `internal/canon` package. Do not write an in-house JCS encoder.

`internal/canon` first validates input against Ledger's stricter supported-input contract, then canonicalizes:

- Decoding uses `jsontext` defaults, which reject duplicate object member names (compared after unescaping, so `"a"` and `"a"` are duplicates), invalid UTF-8, and unpaired or reversed surrogate escapes.
- Numbers must be plain integer syntax matching `-?(0|[1-9][0-9]*)`, within ±(2^53−1). Fractions, exponents (including integral values such as `1e2`), and `-0` are rejected. Larger integers, decimals, and timestamps are strings.
- A byte-order mark, trailing data after the value, `NaN`, and `Infinity` are rejected.
- Canonicalization never applies Unicode normalization; NFC normalization of paths happens before record creation.

## Alternatives considered

- **In-house encoder.** Rejected: the standard library implementation passes every independent check below, and a second implementation adds risk without benefit.
- **Third-party Go JCS libraries.** Rejected for the same reason, plus an extra dependency in the security-critical path.

## Evidence (Stage 0 spike S4)

- All six reference vector files from the RFC 8785 author's repository (`cyberphone/json-canonicalization`, including floats, Unicode, and property-ordering cases) produce byte-identical output.
- 20,000 randomly generated documents (control characters, U+2028/2029, supplementary-plane characters, near-boundary safe integers, nesting) are byte-identical to the JavaScript reference implementation `canonicalize` 2.1.0 by an RFC 8785 co-author.
- UTF-16 ordering is correct where it differs from code-point order (`U+1F600` sorts before `U+E000`).
- Native Go fuzzing ran 33 million inputs in 5 minutes with no idempotence or meaning-change failures.
- Every rejection case fails for the intended reason.

## Consequences

- The independent reference inputs currently live outside the repository. A default spike test run skips those two checks when scratch data is absent; this does not reproduce the historical differential/vector results above. If `SPIKE_SCRATCH` is explicitly set, missing inputs now fail. M0 must vendor/license the reference vectors and make its independent conformance checks mandatory in CI.

- `internal/canon` keeps these vectors, the differential test, and the fuzz target as permanent tests.
- The no-exponent rule is stricter than "no fractional numbers": a record containing `1e2` is rejected even though its value is integral. This keeps a single valid spelling for every number Ledger accepts.
