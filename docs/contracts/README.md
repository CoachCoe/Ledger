# Phase 1 execution contracts

Ledger is a local, provider-neutral control plane over Git. Git stores code; immutable signed Ledger records store intent, authority, revisions, evidence, selection and lifecycle history. SQLite and live observations are rebuildable local state. Selection never means acceptance or merge authorization.

D0 is documented in ADRs [0007](../adr/0007-record-envelope-and-identifiers.md) through [0015](../adr/0015-speculation-frozen-inputs.md), building on the six Stage 0 ADRs. The [recovery table](recovery.md) assigns every external side effect a failure/retry obligation. The [worked reference graph](reference-graph.json) shows the hash dependency order and stable-id resolutions without a self-hash or receipt cycle. Run `python3 tools/check_contract_graph.py` to check that example's dependencies, unique ids, lifecycle predecessor and complete-group visibility.

The graph is a symbolic contract example: aliases stand for eventual object hashes, and envelope signatures are not generated. It does not claim schema or cryptographic conformance. M0 must add closed schemas and independently generated real signature/hash fixtures before product record handling is accepted. The working milestone plans and complete acceptance matrix live under the intentionally ignored `Untracked Docs/plans/` as required by the build prompt; no product milestone is complete yet.

An independent verifier performs these steps without SQLite:

1. Validate raw JSON/JCS input policy, version/kind schema, canonical object hash, exact domain-separated signature bytes, and public-key encoding/fingerprint.
2. Resolve stable ids to unique authenticated creation hashes. Root-authenticate session keys/participant kinds and capability holder chains. Do not recursively hash stable-id references between session and capability.
3. Validate complete signed receipt groups and request identities. A valid member without a published complete receipt is pending. Receipts name member hashes, never themselves; members do not name receipt hashes.
4. Follow authenticated creation and lifecycle/binding predecessor hashes. Check actors and authority at claimed operation times. Preserve semantic forks as conflicts and block ambiguous subjects; do not choose by timestamp.
5. Verify manifest trailer, exact Git tree, and sole fixed-base parent separately. Evidence binds that manifest/commit and pinned check definition; selection recomputes explicit-set criterion status and reports known contrary evidence.
6. Report integrity, trust, unavailable dependencies and conflicts distinctly. Public keys in the bundle do not grant trust. A complete export supplies all provenance groups and reachable Git objects without the source repository, daemon, runtime journals or network.

D0 does not implement a daemon, record store, schemas, import, or evidence execution. Those remain M0–M6 acceptance work. M1 starts with real root-authenticated sessions and whole-repository capabilities; M2 enables scoped/delegated authority after enforcement tests pass. The owner-led post-M3 customer checkpoint and commercial validation benchmark remain separate gates.
