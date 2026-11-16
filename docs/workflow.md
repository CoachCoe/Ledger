# Local workflow and API

Ledger runs a per-repository daemon that holds session keys, observes workspaces, and serves a versioned API. The CLI is a thin client over that API and starts the daemon on demand. A repository keeps its code in Ledger's own content store; Ledger records who was authorized to do what, against which intent, and what was proposed.

Requirements: Go 1.27.3 or newer to build, and on macOS cgo with Apple's command-line C toolchain (FSEvents). Linux uses inotify. Git is not needed except by the Git bridge. The repository path must fit the operating system's Unix-socket path limit (104 bytes on macOS, 108 on Linux, including `/.ledger/runtime/ledgerd.sock`).

`ledger init` makes the current directory a native repository: it imports every file that `.gitignore` and `.ledgerignore` rules do not exclude as the repository's first tree, and signs the root public key and a `repository` record naming that tree and the case policy of the filesystem. That tree is the head of line `main` until a signed `line_advance` moves it. Commands run anywhere inside the directory find it. A `.ledger/` without a content store is a Phase 1 repository: this binary runs `status`, `verify` and `export` there, and refuses every command that would change it, the daemon and the MCP server with `migrate_required` until `ledger migrate` converts it (see [Migrating a Phase 1 repository](#migrating-a-phase-1-repository)).

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
| `session create` | root | Sign a session and a whole-repository capability that expires after 24 hours or at `expires_at`, and may advance the lines in `may_advance` (default none). The session key lives only in daemon memory. |
| `session end` | root or the session | End the session. |
| `session recover` | root | Issue a replacement session for an open or proposed workspace and rebind it. Earlier authorship is preserved. |
| `intent create` | root or session | Create an intent with at least one criterion, optionally under `parent_id`. |
| `intent status` | anyone | Return the intent and its derived state. |
| `intent close` | root or owning session | Close as `completed`, `abandoned`, or `superseded` (with `replacement_id`). |
| `workspace fork` | session | Clone a copy-on-write workspace at `base_tree` (default: the head of line `main`) for a leaf intent. |
| `workspace status` | anyone | Return the latest observed write set relative to the base. |
| `workspace log` | anyone | Return the latest `limit` (default 50, at most 500) operation log entries, each with the files and symbols it changed. See Checkpoints and undo. |
| `workspace restore` | bound session | Write the tree of an earlier log `entry` back into an open workspace as a new `restore` entry. See Checkpoints and undo. |
| `evidence resolve` | anyone | Return the frozen binding (manifest, tree, base tree, pinned check definitions) for `{change_id}` or `{manifest}`. A change id resolves once; use the returned manifest afterwards. |
| `evidence run` | root or an unrelated session | Run one pinned check against `{manifest, check_definition, keep_worktree?}` in a fresh checkout of the manifest's tree and publish a signed attestation. See Evidence runs. |
| `evidence judge` | root or an unrelated session | Record a signed judgment `{manifest, criterion_id, result, reason?}` for a judgment criterion. The reviewer kind comes from the signing session, never the input. |
| `evidence isIndependent` | anyone | Report whether the evidence for `{manifest}` was signed by a session other than the author, and its signer kind. |
| `intent criteriaStatus` | anyone | Report each criterion as `satisfied`, `failed` or `outstanding` for `{manifest, evidence?}`, with the deciding attestations and known contrary evidence. |
| `selection record` | root or an unrelated session | Sign a selection decision over `{manifest, evidence, competitors?, scoring?}` with recomputed statuses. It is not acceptance. |
| `line advance` | root or a session with `may_advance` for the line, never the author | Accept a selected manifest onto `line` (default `main`) with a signed `line_advance`. See Acceptance. |
| `line status` | anyone | Return a line's last advance (`null` before the first) and the tree workspaces fork from. |
| `workspace propose` | bound session | Capture the workspace as a durable `propose` entry in its operation log and sign a manifest naming that tree, its base tree and the entry. |
| `workspace rework` | root or bound session | Reopen a proposed workspace for another revision. |
| `workspace close` | root or bound session | Record the close and the workspace's last tree, then remove the checkout unless it holds ignored files. |
| `workspace deps detach` | bound session | Give the workspace its own writable copy of a dependency tree it shares read-only with other workspaces (`path`, or all of them). |
| `system status` | anyone | List subjects, whether session recovery is pending, and the daemon's process id. |
| `daemon stop` | root | Stop the daemon after recording the request. |

