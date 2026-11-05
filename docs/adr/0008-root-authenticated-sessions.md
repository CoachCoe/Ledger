# ADR 0008: Root-authenticated sessions and authorization

- Status: Accepted
- Date: 2026-11-05

## Context

Agents act through short-lived sessions, but a session key alone proves nothing: something must bind each session's key, participant kind, and capability to the human root.

## Decision

The local Ed25519 root private key lives outside repositories, by default `~/.config/ledger/keys/`, with private directories and 0600 key files. Only public keys/fingerprints enter records. `IdentityProvider` separates local custody from future identity providers. Imported keys never become trusted by being present.

Generate the session id, capability id, and ephemeral key before creating records. A root-signed capability names its intended session **id**; a root-signed session creation names its capability **id**, public key, participant kind, and start time. Their object hashes do not depend on each other. Authentication is evaluated as a pair, not by recursively hashing their stable-id references. Root authority authenticates human/agent/service kind; descriptive agent/model labels do not prove process identity.

All provenance, including intent/check creation and receipts, is signed. Root or authenticated sessions may create intents/checks. Root capabilities are root-signed; delegation is signed by the authenticated holder of the parent capability and names the parent id. A manifest signer must be its author session and current workspace binding holder; an attestation signer must be its named independent verifier. A lifecycle/binding actor must match the signer and be authorized for the subject. Root may reconcile lost sessions and authorize explicit recovery. Unrelated sessions cannot mutate a subject merely by knowing its id. M0 defines actor checks; narrower path/delegation enforcement arrives in M2.

The holder must be active and the entire capability chain must authorize the operation at its recorded time. Check paths under every link, inherited denies, expiry, dependency permission, and delegation narrowing. Record creation, lifecycle history, and committed receipts supply the evidence; SQLite supplies none of the authority. Local times are claims, not a trusted time service; ambiguous/conflicting histories fail closed. Report expiry at operation separately from expiry now.

Session keys remain only in daemon memory from M1 onward and disappear at end/restart. In M0, in-process primitives and genuine root signatures suffice; in M1 real root-issued whole-repository capabilities precede workspace operations. Narrow scopes/delegation are not exposed before M2 enforcement. A session signature means Ledger recorded an operation under that session and authority; it does not prove which agent process performed it. Direct disk access is cooperative, not isolated.

## Alternatives considered

Self-signed session creation permits key substitution. Persisting session keys or silently replacing them defeats the recovery/authorship contract. Bundled public keys are evidence, not trust grants.

## Validation

M0.4/M0.10, M1.2a, M2.3/M2.6: substitute session key, wrong holder/actor, unauthenticated reviewer kind, expired ancestor, widening, root unavailable, and historical verification after expiry.
