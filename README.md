# Ledger

Ledger is a local control plane for agent-driven development over Git. Git stores code; Ledger stores signed, content-addressed records of intent, authority, proposals, evidence, and lifecycle history alongside it, in `.ledger/` and on the `refs/ledger/records` ref. Agents work in isolated workspaces bound to an intent, and every proposal lands as an ordinary Git branch whose commit trailers link it to an immutable manifest. Ledger never merges.

Ledger runs on macOS and Linux, one repository per daemon, on one machine. It does not bundle or call any model or coding agent: agents reach it through the CLI or the MCP server, and enforcement is cooperative (see [`docs/security-model.md`](docs/security-model.md)).

## Build

Requires Go 1.27.3 or later, Git 2.25 or later, cgo and a C compiler for tree-sitter. macOS also uses Apple's command-line C toolchain for FSEvents.

```sh
make build     # bin/ledger
make test      # go build, then go test -race -p 1 ./...
make lint      # go vet + golangci-lint v2.15
make demo      # the end-to-end demos
make security  # govulncheck
make check     # lint, test, demo and security: everything CI runs
```

The demos also need `jq` and `python3`.

## Quick start

Put `bin/ledger` on your `PATH`, then, in any Git repository with at least one commit:

```sh
ledger init
ledger session create '{"agent_name":"my-agent"}'
ledger intent create '{"description":"Implement refunds","criteria":[{"id":"review","statement":"Review the implementation","verification":{"kind":"judgment","reviewer":"human"}}]}'
ledger workspace fork --session ses_... '{"intent_id":"int_...","capability_id":"cap_..."}'
# edit files in the returned path with any tool
ledger workspace status '{"workspace_id":"wsp_..."}'
ledger workspace propose --session ses_... '{"workspace_id":"wsp_...","plan":"Add refunds"}'
ledger daemon stop
ledger verify
```

Replace each `..._...` with the id an earlier command returned. The proposal is an ordinary branch, `ledger/<intent>/<workspace>`, and `ledger verify` reports the integrity of every record without the daemon. [`docs/workflow.md`](docs/workflow.md) walks through the same session and documents every operation; [`docs/contracts/agents.md`](docs/contracts/agents.md) shows how to connect an MCP client.

## Commands

Commands print one JSON document: `{"ok":true,"result":...}`, or `{"ok":false,"error":{"code":...,"message":...}}` with exit status 1. `ledger mcp` instead serves the MCP stdio protocol.

| Command | Effect |
|---|---|
| `ledger init` | Create `.ledger/` in the current repository, exclude it from Git, create the root key in `~/.config/ledger/keys/` if needed, and record its public key. Safe to repeat. |
| `ledger session …` | Create, end, or recover a signing session. |
| `ledger capability …` | Issue root authority or delegate narrower authority to an authenticated session. |
| `ledger intent …` | Create, inspect, or close an intent; `intent criteriaStatus` reports each criterion as satisfied, failed, or outstanding for an exact revision, with contrary evidence. |
| `ledger workspace …` | Fork, inspect, write regular text files, propose, rework, or close a workspace. |
| `ledger coordination …` | Inspect advisory symbols, poll and acknowledge notifications, and create, list, or release advisory leases. |
| `ledger evidence …` | Resolve a proposal's frozen binding, run a pinned check in a fresh verification worktree, record a judgment, or ask whether evidence is independent. |
| `ledger selection record` | Sign a selection decision with recomputed criterion statuses. Selection is not acceptance and never merges. |
| `ledger speculation …` | Start a run of several attempts for one intent, submit frozen candidates, evaluate them against the intent's checks and a scorer, and inspect, score, resume, or cancel the run. The selected candidate is not merged. |
| `ledger history …` | Query portable symbol history, attempts, and selected dependents. |
| `ledger mcp` | Serve every versioned core operation as an MCP tool over stdio. |
| `ledger system status` | Report the daemon's view of every subject and any pending session recovery. |
| `ledger daemon` | Run the per-repository daemon in the foreground; `ledger daemon stop` stops it. |
| `ledger export --out FILE` | Write a portable bundle of records and the Git objects they bind, without the daemon or SQLite. |
| `ledger verify [--bundle FILE]` | Report integrity and trust separately for a bundle or this repository's records; `--trust`, `--trust-file`, `--strict`. |
| `ledger import FILE` | Stage, verify, and import a bundle. Trusts nothing and touches no code branch. |
| `ledger status` | List lifecycle subjects with their derived state and any conflict, read directly from the index. |
| `ledger recover` | Finish interrupted operations, quarantine corrupt record files, reconcile `.ledger/objects` with `refs/ledger/records`, and index complete groups. |
| `ledger reindex` | Rebuild the index from the record files in one database transaction. |
| `ledger version` | Print the version. |

