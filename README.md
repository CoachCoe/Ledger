# Ledger

Ledger is a local control plane for agent-driven development. It keeps code in its own content-addressed store and signed records of intent, authority, proposals, evidence, and lifecycle history beside it, all in `.ledger/`; Git is not needed. Agents work in copy-on-write workspaces bound to an intent, every change to a workspace is an entry in its hash-chained operation log, and every proposal is an immutable manifest naming the exact tree and log entry it captured. Ledger never merges.

Ledger runs on macOS and Linux, one repository per daemon, on one machine. It does not bundle or call any model or coding agent: agents reach it through the CLI or the MCP server, and enforcement is cooperative (see [`docs/security-model.md`](docs/security-model.md)).

## Build

Requires Go 1.27.3 or later, cgo and a C compiler for tree-sitter. macOS also uses Apple's command-line C toolchain for FSEvents. Git is needed only to build and to read Phase 1 repositories.

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

Put `bin/ledger` on your `PATH`, then, in the directory holding your project:

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

Replace each `..._...` with the id an earlier command returned. `ledger init` imports the directory, minus anything `.gitignore` or `.ledgerignore` excludes, as the repository's first tree. The proposal is a signed manifest of the workspace's tree, and `ledger verify` reports the integrity of every record, tree and operation log without the daemon. [`docs/workflow.md`](docs/workflow.md) walks through the same session and documents every operation; [`docs/contracts/agents.md`](docs/contracts/agents.md) shows how to connect an MCP client.

## Commands

Commands print one JSON document: `{"ok":true,"result":...}`, or `{"ok":false,"error":{"code":...,"message":...}}` with exit status 1. `ledger mcp` instead serves the MCP stdio protocol.

| Command | Effect |
|---|---|
| `ledger init` | Create a native repository in the current directory: import its files into `.ledger/store/`, create the root key in `~/.config/ledger/keys/` if needed, and sign its public key and the repository record. Inside a Git working tree it also keeps `.ledger/` out of Git. Safe to repeat. |
| `ledger session …` | Create, end, or recover a signing session. |
| `ledger capability …` | Issue root authority or delegate narrower authority to an authenticated session. |
| `ledger intent …` | Create, inspect, or close an intent; `intent criteriaStatus` reports each criterion as satisfied, failed, or outstanding for an exact revision, with contrary evidence. |
| `ledger workspace …` | Fork, inspect, write regular text files, propose, rework, or close a workspace, or detach its shared dependency trees. |
| `ledger coordination …` | Inspect advisory symbols, poll and acknowledge notifications, and create, list, or release advisory leases. |
| `ledger evidence …` | Resolve a proposal's frozen binding, run a pinned check in a fresh copy-on-write checkout of its tree, record a judgment, or ask whether evidence is independent. |
| `ledger selection record` | Sign a selection decision with recomputed criterion statuses. Selection is not acceptance and never merges. |
| `ledger speculation …` | Start a run of several attempts for one intent, submit frozen candidates, evaluate them against the intent's checks and a scorer, and inspect, score, resume, or cancel the run. The selected candidate is not merged. |
| `ledger history …` | Query portable symbol history, attempts, and selected dependents. |
| `ledger mcp --session ID` | Serve every versioned core operation as an MCP tool over stdio, acting as that session. There is no root mode over MCP. |
| `ledger system status` | Report the daemon's view of every subject and any pending session recovery. |
| `ledger daemon` | Run the per-repository daemon in the foreground; `ledger daemon stop` stops it. |
| `ledger export --out FILE` | Write a portable bundle of records, the store objects they bind and the operation logs they pin, without the daemon or SQLite. |
| `ledger verify [--bundle FILE]` | Report integrity and trust separately for a bundle or this repository's records; `--trust`, `--trust-file`, `--strict`. |
| `ledger import FILE` | Stage, verify, and import a bundle from this repository. Trusts nothing and advances no line. |
| `ledger status` | List lifecycle subjects with their derived state and any conflict, read directly from the index. |
| `ledger recover` | Finish interrupted operations, quarantine corrupt record files, index complete groups, and bring the records head log up to date. |
| `ledger reindex` | Rebuild the index from the record files in one database transaction. |
| `ledger version` | Print the version. |

