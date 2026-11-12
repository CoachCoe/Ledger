# Phase 1 execution contracts

The [agent integration contract](agents.md) specifies MCP setup, JSON envelopes, session/retry identities, lifecycle handoff and history/runtime provenance.

Ledger is a local, provider-neutral control plane over Git. Git stores code; immutable signed Ledger records store intent, authority, revisions, evidence, selection and lifecycle history. SQLite and live observations are rebuildable local state. Selection never means acceptance or merge authorization.

The execution contracts are specified in ADRs [0007](../adr/0007-record-envelope-and-identifiers.md) through [0015](../adr/0015-speculation-frozen-inputs.md) (speculation as implemented: [0023](../adr/0023-speculation-runs.md)), building on the platform ADRs 0001–0006. The [recovery table](recovery.md) lists every external side effect and the recovery it requires. Record shapes are in [`schemas/v1/`](../../schemas/v1/) and, for native repositories, [`schemas/v2/`](../../schemas/v2/) ([ADR 0027](../adr/0027-record-schema-v2.md)), with signed examples in [`schemas/examples/`](../../schemas/examples/) (ADR 0016); independently generated signature and canonicalization fixtures are under `internal/envelope/testdata/` and `internal/canon/testdata/`.

An independent verifier performs these steps without SQLite:

1. Validate raw JSON/JCS input policy, version/kind schema, canonical object hash, exact domain-separated signature bytes, and public-key encoding/fingerprint.
2. Resolve stable ids to unique authenticated creation hashes. Root-authenticate session keys/participant kinds and capability holder chains. Do not recursively hash stable-id references between session and capability.
3. Validate complete signed receipt groups and request identities. A valid member without a published complete receipt is pending. Receipts name member hashes, never themselves; members do not name receipt hashes.
4. Follow authenticated creation and lifecycle/binding predecessor hashes. Check actors and authority at claimed operation times. Preserve semantic forks as conflicts and block ambiguous subjects; do not choose by timestamp.
5. Verify manifest trailer, exact Git tree, and sole fixed-base parent separately. Evidence binds that manifest/commit and pinned check definition; selection recomputes explicit-set criterion status and reports known contrary evidence. Implemented by `verify.CheckBinding`, `verify.Evidence` and `verify.Selection` in `internal/verify`.
6. Report integrity, trust, unavailable dependencies and conflicts distinctly. Public keys in the bundle do not grant trust. A complete export supplies all provenance groups and reachable Git objects without the source repository, daemon, runtime journals or network. Implemented by `verify.Verify` (report), `verify.Export` and `verify.Import`; `ledger verify|export|import` run without the daemon or SQLite.
