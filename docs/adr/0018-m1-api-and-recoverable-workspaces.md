# ADR 0018: M1 API, observation and recoverable workspaces

- Status: Accepted
- Date: 2026-11-05

## Context

M1 turns signed storage into a live local workflow. Transport, ephemeral key custody, observation and Git publication must share the same retry/recovery contract.

## Decision

- The core accepts a closed `api_version: 1` request containing operation, operation id, acting session id (empty means root), and input. Each operation has input and output schemas. The request's canonical digest freezes caller intent; resolution of defaults/checks/revisions happens only on the first execution. Responses are versioned success/error envelopes. Mutations return the receipt and exact frozen result.
- One daemon holds session keys only in memory and serves Unix HTTP at `.ledger/runtime/ledgerd.sock` (0600, inside a 0700 runtime directory). CLI clients auto-start the binary and converge through the exclusive daemon lock. Default idle shutdown is ten minutes without connected clients or active workspaces. A root-signed `daemon.stop` receipt precedes graceful stop.
- Root-issued whole-repository capabilities precede workspace creation. Scoped/delegated APIs remain unavailable until M2. Startup completes prepared groups and root-ends lost-key sessions; missing root authorization leaves recovery pending. Recovery issues new session/capability records and an explicit root-signed binding, preserving historical authorship.
- RecordStore journals carry frozen typed workspace/ref effects alongside fully signed records. Worktree add/remove is serialized. Ref effects use the previous signed proposal receipt as the expected tip, reject external advances, and retain revisions before advancing proposal branches. After receipt publication, recovery never reapplies an old branch update over a later advance. Cleanup follows committed closure and remains explicitly pending when tracked, untracked or ignored user files remain that no proposal records. A worktree whose `HEAD` is still its base and whose captured content equals its latest proposed tree is removed with `--force`, because that retained commit already holds every file.
- `WorkspaceProvider` owns path discovery, capture and effects. Git metadata reconstructs moved worktree locations without SQLite. A path registered to another workspace is rejected. Private-index snapshots include agent commits and file modes, preserve the user's index, reject changed submodules, and use two independent captures with bounded retry for proposals. No atomic filesystem snapshot is claimed.
- FSEvents/inotify trigger bounded 500 ms coalescing, plus API-session refresh and a ten-second fallback poll. Root status reads return cached observations so watcher tests cannot pass merely by rescanning. Observation captures use at most ten concurrent workers. Only mutations are serialized in the daemon; reads and observation run alongside them, each workspace's private index guarded by its own lock so captures cannot collide or save an older write set over a newer one. Index write transactions take the SQLite write lock when they begin, so concurrent writers wait instead of failing on a stale snapshot. Ignored dependency-directory events are filtered without one Git subprocess per file.
- Proposal subjects use the intent summary; stable trailers are appended literally from validated ids/hashes. This supersedes ADR 0004's `interpret-trailers` construction choice: repository trailer command/alias configuration must not execute or retarget signed bindings. Git parses the resulting trailers in independent tests. Git replacement objects are disabled for Ledger commands so signed hashes bind actual stored objects.
- An exactly identical canonical manifest reuses its previously receipted commit. Rework still records a new lifecycle transition, but a new timestamped commit must not compete for the same manifest retention ref or invalidate earlier evidence bindings. Changed content, plan, context or authorship produces a distinct manifest.

## Alternatives considered

Keeping session keys on disk would contradict the identity contract. A mutable branch lookup during retry would retarget a revision. Removing dirty or ignored files during close would risk user data. Running configured trailer generators would make bindings depend on user Git configuration. Serial observation of every worktree consumes most of the latency budget.

## Validation

Core/protocol/snapshot/fault tests, real-binary CLI tests (simultaneous auto-start, ten forks, watcher/poll/idle/restart), both milestone demos, and the real-project observer benchmark cover these decisions. Platform measurements are archived under `docs/benchmarks/`. Enforcement remains cooperative; a session signature identifies Ledger's recorded authority, not an isolated agent process.