## Checks and intents

`.ledger/checks.json` is an array of complete check definitions:

```json
[{"name":"unit","category":"mechanical","setup":null,"command":"go test ./...","working_directory":".","env_allowlist":[],"timeout_seconds":60,"output_limit_bytes":65536}]
```

A criterion `{"kind":"check","check":"unit"}` pins the definition when the intent is created: the definition is stored as a signed record and its hash is written into the criterion, so later edits to the file cannot retarget the intent or a retried request. Requirements change through a superseding intent. A change attaches to exactly one leaf intent, and an intent used by a change cannot acquire children.

## Evidence runs
`ledger evidence run '{"manifest":"…","check_definition":"…"}'` runs the pinned definition's optional setup command, then its command, with `/bin/sh -c` in a fresh copy-on-write checkout of the manifest's tree under `.ledger/runtime/verify/<run-id>/`, never in the author's workspace. The checks of one speculation evaluation run in parallel, each in its own checkout. Each phase gets the definition's timeout; the child environment holds only allowlisted variables; output is captured up to `output_limit_bytes`. Tracked files are compared with the manifest tree before the run, after setup and after the check, so a command that modifies, deletes or re-modes a tracked file fails even if it exits 0. Untracked and ignored files are allowed.

The daemon provisions a root-signed `service` session for the run, signs the attestation with it and ends it, so the signer is never the author. The author and any session ever bound to the workspace cannot request the run. Setup failure, a non-zero exit, a timeout, cancellation (Ctrl-C on the CLI, which has no client deadline for this operation) and tracked mutation each publish a failed attestation with `reason` `setup_failed`, `setup_timeout`, `check_failed`, `check_timeout`, `cancelled` or `tracked_mutation`; setup and check status and duration are recorded separately. A checkout whose tracked files differ from the manifest tree (`dirty_checkout`) aborts without an attestation. `keep_worktree` is `never` (default), `on_failure` or `always`.

If the daemon dies, or the verifier key is lost, before the attestation is published, no attestation exists. `system status` lists the run under `interrupted_runs`; retrying the same operation id returns `run_interrupted`, and a new operation id starts a fresh run. Attestations always carry `isolation: "none"`: Phase 1 evidence is not isolated (see `docs/security-model.md`).
## Observation

Each open workspace is observed through a stat cache under `.ledger/runtime/`, and every changed tree is recorded as an entry in the workspace's operation log, so the write set is the full difference from the fixed base: new and changed non-ignored files, deletions, both sides of renames, modes and symlinks. Ignored dependency directories such as `node_modules` are shared read-only between workspaces of the same base until a workspace changes its dependency manifest or detaches them; a shared copy no workspace links to is deleted when a workspace is removed and when the daemon starts. Observation is triggered by filesystem events (FSEvents or inotify) coalesced for 500 ms, by any call made by the workspace's session, and by a fallback poll every 10 seconds. `workspace status` returns the cached result and its observation time.

After a restart the daemon re-observes current files, so edits made while it was stopped are detected; edits made and reverted while it was stopped cannot be.

`ledger daemon` runs in the foreground with `--idle` (default `10m`), `--poll` (default `10s`), and `--no-watch` for polling only. An auto-started daemon logs to `.ledger/runtime/ledgerd.log` and exits after the idle period when no workspace is open and no client is connected.

## Checkpoints and undo

