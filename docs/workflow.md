# Local workflow and API

Ledger runs a per-repository daemon that holds session keys, observes workspaces, and serves a versioned API. The CLI is a thin client over that API and starts the daemon on demand. Git remains canonical for code; Ledger records who was authorized to do what, against which intent, and what was proposed.

Requirements: Go 1.27.3 or newer to build, Git 2.25 or newer, and on macOS cgo with Apple's command-line C toolchain (FSEvents). Linux uses inotify. Bare repositories and separate Git directories are rejected. The repository path must fit the operating system's Unix-socket path limit (104 bytes on macOS, 108 on Linux, including `/.ledger/runtime/ledgerd.sock`).

## A complete session

```sh
ledger init
ledger session create '{"agent_name":"my-agent"}'
ledger intent create '{"description":"Implement refunds","criteria":[{"id":"review","statement":"Review the implementation","verification":{"kind":"judgment","reviewer":"human"}}]}'
ledger workspace fork --session ses_... '{"intent_id":"int_...","capability_id":"cap_..."}'
# edit files in the returned path with any tool
ledger workspace status '{"workspace_id":"wsp_..."}'
ledger workspace propose --session ses_... '{"workspace_id":"wsp_...","plan":"Add refunds"}'
ledger workspace rework --session ses_... '{"workspace_id":"wsp_..."}'
ledger workspace close --session ses_... '{"workspace_id":"wsp_...","reason":"finished"}'
ledger daemon stop
```

The ellipses stand for the full ids returned by earlier calls. Flags precede the optional JSON input. `--session` names the acting session; without it the call is made by the root key. Every mutating call takes `--operation-id op_<26 base32 characters>`, or the CLI generates one: retrying the same id with the same request returns the original result, and reusing it with a different request is rejected.

Mutations print `{"ok":true,"result":{"operation_id":...,"receipt":...,"result":{...},"members":[...],"replayed":false}}`, with the domain ids in `result.result`. Reads print their result directly. Errors carry a stable `code`, a `message`, and optional `details`.

| Operation | Caller | Effect |
|---|---|---|
| `session create` | root | Sign a session and a whole-repository capability that expires after 24 hours or at `expires_at`. The session key lives only in daemon memory. |
| `session end` | root or the session | End the session. |
| `session recover` | root | Issue a replacement session for an open or proposed workspace and rebind it. Earlier authorship is preserved. |
| `intent create` | root or session | Create an intent with at least one criterion, optionally under `parent_id`. |
| `intent status` | anyone | Return the intent and its derived state. |
| `intent close` | root or owning session | Close as `completed`, `abandoned`, or `superseded` (with `replacement_id`). |
| `workspace fork` | session | Create a detached worktree at `base_commit` (default `HEAD`) for a leaf intent. |
| `workspace status` | anyone | Return the latest observed write set relative to the base. |
| `evidence resolve` | anyone | Return the frozen binding (manifest, bound commit, tree, pinned check definitions) for `{change_id}` or `{manifest, commit?}`. A change id resolves once; use the returned manifest and commit afterwards. |
| `evidence run` | root or an unrelated session | Run one pinned check against `{manifest, commit, check_definition, keep_worktree?}` in a fresh verification worktree and publish a signed attestation. See Evidence runs. |
| `evidence judge` | root or an unrelated session | Record a signed judgment `{manifest, commit, criterion_id, result, reason?}` for a judgment criterion. The reviewer kind comes from the signing session, never the input. |
| `evidence isIndependent` | anyone | Report whether the evidence for `{manifest, commit}` was signed by a session other than the author, and its signer kind. |
| `intent criteriaStatus` | anyone | Report each criterion as `satisfied`, `failed` or `outstanding` for `{manifest, commit, evidence?}`, with the deciding attestations and known contrary evidence. |
| `selection record` | root or an unrelated session | Sign a selection decision over `{manifest, commit, evidence, competitors?, scoring?}` with recomputed statuses. It is not acceptance. |
| `workspace propose` | bound session | Capture the workspace, sign a manifest, and publish a commit on `ledger/<intent-short-id>/<workspace-short-id>`. |
| `workspace rework` | root or bound session | Reopen a proposed workspace for another revision. |
| `workspace close` | root or bound session | Record the close, then remove the worktree if nothing would be lost. |
| `system status` | anyone | List subjects, whether session recovery is pending, and the daemon's process id. |
| `daemon stop` | root | Stop the daemon after recording the request. |

