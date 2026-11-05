# M1 local workflow

Ledger currently provides a local daemon, authenticated sessions, intents, detached workspaces, observation, proposal revisions and close/recovery. Git remains canonical for code. Selection, evidence, bundle verification and scoped enforcement follow in later milestones.

Build with `make build` using Go 1.27.3 or newer and Git 2.25 or newer. macOS builds need cgo and Apple's command-line C toolchain for FSEvents. Linux uses inotify. Bare repositories and separate Git directories are currently rejected. The repository path must fit the OS Unix-socket path limit.

## Public CLI

The CLI starts `ledger daemon` on demand. Flags precede the optional JSON input argument. The caller may supply `--operation-id op_<26 base32 characters>` for durable retries; otherwise the CLI generates one. The same id/request returns the original receipt/result; different input is rejected.

```sh
ledger init
ledger session create '{"agent_name":"my-agent"}'
ledger intent create '{"description":"Implement refunds","criteria":[{"id":"review","statement":"Review the implementation","verification":{"kind":"judgment","reviewer":"human"}}]}'
ledger workspace fork --session ses_... '{"intent_id":"int_...","capability_id":"cap_..."}'
ledger workspace status '{"workspace_id":"wsp_..."}'
ledger workspace propose --session ses_... '{"workspace_id":"wsp_...","plan":"Add refunds"}'
ledger workspace rework --session ses_... '{"workspace_id":"wsp_..."}'
ledger workspace close --session ses_... '{"workspace_id":"wsp_...","reason":"finished"}'
ledger daemon stop
```

The ellipses stand for the full ids returned by earlier calls. Mutating CLI output is `{"ok":true,"result":{"operation_id":...,"receipt":...,"result":{...},"members":[...],"replayed":false}}`; the domain ids are in `result.result`. Read operations return their domain result directly inside `result`. Errors include stable `code`, `message` and optional `details`. Logs stay in `.ledger/runtime/ledgerd.log` or foreground stderr. `ledger system status` includes the live process id for local diagnostics.

Use `ledger daemon --idle 10m --poll 10s` to run in the foreground with explicit settings; `--no-watch` uses only API refresh and polling. Normal observation coalesces events for 500 ms. `workspace.status` returns the latest cached write set and observation time. Calls carrying a session id refresh that session's active workspaces before execution. Restart re-observes current files; edits reverted while stopped cannot be reconstructed.

## Checks and intent rules

`.ledger/checks.json` is an array of complete check definitions:

```json
[{"name":"unit","category":"mechanical","setup":null,"command":"go test ./...","working_directory":".","env_allowlist":[],"timeout_seconds":60,"output_limit_bytes":65536}]
```

A criterion can use `{"kind":"check","check":"unit"}`. Intent creation stores the definition and pins its hash. Later edits cannot retarget the criterion or a retried request. Requirements change through a superseding intent. A change attaches to one leaf intent; an intent already used by a change cannot acquire children. M1 pins checks; execution arrives in M3.

## Revisions and recovery

Each workspace has one fixed base and change id. Worktrees are detached; proposal branches are never checked out. Rework creates a replacement manifest/commit with the same sole base parent. Exactly identical manifests reuse their original receipted commit. Every revision remains reachable at `refs/ledger/proposals/<manifest-hash>` for later evidence and Git GC. The manifest names the tree, not the commit that embeds it. Snapshot capture uses private indexes and detects concurrent captures with bounded retries; it does not claim an atomic filesystem snapshot.

Session private keys exist only in the daemon's memory. Restart ends lost-key sessions with root-signed events. `ledger session recover '{"workspace_id":"wsp_..."}'` issues replacement authority and a signed binding for an open/proposed workspace. Historical authorship stays unchanged. Missing root authorization reports recovery pending. Selected/closed workspaces cannot rebind.

Close publishes terminal state before cleanup. Modified, untracked and ignored files are preserved. When cleanup cannot complete, `cleanup_pending` reports `committed: true`, operation id and receipt; `ledger recover` reports the outstanding cleanup. After preserving/removing those files explicitly, retry recovery. Branches and retained revisions survive closure. Reindex reconstructs active paths from Git metadata and uses independent snapshots to rebuild observations.

M1 exposes root-issued whole-repository capabilities only. Direct disk writes are cooperative and session signatures record Ledger's authority, not proof of an isolated agent process. M2 adds narrower paths/delegation and proposal enforcement. M3 supplies evidence and full offline authorization/trust/bundle verification.

## Core protocol

POST a versioned request to `/v1` over `.ledger/runtime/ledgerd.sock`:

```json
{"api_version":1,"operation":"workspace.status","operation_id":"","session_id":"","input":{"workspace_id":"wsp_..."}}
```

Requests are bounded to 1 MiB. Input/output schemas are embedded from [`schemas/v1/api/`](../schemas/v1/api/). The server returns `{"api_version":1,"ok":true,"result":...}` or the corresponding structured error. Root calls use an empty `session_id`; local socket/runtime permissions restrict access to the user. No model SDK or network listener is required.

Run `bash demos/m1.sh` for the public CLI flow. `go test -race ./...` also covers simultaneous auto-start, ten forks, watcher/poll/idle/restart, capability/key binding, retries, user-index preservation, modes/symlinks/deletes/renames, submodule denial, moved paths, trailer configuration and cleanup safety. Real-project latency evidence and reproduction instructions are in [`docs/benchmarks/`](benchmarks/).