Every observed tree is already a checkpoint: there is nothing to commit. `ledger workspace log '{"workspace_id":"…"}'` returns the newest entries of the workspace's operation log in order, each with its hash, kind (`snapshot`, `write`, `restore`, `propose`, `rework`), tree, trigger and session, the files that differ from the entry before it (`A`, `D`, `M` or `T`, as Git reports them) and the symbols those changes wrote. The entries and trees are exact; symbol deltas are advisory, like all symbol analysis. `total` counts every entry. The log outlives the workspace, so it can be read after close.

`ledger workspace restore --session SESSION '{"workspace_id":"…","entry":"…"}'` returns an open workspace to the exact tree of one of its entries. The current state is recorded first and the restore is appended as a new entry naming its `source`, so later entries, and the edits they hold, stay in the log and can be restored in turn. Ignored files, such as dependency trees and build output, are left alone. Ledger writes the restored files on the session's behalf, so every path the restore would change must be within the workspace's capability (`scope_violation` lists the paths otherwise, and nothing changes); a restore that changes a dependency manifest first gives the workspace private dependency trees. A proposed workspace must be reworked before it can be restored. Like `workspace write`, a restore is unsigned runtime state: retrying an operation id returns its first result, and files that change during the restore fail it with `concurrent_edit` after recording them.

## Proposals

A workspace has one fixed base tree and a stable change id. Each proposal is a manifest naming its `base_tree`, its `tree`, the files that differ between them, the `op_head` (the hash of the operation log entry that recorded the tree) and a symbol map for each analyzed file. The store keeps every tree a manifest names, so later evidence stays resolvable after rework; the operation log's hash chain up to `op_head` is covered by the manifest's signature, so editing it is detected (`oplog_broken`).

Capture scans the workspace twice and records the tree only when both scans agree, retrying when files change mid-capture; it does not claim an atomic filesystem snapshot. Submodule changes are rejected.

## Sessions and recovery

Session private keys exist only in daemon memory. When the daemon restarts, sessions whose keys were lost are ended with root-signed events; if the root key is unavailable, `system status` reports `recovery_pending` and those sessions cannot sign. Work continues only through `session recover`. Closed and selected workspaces cannot be rebound.

Close publishes the terminal state before cleanup. It first records the workspace's current tree in its operation log, so edits after the last proposal are kept in the store, and then removes the checkout. Ignored files, which no snapshot records, are preserved: they make the call fail with `cleanup_pending`, `committed: true`, the operation id and the receipt, and `ledger recover` reports the outstanding cleanup until the files are dealt with and recovery is retried. The operation log and every tree it names survive closure.

## Protocol

Clients POST one request to `/v1` over `.ledger/runtime/ledgerd.sock`:

```json
{"api_version":2,"operation":"workspace.status","operation_id":"","session_id":"","input":{"workspace_id":"wsp_..."}}
```

The daemon answers `api_version` 2, whose records bind store trees; a request of another version fails with `unsupported_api_version`. Requests are limited to 1 MiB. Input and output schemas for every operation are in [`schemas/v2/api/`](../schemas/v2/api/); [`schemas/v1/api/`](../schemas/v1/api/) describes the API of Phase 1 daemons. The socket is mode 0600 inside a 0700 directory, so only the repository owner's processes can connect; there is no network listener and no model SDK dependency.

## Current limits

Enforcement is cooperative: direct disk writes are observed, not prevented, and a session signature records that Ledger acted under that session's authority, not that a particular agent process held the key. Evidence is not isolated (see [security-model.md](security-model.md)).

## Criteria, judgment and selection
After evidence runs, `ledger intent criteriaStatus '{"manifest":"…"}'` evaluates every criterion against the evidence for that exact manifest and tree. A check criterion is satisfied only by an independent attestation of its pinned definition; a judgment criterion only by an independent judgment of the stated reviewer kind (`ledger evidence judge`, signed by a root-provisioned `human` session when root calls it). All-pass is `satisfied`, any fail is `failed` (mixed results list the `conflicting` attestations), and nothing applicable is `outstanding`. Failing evidence is never hidden: it is listed under `contrary_evidence` even when an explicit `evidence` set leaves it out. Evidence for another revision is rejected with `cross_revision_evidence`.
`ledger selection record '{"manifest":"…","evidence":["…"]}'` signs a decision recording the evidence set, recomputed per-criterion statuses, outstanding criteria and contrary evidence. The input has no status members, so a caller cannot claim a criterion is satisfied, and the index re-derives every decision when it is published.
Selection is not acceptance. A decision is one signed record: it advances no line, merges nothing, and leaves the intent and workspace states unchanged (only a speculation run moves a workspace to `selected`). Acceptance is a separate signed `line_advance` record (ADR 0027).