The session, capability, intent, workspace, coordination, evidence, selection, speculation, history, system, and daemon-stop commands go through the daemon, which the CLI starts on demand. `export`, `verify`, `import`, `status`, `recover`, and `reindex` work directly on the record files.

## Storage

- `.ledger/objects/`: immutable signed records in canonical JSON, named by SHA-256. These and their copy on `refs/ledger/records` are the source of truth.
- `.ledger/index.db`: a SQLite index of committed records plus local runtime state. It can always be rebuilt with `ledger reindex`.
- `.ledger/journal/`, `.ledger/quarantine/`, `.ledger/runtime/`: in-flight operations, damaged files kept for inspection, and the daemon's socket, log, workspaces, and caches.

Records, together with the Git commits they bind, are portable: `ledger export` writes a single bundle that `ledger verify` checks in any directory, offline and without the source repository, and `ledger import` brings into another repository. Integrity is computed from the bundle; trust comes only from your local trusted-roots list. Share records with `git push origin refs/ledger/records` and fetch them with `git fetch origin refs/ledger/records:refs/ledger/records`; `ledger recover` then imports what arrived. Proposal revisions are kept at `refs/ledger/proposals/*` and shared the same way.

## Demos

Each demo runs in a disposable repository and is part of `make demo`.

- `demos/m0.sh`: the record store: idempotent init, stable hashing, multi-process writers, crash recovery, a concurrently moved ref, quarantine, racing transitions, imported conflicts, and reindex.
- `demos/m1.sh`: signed sessions, workspace observation, three fixed-base revisions, retained refs and garbage collection, restart and rebinding, and safe close through the public CLI.
- `demos/m2.sh`: scoped authority, delegation, unsafe symlinks, dependency-file denial, historical signature chains, queue recovery, and the public CLI.
- `demos/m3.sh`: every M3 acceptance and recovery test, then an exported bundle verified in a fresh directory with the network disabled and the source unreadable (integrity verified, trust untrusted, then trusted with `--trust`), tamper detection, and a repeated import into a fresh repository.
- `demos/m4.sh`: JS/TS/TSX/Python symbol analysis, symmetric watcher notifications within five seconds, queue and lease recovery, and frozen symbol provenance with advisory leases. Symbol analysis is incomplete; missing dependency edges do not establish safety.
- `demos/m5.sh`: speculation through the real binary: three scripted attempts where one fails the check, one loses on score and one is selected; an all-fail run; cancellation; a deadline timeout with a late edit preserved; a scorer outage that interrupts and resumes; and offline verification of every run.
- `demos/m6.sh`: the MCP contracts, history and reindex, and editing retries, then `demos/phase1.sh`: all nine Phase 1 engineering assertions on a polyglot fixture through real stdio and daemon processes.
- `demos/governance.sh`: the M0–M3 story end to end: a root-scoped session, a denied out-of-scope proposal, independent evidence for the exact revision including a failing run and its contrary evidence, a human judgment, a selection that is not acceptance, and offline verification and import.

`tools/observerbench` measures the daemon's observation latency on ten workspaces of a real project; see [`docs/benchmarks/`](docs/benchmarks/).

## Design

Architecture decisions are in [`docs/adr/`](docs/adr/). The record, recovery, verification, and agent contracts are in [`docs/contracts/`](docs/contracts/), and the enforcement boundary is in [`docs/security-model.md`](docs/security-model.md).
