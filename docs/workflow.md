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

Capabilities are root-issued and cover the whole repository; path scopes, delegation and enforcement at proposal are not yet available. Enforcement is cooperative: direct disk writes are observed, not prevented, and a session signature records that Ledger acted under that session's authority, not that a particular agent process held the key. Evidence execution, selection, and portable bundle verification are not yet implemented.

## Scoped capabilities and warnings

Session creation and recovery now accept path scopes. Capability issuance, delegation, polling and acknowledgment are available through the CLI and versioned socket API. See [security-model.md](security-model.md) for inputs, matching rules, dependency filenames, recovery semantics and limitations.