## Acceptance

`ledger line advance --session SESSION '{"selection":"…","line":"main"}'` accepts the manifest a selection decision chose onto a line. The caller is root or a session whose capability lists the line in `may_advance`, which only root grants, at `session create` or `session recover`; the manifest's author cannot accept its own change. The advance is refused with `criteria_unsatisfied` unless every criterion is satisfied by the selection's evidence when evaluated again, and also when contrary evidence has been recorded since the selection; contrary evidence the decision already lists was weighed by the selector and does not block. The intent must be open and the selected manifest must still be its workspace's current proposal (`stale_revision`).

A line moves only by fast-forward. Inside the RecordStore lock an advance must name the line's current head as `previous` (`stale_line`) and its manifest must be based on that head's tree (`stale_base`), so a manifest forked before another change was accepted must be reworked on the new head. Nothing is merged. After an advance, `workspace fork` without `base_tree` starts from the accepted tree and records the advance as the workspace's `base_line_advance`. `ledger line status '{"line":"main"}'` shows where a line stands. An advance changes no intent or workspace state: close them as usual.

## Scoped capabilities and warnings

Session creation and recovery now accept path scopes. Capability issuance, delegation, polling and acknowledgment are available through the CLI and versioned socket API. See [security-model.md](security-model.md) for inputs, matching rules, dependency filenames, recovery semantics and limitations.

## Symbol coordination and leases

JavaScript, TypeScript/TSX and Python analysis uses the official tree-sitter grammars. A symbol locator combines path, language, lexical qualified name, declaration kind and an optional disambiguator. Body/signature edits preserve the locator; renames and moves are deletion/addition. Proposals analyze the exact frozen base and candidate file contents, store a symbol map for each analyzed file, and record written symbols and best-effort static read references. Optional `context_refs` entries of the form `path#lexical.name` also contribute declared reads. This describes referenced code, not what an agent actually read.

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

`start` returns each attempt's workspace path, session, capability, strategy and spec. `n` (default: one attempt per strategy) assigns strategies round-robin. Give exactly one of `deadline` and `deadline_seconds`. Each attempt's agent edits its workspace and submits once, from its own session, before the deadline; the submitted manifest is the frozen candidate and the workspace cannot be reworked, closed or selected by hand until the run ends.

`evaluate` closes collection, runs every pinned check on every candidate in parallel, each in a fresh checkout, and scores the candidates that satisfy every check criterion. A `command` scorer reads the candidate document from `$LEDGER_SCORER_INPUT` and writes `{"scores":{"<manifest>":<integer>,…}}` to `$LEDGER_SCORER_OUTPUT`; `$LEDGER_CANDIDATES/<manifest>/` holds a checkout of each candidate's tree. It also gets `LEDGER_REPOSITORY`, `LEDGER_RUN_ID` and its allowlisted variables, and nothing else. With a `submission` scorer, `evaluate` returns `awaiting_scores: true` and the candidates, and the run waits for `ledger speculation score '{"run_id":"…","scores":{…}}'`. The highest score wins; ties go to the lowest manifest hash. Evaluation keeps running if the client disconnects.

