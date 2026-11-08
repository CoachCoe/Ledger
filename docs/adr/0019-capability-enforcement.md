# 0019 — Cooperative capability enforcement and historical authority

Status: accepted. Implements M2; retains ADR 0008's root-authenticated sessions.

Capabilities accept exact paths, directory prefixes ending in `/**`, or `**`.
Matching uses NFC and the init-time filesystem case policy; case folding uses
Unicode case folding. Paths with traversal segments, absolute paths, backslashes,
colons, NUL, or unsupported glob syntax are rejected rather than cleaned.
Every child allow must be structurally covered by a parent allow; expiry and
dependency authority cannot grow. Effective permissions intersect every link,
so inherited denies cannot be removed by omission in a child.

Session creation/recovery accepts optional scope fields. Omitted scopes retain
M1's whole-repository defaults for compatibility; callers should request narrow
scopes explicitly. An additional `capability.issue` or `capability.delegate`
capability targets an existing root-authenticated session. Delegation is signed
by the authenticated parent holder, never used as a substitute for root session
key authentication. A workspace records its chosen capability; a recovery binding
replaces that authority without changing historical authorship. Recovery accepts
an optional `previous_binding` compare-and-swap precondition. With no precondition,
each root recovery deliberately binds from the latest state under the store lock.

Observation warns; proposal rejects. Proposal uses the stable private-index
snapshot and checks both immutable base and candidate tree entries, including
symlink targets read by blob id. Raw Git filenames are preserved for lookups;
matching uses normalized names, while immutable manifest paths are NFC-normalized before signing. Multiple raw names with the same NFC representation are rejected. Following live disk
symlinks would permit post-capture edits to change authorization of signed bytes.
Two frozen tree captures detect concurrent edits; no atomic filesystem claim is made.

Notification enqueue and ack are individual transactional SQLite statements.
Warning ids derive from binding, path and failure code; repeated observation,
including after acknowledgment, preserves the original warning. A new binding
gets its own attribution. Ordinary restart preserves queues. Explicit reindex
reports discarded runtime row counts and the next observation regenerates current
scope warnings. Historical transient warnings and acknowledgments cannot be rebuilt.
Polling uses an inclusive creation timestamp (`since`); clients use ids to deduplicate
entries sharing timestamps, so a subsequent enqueue in the same millisecond is
not skipped.

Replay validates chain authority at receipt time, not verification wall-clock
time. The in-memory M2 verifier reads immutable records without SQLite and reports
signature/authorization integrity separately from configured root trust. It checks
manifest file claims; complete Git-object and bundle closure verification belongs
to M3. Imported misattributed lifecycle events remain inspectable rejected history,
as required by the existing recovery contract.

Alternative: make every delegated session creation parent-signed. Rejected because
session public keys must remain authenticated by the human root. Alternative: OS
write prevention. Deferred to a hosted execution isolation boundary; local paths
and the daemon remain controlled by the same OS user.
