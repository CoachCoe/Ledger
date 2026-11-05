# Canonicalization fixtures

- `rfc8785/`: test vectors from the RFC 8785 author's reference repository, <https://github.com/cyberphone/json-canonicalization> at commit `32a48195b9b20fe67b1795e39e4509f14d860b45`, under the Apache License 2.0 (`rfc8785/LICENSE`). Some inputs contain fractional numbers, so they test the underlying canonicalizer rather than Ledger's input policy.
- `js-reference.jsonl`: 400 random documents within Ledger's input policy. Each line holds an `input` (indented JSON with Go-style escapes) and the `canonical` output of the JavaScript reference implementation `canonicalize` 2.1.0 (Apache-2.0, by an RFC 8785 co-author).