The run ends in one signed group: the winner's selection and `selected` transition, and for every other attempt an attempt record (`failed_criteria`, `lost_tiebreaker`, `timeout`, `not_proposed` or `cancelled`) and a `closed` transition. If no candidate is eligible, the run resolves with no winner. `ledger speculation cancel '{"run_id":"…","reason":"…"}'` ends a run with no winner, stopping an evaluation in progress. A scorer failure, timeout or malformed scores interrupt the run with no winner; `ledger speculation resume '{"run_id":"…"}'` returns it to evaluation with the same frozen inputs. A daemon restart interrupts a run that was evaluating; a collecting run keeps collecting, and `session.recover` replaces a lost attempt session.

Losing workspaces are closed after the run ends: their last trees are recorded in their operation logs and their checkouts removed, unless they hold ignored files, in which case they are kept and the call reports `cleanup_pending` with `committed: true`. Nothing is merged. See [ADR 0023](adr/0023-speculation-runs.md).

## Git bridge

Git is a place Ledger reads history from and writes accepted changes to; it is not where Ledger keeps code (ADR 0028). Every Git object is read and written in a bare staging repository under `.ledger/runtime/bridge/`, with system and global Git configuration disabled, hooks pointed at `/dev/null`, transferred objects checked and only local repositories readable, so nothing a source repository configures (hooks, filters, `.gitattributes`, aliases) runs during import or export.

`ledger init --from-git PATH [--ref REF]` creates a native repository whose first tree is that of `REF` (default `HEAD`) in the Git repository at `PATH`, which may be the directory itself, and signs one root `bridge_mapping` per commit of its first-parent history: the native tree, the Git commit, tree and parents, and its author, committer and message. Uncommitted files are not imported. Imported history is labelled imported: it carries no intent, capability or evidence and satisfies no criterion. A path that is not NFC UTF-8, two paths that collide under the case policy, and Git modes other than regular, executable, symlink, tree and submodule are refused (`unsupported_path`, `path_collision`, `unsupported_mode`); `100664` files import as regular files. Submodules are kept as opaque `gitlink` entries. `ledger git import [--from PATH] REF` imports another ref the same way. Both are safe to repeat: a commit is mapped once.

`ledger git export [--line main] --to PATH [--ref REF]` writes one Git commit per `line_advance` of the line on top of the imported commit of the repository's first tree (or, in a repository not imported from Git, a root commit of that tree), and fast-forwards `REF` (default `refs/heads/LINE`) in the Git repository at `PATH`. A commit is authored by the manifest's author session and committed by the advance's actor, both at the advance's time, with `@ledger.invalid` addresses; its message is the proposal's plan, or the intent's description, followed by `Ledger-Line-Advance`, `Ledger-Manifest`, `Ledger-Intent` and `Ledger-Selection` trailers. The same records always produce the same commit hashes, and Ledger signs an export `bridge_mapping` for each new commit. A ref that has moved is reported as `ref_moved`, and the branch checked out in a working tree as `ref_checked_out` (export to another `--ref`); neither is changed.

`ledger git mirror --remote NAME [--line main] [--repo PATH]` exports the line into `refs/ledger/lines/LINE` of the bridged repository (or `--repo`) and runs an ordinary `git push` of it to `refs/heads/LINE` on that repository's remote. The push uses your Git configuration, so credential helpers work, with hooks disabled. It never force-pushes: a remote branch that moved is reported as `ref_moved` and left alone.

`--proposals` on `ledger git export` or `ledger git mirror` also writes each open proposal as a branch for review on a Git host:

- Each workspace that is proposed or selected becomes a branch `refs/heads/ledger/<intent>/<workspace>`. Its one commit has the latest manifest's tree, on top of the line commit for the manifest's base tree.
- The commit is authored and committed by the author session, at the time of its proposal. Its message is the plan, or the intent's description, followed by `Ledger-Manifest`, `Ledger-Intent` and `Ledger-Workspace` trailers. The same records always give the same commit, and root signs an export `bridge_mapping` naming the manifest.
- A re-proposal replaces the branch only while the branch still holds a commit Ledger exported for that workspace. Otherwise the branch is listed under `refused` with `ref_moved` and left alone. Mirroring pushes with `--force-with-lease` against the commit it last mirrored, so a re-proposal updates the pull request but never overwrites someone else's push.
- A proposal from Phase 1, or one whose base is no longer on the line, is listed under `skipped`.