## Checks and intents

`.ledger/checks.json` is an array of complete check definitions:

```json
[{"name":"unit","category":"mechanical","setup":null,"command":"go test ./...","working_directory":".","env_allowlist":[],"timeout_seconds":60,"output_limit_bytes":65536}]
```

A criterion `{"kind":"check","check":"unit"}` pins the definition when the intent is created: the definition is stored as a signed record and its hash is written into the criterion, so later edits to the file cannot retarget the intent or a retried request. Requirements change through a superseding intent. A change attaches to exactly one leaf intent, and an intent used by a change cannot acquire children.

## Evidence runs
`ledger evidence run '{"manifest":"…","commit":"…","check_definition":"…"}'` runs the pinned definition's optional setup command, then its command, with `/bin/sh -c` in a fresh detached worktree of the bound commit under `.ledger/runtime/verify/<run-id>/`, never in the author's workspace. Each phase gets the definition's timeout; the child environment holds only allowlisted variables; output is captured up to `output_limit_bytes`. Tracked files are compared with the manifest tree before the run, after setup and after the check, so a command that modifies, deletes or re-modes a tracked file fails even if it exits 0. Untracked and ignored files are allowed.

The daemon provisions a root-signed `service` session for the run, signs the attestation with it and ends it, so the signer is never the author. The author and any session ever bound to the workspace cannot request the run. Setup failure, a non-zero exit, a timeout, cancellation (Ctrl-C on the CLI, which has no client deadline for this operation) and tracked mutation each publish a failed attestation with `reason` `setup_failed`, `setup_timeout`, `check_failed`, `check_timeout`, `cancelled` or `tracked_mutation`; setup and check status and duration are recorded separately. A checkout that fails verification (`binding_mismatch`, `tree_mismatch`, `dirty_checkout`) aborts without an attestation. `keep_worktree` is `never` (default), `on_failure` or `always`.

If the daemon dies, or the verifier key is lost, before the attestation is published, no attestation exists. `system status` lists the run under `interrupted_runs`; retrying the same operation id returns `run_interrupted`, and a new operation id starts a fresh run. Attestations always carry `isolation: "none"`: Phase 1 evidence is not isolated (see `docs/security-model.md`).
## Observation

Each open workspace is observed through a private Git index under `.ledger/runtime/`, so the write set is the full difference from the fixed base: tracked and untracked non-ignored files, deletions, both sides of renames, modes, symlinks, and commits the agent made itself. Observation is triggered by filesystem events (FSEvents or inotify) coalesced for 500 ms, by any call made by the workspace's session, and by a fallback poll every 10 seconds. `workspace status` returns the cached result and its observation time.

After a restart the daemon re-observes current files, so edits made while it was stopped are detected; edits made and reverted while it was stopped cannot be.

`ledger daemon` runs in the foreground with `--idle` (default `10m`), `--poll` (default `10s`), and `--no-watch` for polling only. An auto-started daemon logs to `.ledger/runtime/ledgerd.log` and exits after the idle period when no workspace is open and no client is connected.

## Proposals

