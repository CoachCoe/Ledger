# 0020 — Evidence execution, exact binding and criteria status

Status: accepted for M3.1; sections for M3.3-M3.5 are appended as those steps land. Builds on ADR 0012 (pinned evidence and selection) and ADR 0008 (root-authenticated sessions).

## M3.1: definitions and exact binding

Check definitions are validated by `evidence.ValidateDefinition` when `intent.create` pins them. Rules beyond the record schema: names match the record name pattern; the working directory is a clean, NFC, repository-relative path (no absolute path, `..`, `.`/empty segment, backslash or NUL); commands and setup are non-blank without NUL; allowlisted variable names are valid and unique; timeout is 1..86400 s; `output_limit_bytes` is at most `MaxOutputLimit` (64 MiB). The schema permits 1 GiB, so the stricter cap is a run-time safety limit and a definition above it is rejected at pinning with `invalid_checks`. Only definitions a criterion references are validated, so an unused broken entry in `checks.json` does not block an unrelated intent. Judgment-category definitions can be pinned but are never runnable (`evidence.Runnable`); M3.3 enforces that when a run is requested.

A binding is the triple manifest id, bound commit SHA and definition hash(es). `evidence.resolve` is read-only and accepts either `{change_id}` or `{manifest, commit?}`. A change id resolves once, to the workspace's current proposal, and the response (`frozen: true`) carries the manifest, commit, tree, base commit, intent, workspace, author session and each pinned definition with the criteria it serves. Callers use those values from then on. A later rework creates a new manifest; the earlier manifest still resolves to its original commit and definitions. A supplied commit that differs from the commit bound to the manifest fails with `binding_mismatch`. The pinned definition is read from the stored `check_definition` record, so later edits to `.ledger/checks.json` cannot change it.

`verify.CheckBinding` reports the trailer, tree and parent conditions separately as `ok`, `mismatch` or `unavailable` (AC-057). The trailer is the last paragraph of the commit message, parsed literally, with exactly one `Ledger-Manifest:` line naming the manifest.

Deviations and ambiguities:
- The parent condition requires exactly one parent equal to the manifest base commit, not merely a first parent equal to it. ADR 0010 makes every proposal a sole-parent replacement commit, so a merge-shaped commit is not a Ledger proposal.
- `evidence.Resolve` takes no `*gitx.Repo`. Resolution reads only the index; the commit object is first read by the verification worktree (M3.2), where an absent object is `dependency_unavailable`.
- `index.Proposal` gains the proposal receipt's `manifest`, so a change id resolves without a second lookup.

## M3.2 verification worktrees

- `evidence.Prepare` creates a detached worktree at `.ledger/runtime/verify/<run-id>/`, after reading the bound commit and checking trailer, tree and parent (`verify.CheckBinding`). Nothing is created if any condition fails. After checkout it compares `HEAD^{tree}` and a tracked-files tree (temporary index, `read-tree` + `add -u` + `write-tree`) with the manifest tree. Untracked and ignored files are allowed; modified, deleted and mode/type-changed tracked files are not.
- Deviation from the plan text: a binding failure is `binding_mismatch` except when only the tree condition fails, which is `tree_mismatch` (AC-091 names tree mismatch as its own abort). All three conditions are always in the error details. Dirty tracked files are `dirty_checkout`.
- Run journals (`.ledger/runtime/evidence/<run-id>.json`, runtime schema `evidence_run`) move `preparing -> ready`, or `aborted` with the error code. `Reconcile` (called from `core.New`) marks non-terminal journals `interrupted` and removes the worktree only if its journalled path is exactly the run's verify path and git lists it as a detached worktree; other paths are never touched. `system.status` reports `interrupted_runs`. A fault hook error simulates a crash and skips cleanup.
- Worktree removal on abort uses `context.WithoutCancel`, so a cancelled request still cleans up.

## Decisions for M3.3-M3.5 (recorded now so later steps cannot drift)

1. Verifier sessions are provisioned per run by the daemon: an ephemeral root-signed `service` session with no path authority, ended after the run, so the signer is never the author.
2. `evidence.run` does not hold the service lock while the child runs: resolve and journal under the lock, execute unlocked, publish under the lock using the original operation id so a retry replays the frozen result.
3. A run journal under `.ledger/runtime/evidence/` is reconciled at startup; an unfinished run becomes `interrupted` and is reported, never turned into an attestation.
4. Judgment attestations bind the criterion and revision rather than a pinned definition. The reviewer kind comes from the authenticated signing session, never from a caller-supplied label.
5. The output digest covers the first `output_limit_bytes` of combined stdout/stderr; the total size and a truncation flag are recorded.
6. The child environment contains only allowlisted variables; the attestation stores digests of their values, never the values.
7. A `pass` is applicable only if setup (when present) and check both exited 0 without timing out.
8. Independence: the signer is not the manifest author and not any session ever bound to the manifest's workspace.
9. Criterion status over an explicit evidence set: no applicable evidence is outstanding, any applicable failure is failed (passing results listed as conflicting), otherwise satisfied. Known contrary evidence is always returned.
10. `selection.record` does not move a workspace to `selected`: that terminal transition belongs to M5.4's single resolution group, so AC-051 stays partial until then.