A reviewer with a bundle from `ledger export` and the branch's head commit runs `ledger verify --bundle FILE --git-commit SHA`. The same works for a commit of an exported line, which resolves through its advance to the accepted manifest. The report's `git_commit` part names the manifest and checks three things:

- `commit_id`: the export mapping's fields hash to the commit id.
- `trailer`: the message names the manifest.
- `tree`: the Git tree id computed from the manifest's native tree in the bundle is the commit's tree.

None of these rests on who signed the mapping, and the manifest's evidence is in the same report. With `--strict`, the command fails unless all three hold and the manifest verifies.

`ledger git sync [--line main] [--from PATH] [--ref REF]` brings the line up to commits made in Git. It exports the line, fetches `REF` (default `refs/heads/LINE`) from the bridged repository (or `--from`), and compares the two:

- If `REF` continues the exported line along first parents, sync imports each new commit and accepts it with a root-signed `foreign_change` and a `line_advance` that names it. The change has participant `bridge`, carries no intent, capability or evidence, and satisfies no criterion. Running sync is the root's acceptance of what was committed in Git.
- Exporting afterwards reuses those Git commits rather than writing new ones, so a mirror after a sync pushes nothing new.
- Status `up_to_date` means the two agree, and `behind` means Git has not caught up with the line yet; `ledger git mirror` pushes it.
- If both have moved, sync fails with `diverged`, names both tips and changes nothing. Ledger never merges them.

To take commits from a remote, run `git fetch` in the bridged repository first and pass the remote-tracking ref, for example `--ref refs/remotes/origin/main`.

## Migrating a Phase 1 repository

`ledger migrate`, run in a Phase 1 repository with its root key, converts it in place (ADR 0027). It writes v2 records beside the v1 ones and rewrites or re-signs none of them:

- It refuses with `verification_failed` if the v1 records do not verify, and with `root_unavailable` or `root_mismatch` without the repository's root key.
- Through the Git bridge it imports HEAD's first-parent history and every other commit a v1 record names, such as proposal and base commits, and signs an import `bridge_mapping` for each. A commit that no branch or `refs/ledger/proposals/` ref reaches fails with `commit_unavailable`.
- It signs a `closed` event, reason `migrated`, for every Phase 1 workspace still open or proposed. A Phase 1 workspace is a Git worktree with no operation log, so it cannot continue. Its worktree is left on disk under `.ledger/runtime/workspaces/` for you to carry uncommitted files into a new workspace. Its proposals, evidence and selections stay in the history, but a v1 selection cannot advance a line.
- It signs the `repository` record, whose tree is HEAD's, so HEAD's tree becomes the head of `main`. Uncommitted files in the working tree are not imported.
- It writes `.ledger/bridge.json` naming the repository itself, so `ledger git export` and `ledger git mirror` work at once.

The content store is converted under `.ledger/runtime/migrate/` and moved into place last. Until then the repository is still a Phase 1 one, so an interrupted migrate is finished by running it again. Afterwards `ledger verify` checks each v1 manifest's commit binding (trailer, tree and parent) against the commit rebuilt from its import mapping, accepting a mapping only when its fields hash to the commit id the v1 records name. The check is the same as Phase 1's, without the Git objects.

## Export, import and verify

For MCP stdio setup, the complete tool inventory, checked text editing and
portable history/live coordination queries, see the [agent integration
contract](contracts/agents.md). `ledger history why|attempts|dependents` and
`ledger coordination nearby` expose the same query inputs through the CLI.

`ledger export --out FILE [--manifest HASH]... [--intent ID]... [--allow-partial]` writes a version 2 bundle (an uncompressed tar: `bundle.json` inventory, `records/<aa>/<hash>.json`, `store/<aa>/<hash>` for every store object the exported manifests' trees, base trees and symbol maps reach, and `oplog/<workspace>.log` for each workspace's operation log up to the last head an exported manifest pinned; later log entries stay private). The repository record always travels with the bundle. The default exports every committed group; `--manifest` and `--intent` export a closure (author and verifier sessions with their capability chains, root and session public keys, workspace, bindings and lifecycle events, intent, check definitions, receipts, evidence and selections). A missing dependency fails with `export_incomplete`; `--allow-partial` writes a bundle whose inventory says `complete: false`.