The session, capability, intent, workspace, coordination, evidence, selection, speculation, history, system, and daemon-stop commands go through the daemon, which the CLI starts on demand. `export`, `verify`, `import`, `status`, `recover`, and `reindex` work directly on the record files.

## Storage

- `.ledger/objects/`: immutable signed records in canonical JSON, named by SHA-256. These are the source of truth.
- `.ledger/store/`: the content store: file contents, trees and symbol maps, named by SHA-256.
- `.ledger/oplog/`: one hash-chained operation log per workspace, which outlives the workspace.
- `.ledger/records.log`: the records head log, listing every accepted group in order. It is checked by recovery, not by verification: signed receipts are the authority.
- `.ledger/index.db`: a SQLite index of committed records plus local runtime state. It can always be rebuilt with `ledger reindex`.
- `.ledger/journal/`, `.ledger/quarantine/`, `.ledger/runtime/`: in-flight operations, damaged files kept for inspection, and the daemon's socket, log, workspaces, and caches.

Records, together with the trees and logs they bind, are portable: `ledger export` writes a single bundle that `ledger verify` checks in any directory, offline and without the source repository, and `ledger import` brings into another copy of the same repository. Integrity is computed from the bundle; trust comes only from your local trusted-roots list.

A Phase 1 repository, whose records live on `refs/ledger/records` in Git, stays readable and verifiable. This binary creates only native repositories: `init` and `import` refuse a Phase 1 repository with `migrate_required`.

## Demos

Each demo runs in a disposable repository and is part of `make demo`.

- `demos/m0.sh`: the record store: idempotent init, stable hashing, multi-process writers, crash recovery before the head log append, a group arriving from another installation, quarantine, racing transitions, imported conflicts, and reindex.
- `demos/m1.sh`: signed sessions, workspace observation, three fixed-base revisions verified offline, restart and rebinding, and close recording unproposed edits through the public CLI.
- `demos/m2.sh`: scoped authority, delegation, unsafe symlinks, dependency-file denial, historical signature chains, queue recovery, and the public CLI.
- `demos/m3.sh`: every M3 acceptance and recovery test, then an exported bundle verified in a fresh directory with the network disabled and the source unreadable (integrity verified, trust untrusted, then trusted with `--trust`), tamper detection, a repeated import into an earlier copy of the repository, and an unrelated repository refusing the bundle.
- `demos/m4.sh`: JS/TS/TSX/Python symbol analysis, symmetric watcher notifications within five seconds, queue and lease recovery, and frozen symbol provenance with advisory leases. Symbol analysis is incomplete; missing dependency edges do not establish safety.
- `demos/m5.sh`: speculation through the real binary: three scripted attempts where one fails the check, one loses on score and one is selected; an all-fail run; cancellation; a deadline timeout with a late edit kept in the operation log; a scorer outage that interrupts and resumes; and offline verification of every run.
- `demos/m6.sh`: the MCP contracts, history and reindex, and editing retries, then `demos/phase1.sh`: all nine Phase 1 engineering assertions on a polyglot fixture through real stdio and daemon processes.
- `demos/governance.sh`: the M0–M3 story end to end: a root-scoped session, a denied out-of-scope proposal, independent evidence for the exact revision including a failing run and its contrary evidence, a human judgment, a selection that is not acceptance, and offline verification and import.

Every demo runs in a native repository without Git.

`tools/observerbench` measures the daemon's observation latency on ten workspaces of a real project; see [`docs/benchmarks/`](docs/benchmarks/).

## Design

Architecture decisions are in [`docs/adr/`](docs/adr/). The record, recovery, verification, and agent contracts are in [`docs/contracts/`](docs/contracts/), and the enforcement boundary is in [`docs/security-model.md`](docs/security-model.md).