A workspace has one fixed base and a stable change id. Each proposal is a replacement commit whose sole parent is the base, carrying the trailers `Ledger-Change`, `Ledger-Intent`, `Ledger-Session`, and `Ledger-Manifest`. The manifest names the tree, not the commit that embeds it. The branch name uses each id's prefix and first eight characters, for example `ledger/int_5i7943qg/wsp_39jidong`. The branch advances by compare-and-swap against the last proposal Ledger recorded, so commits added to it outside Ledger are never overwritten. Every revision stays reachable at `refs/ledger/proposals/<manifest-hash>`, so later evidence remains resolvable after rework and garbage collection; share them with `git push origin 'refs/ledger/proposals/*'`. An identical manifest reuses its original commit.

Capture uses two independent private indexes and retries when files change mid-capture; it does not claim an atomic filesystem snapshot. Submodule changes are rejected.

## Sessions and recovery

Session private keys exist only in daemon memory. When the daemon restarts, sessions whose keys were lost are ended with root-signed events; if the root key is unavailable, `system status` reports `recovery_pending` and those sessions cannot sign. Work continues only through `session recover`. Closed and selected workspaces cannot be rebound.

Close publishes the terminal state before cleanup. Modified, untracked, and ignored files are preserved: if any remain, the call fails with `cleanup_pending`, `committed: true`, the operation id and the receipt, and `ledger recover` reports the outstanding cleanup until the files are dealt with and recovery is retried. Branches and retained revisions survive closure.

## Protocol

Clients POST one request to `/v1` over `.ledger/runtime/ledgerd.sock`:

```json
{"api_version":1,"operation":"workspace.status","operation_id":"","session_id":"","input":{"workspace_id":"wsp_..."}}
```

Requests are limited to 1 MiB. Input and output schemas for every operation are in [`schemas/v1/api/`](../schemas/v1/api/). The socket is mode 0600 inside a 0700 directory, so only the repository owner's processes can connect; there is no network listener and no model SDK dependency.

## Current limits

Enforcement is cooperative: direct disk writes are observed, not prevented, and a session signature records that Ledger acted under that session's authority, not that a particular agent process held the key. Evidence is not isolated (see [security-model.md](security-model.md)).

## Criteria, judgment and selection
After evidence runs, `ledger intent criteriaStatus '{"manifest":"…","commit":"…"}'` evaluates every criterion against the evidence for that exact manifest, commit and tree. A check criterion is satisfied only by an independent attestation of its pinned definition; a judgment criterion only by an independent judgment of the stated reviewer kind (`ledger evidence judge`, signed by a root-provisioned `human` session when root calls it). All-pass is `satisfied`, any fail is `failed` (mixed results list the `conflicting` attestations), and nothing applicable is `outstanding`. Failing evidence is never hidden: it is listed under `contrary_evidence` even when an explicit `evidence` set leaves it out. Evidence for another revision is rejected with `cross_revision_evidence`.
`ledger selection record '{"manifest":"…","commit":"…","evidence":["…"]}'` signs a decision recording the evidence set, recomputed per-criterion statuses, outstanding criteria and contrary evidence. The input has no status members, so a caller cannot claim a criterion is satisfied, and the index re-derives every decision when it is published.
Selection is not acceptance. A decision is one signed record: it moves no branch, tag, proposal ref or `HEAD`, creates no commit, merges nothing, and leaves the intent and workspace states unchanged (only a speculation run moves a workspace to `selected`). Merging remains an ordinary Git action taken by a person or another tool.
## Scoped capabilities and warnings

Session creation and recovery now accept path scopes. Capability issuance, delegation, polling and acknowledgment are available through the CLI and versioned socket API. See [security-model.md](security-model.md) for inputs, matching rules, dependency filenames, recovery semantics and limitations.

## Symbol coordination and leases

JavaScript, TypeScript/TSX and Python analysis uses the official tree-sitter grammars. A symbol locator combines path, language, lexical qualified name, declaration kind and an optional disambiguator. Body/signature edits preserve the locator; renames and moves are deletion/addition. Proposals analyze the exact frozen base/candidate Git blobs and record written symbols and best-effort static read references. Optional `context_refs` entries of the form `path#lexical.name` also contribute declared reads. This describes referenced code, not what an agent actually read.

`ledger coordination status '{"workspace_id":"…"}'` refreshes a workspace and returns its written/read locators, direct write and statically inferred read edges, unresolved references and file-level fallbacks. Unsupported files, parser errors, ambiguous declarations and unmapped changes retain explicit fallback reasons. Local relative imports and Python repository modules are resolved only when unique; external packages, re-exports, dynamic calls, type-driven dispatch and unrecognized/shadowed bindings remain best effort. Analysis is always marked `advisory: true`; missing edges do not prove independence. Sources over 4 MiB fall back to file level.

Active workspace observers emit symmetric `overlap` notifications for write/write and write/read conflicts. Symbol notifications identify the locator, overlap kind, confidence and counterpart workspace/session/intent. Conservative file-level notifications include their reason. `ledger coordination notifications '{"session_id":"…","since":"…"}'` returns unacknowledged items; the inclusive timestamp cursor permits client deduplication by id. Acknowledge one item with `notification_id`, or a batch atomically with `notification_ids`, through `ledger coordination ack --session SESSION '{"session_id":"…","notification_ids":["…"]}'`. Acknowledgments remain stored across ordinary restart; repeated observation and acknowledgment are safe. Explicit reindex reports lost delivery state and the next observation regenerates current warnings for active owners.

`ledger coordination lease --session SESSION '{"session_id":"…","paths":["src/api.ts"],"symbols":[],"reason":"review signature","expires_at":"…"}'` creates a runtime advisory lease. Paths are literal repository-relative paths within the holder's capability; symbol scopes use the same locator objects as manifests. Supply a stable `--operation-id` for retries. Expiry must be in the next 24 hours. `coordination leases '{}'` lists current leases; `coordination release --session SESSION '{"session_id":"…","lease_id":"…"}'` releases one, idempotently. Writes to another session's leased code warn both parties and still allow proposal publication. Reads alone do not conflict with a lease. Expiry, capability/session loss, daemon restart and reindex remove stale leases; exported history never restores them.

Status and lease responses contain `runtime: true`, `observed_at` and a canonical `snapshot_digest` computed before adding that digest field. These cite a local unsigned observation. Notifications, analyses, edges and leases are excluded from portable bundles; only the manifest's signed symbol locators travel with its provenance.

## Speculation runs

A speculation run asks several attempts to satisfy one intent and selects among them by score. Ledger hands out the attempts and never launches an agent. The intent must have at least one check criterion.

```sh
ledger speculation start '{"intent_id":"…","strategies":[{"name":"careful","spec":"…"},{"name":"quick","spec":"…"}],"scorer":{"kind":"command","command":"python3 /path/score.py","timeout_seconds":60,"env_allowlist":["PATH"]},"deadline_seconds":3600}'
ledger speculation submit --session ATTEMPT_SESSION '{"run_id":"…","workspace_id":"…"}'
ledger speculation evaluate '{"run_id":"…"}'
ledger speculation status '{"run_id":"…"}'
```

`start` returns each attempt's workspace path, session, capability, strategy and spec. `n` (default: one attempt per strategy) assigns strategies round-robin. Give exactly one of `deadline` and `deadline_seconds`. Each attempt's agent edits its worktree and submits once, from its own session, before the deadline; the submitted manifest is the frozen candidate and the workspace cannot be reworked, closed or selected by hand until the run ends.

`evaluate` closes collection, runs every pinned check on every candidate in a fresh verification worktree, and scores the candidates that satisfy every check criterion. A `command` scorer reads the candidate document from `$LEDGER_SCORER_INPUT` and writes `{"scores":{"<manifest>":<integer>,…}}` to `$LEDGER_SCORER_OUTPUT`; it also gets `LEDGER_REPOSITORY`, `LEDGER_RUN_ID` and its allowlisted variables, and nothing else. With a `submission` scorer, `evaluate` returns `awaiting_scores: true` and the candidates, and the run waits for `ledger speculation score '{"run_id":"…","scores":{…}}'`. The highest score wins; ties go to the lowest manifest hash. Evaluation keeps running if the client disconnects.

The run ends in one signed group: the winner's selection and `selected` transition, and for every other attempt an attempt record (`failed_criteria`, `lost_tiebreaker`, `timeout`, `not_proposed` or `cancelled`) and a `closed` transition. If no candidate is eligible, the run resolves with no winner. `ledger speculation cancel '{"run_id":"…","reason":"…"}'` ends a run with no winner, stopping an evaluation in progress. A scorer failure, timeout or malformed scores interrupt the run with no winner; `ledger speculation resume '{"run_id":"…"}'` returns it to evaluation with the same frozen inputs. A daemon restart interrupts a run that was evaluating; a collecting run keeps collecting, and `session.recover` replaces a lost attempt session.

Losing worktrees are removed after the run ends unless they hold edits beyond the submitted candidate, in which case they are kept and the call reports `cleanup_pending` with `committed: true`. Nothing is merged. See [ADR 0023](adr/0023-speculation-runs.md).

## Export, import and verify

For MCP stdio setup, the complete tool inventory, checked text editing and
portable history/live coordination queries, see the [agent integration
contract](contracts/agents.md). `ledger history why|attempts|dependents` and
`ledger coordination nearby` expose the same query inputs through the CLI.

`ledger export --out FILE [--manifest HASH]... [--intent ID]... [--allow-partial]` writes a versioned bundle (an uncompressed tar: `bundle.json` inventory, `records/<aa>/<hash>.json`, and a self-contained `git/objects.bundle` with no prerequisites). The default exports every committed group; `--manifest` and `--intent` export a closure (author and verifier sessions with their capability chains, root and session public keys, workspace, bindings and lifecycle events, intent, check definitions, receipts, evidence and selections). A missing dependency fails with `export_incomplete`; `--allow-partial` writes a bundle whose inventory says `complete: false`. Private keys, trust lists, journals, runtime state, SQLite and uncommitted records are never exported.

`ledger verify [--bundle FILE] [--trust FINGERPRINT]... [--trust-file PATH] [--strict]` reports integrity (`verified`, `invalid`, `unavailable`) separately from trust (`trusted`, `untrusted`, `unknown`). Without `--bundle` it verifies this repository's `refs/ledger/records`. Trust comes only from the local list (default `$XDG_CONFIG_HOME/ledger/trusted-roots.json`, else `~/.config/ledger/trusted-roots.json`, format `{"version":1,"roots":[{"fingerprint":"<hex>","label":"..."}]}`) and `--trust`; keys inside a bundle never add trust. `export`, `verify` and `reindex` need neither the daemon nor SQLite. `--strict` returns `verification_failed` unless integrity is `verified`.

`ledger import FILE` stages the bundle in an inert private directory, verifies it, retains the proposal commits under `refs/ledger/proposals/<manifest>` (create-only, never overwriting a different commit) and publishes the records through the RecordStore. Importing again changes nothing; divergent histories are kept and reported as conflicts. Nothing is trusted and no key, config, hook or code branch is imported.

Proposal revisions live in refs that ordinary `git fetch` and `git push` do not move. To share them (and the records ref) between clones use explicit refspecs:

```sh
git fetch origin 'refs/ledger/proposals/*:refs/ledger/proposals/*' 'refs/ledger/records:refs/ledger/records'
git push origin 'refs/ledger/proposals/*:refs/ledger/proposals/*' 'refs/ledger/records:refs/ledger/records'
```

Run `ledger recover` after fetching the records ref so fetched groups are indexed. Fetching the records ref is equivalent to importing a bundle, so apply the same trust judgement.
`export` reports its size honestly (`bundle_bytes`, `record_bytes`, `git_bytes`, record and commit counts). The Git part carries the full reachable history of each base commit, so a bundle of a large repository is large; Phase 1 does not trim it. A file ending in `.partial` is an interrupted export and is never verified or imported. `demos/m3.sh` verifies a bundle in a fresh directory with the network disabled and the source repository unreadable, then imports it into a fresh repository.