`--with-git` adds a Git part, `git/objects.bundle`, in a repository that has imported or exported Git history. It holds every commit the bundle's bridge mappings name, with their history, under `refs/ledger/commits/<sha>`. A mapped commit that is no longer in the bridge's staging repository makes the export incomplete (`git:<sha>`).

When the Git part is present, `ledger verify --bundle` unpacks it into an inert repository and checks every mapping against it. The commit must exist with the tree, parents and message its mapping records, and must hash to its id. Its tree must also be the Git tree id computed from the mapping's native tree, wherever the bundle carries that tree. A mismatch is reported as `mapping_mismatch` and a missing commit as `git:<sha>`, and the report's `git` part counts the mappings, commits and trees checked. A bundle without `--with-git` checks none of this, and its v1 manifests and proposal commits are still checked through their mappings.

Private keys, trust lists, journals, runtime state, SQLite and uncommitted records are never exported.

`ledger verify [--bundle FILE] [--trust FINGERPRINT]... [--trust-file PATH] [--strict]` reports integrity (`verified`, `invalid`, `unavailable`) separately from trust (`trusted`, `untrusted`, `unknown`). Without `--bundle` it verifies this repository's record files, store and operation logs. A v2 manifest is checked by `tree` (its closure is intact), `files` (exactly the change from its base), `oplog` (the log chains unbroken up to `op_head`, a `propose` entry of that tree) and `symbols` (each map describes its file); a missing object or log is `unavailable`, an edited one `invalid`. Version 1 bundles and Phase 1 repositories keep verifying through the Phase 1 path. Trust comes only from the local list (default `$XDG_CONFIG_HOME/ledger/trusted-roots.json`, else `~/.config/ledger/trusted-roots.json`, format `{"version":1,"roots":[{"fingerprint":"<hex>","label":"..."}]}`) and `--trust`; keys inside a bundle never add trust. `export`, `verify` and `reindex` need neither the daemon nor SQLite. `--strict` returns `verification_failed` unless integrity is `verified`.

`ledger import FILE` stages the bundle in an inert private directory, verifies it, and adds it to this repository: store objects first, then operation logs, then the records through the RecordStore, so no record is published before what it names. A bundle from another repository is refused with `repository_mismatch` before anything is written. An operation log is only ever created; a local log that does not already begin with the bundled entries is reported `oplog_conflict` and kept. Importing again changes nothing; divergent histories are kept and reported as conflicts. Nothing is trusted and no key, config or line is imported.

Record files copied into `.ledger/objects` by any other means are accepted at the next `ledger recover` if their groups are complete and validly signed, so apply the same trust judgement as for a bundle.

`export` reports its size honestly (`bundle_bytes`, `record_bytes`, `store_bytes`, record and store object counts). The store part carries every file of each exported tree and base tree, so a bundle of a large repository is large. A file ending in `.partial` is an interrupted export and is never verified or imported. `demos/m3.sh` verifies a bundle in a fresh directory with the network disabled and the source repository unreadable, imports it into an earlier copy of the repository, and shows an unrelated repository refusing it.

A Phase 1 repository exports a version 1 bundle, whose Git part carries the proposal commits. Its revisions live at `refs/ledger/proposals/<manifest-hash>` and its records at `refs/ledger/records`, which ordinary `git fetch` and `git push` do not move. To share them between clones for verification, use explicit refspecs:

```sh
git fetch origin 'refs/ledger/proposals/*:refs/ledger/proposals/*' 'refs/ledger/records:refs/ledger/records'
git push origin 'refs/ledger/proposals/*:refs/ledger/proposals/*' 'refs/ledger/records:refs/ledger/records'
```
