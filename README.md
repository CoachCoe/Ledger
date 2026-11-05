# Ledger

Ledger is a local control plane for agent-driven development over Git. Git stores code; Ledger stores signed, content-addressed records of intent, authority, proposals, evidence, and lifecycle history alongside it, in `.ledger/` and on the `refs/ledger/records` ref. Agents work in isolated workspaces bound to an intent, and every proposal lands as an ordinary Git branch whose commit trailers link it to an immutable manifest. Ledger never merges.

## Build

Requires Go 1.27.3 or later and Git 2.25 or later. macOS builds need cgo and Apple's command-line C toolchain for FSEvents.

```sh
make build     # bin/ledger
make test      # go test -race ./...
make lint      # go vet + golangci-lint (v2.15 or later)
make demo      # the end-to-end demos
make security  # govulncheck
make check     # everything CI runs
```

## Commands

Every command prints one JSON document: `{"ok":true,"result":...}`, or `{"ok":false,"error":{"code":...,"message":...}}` with exit status 1.

| Command | Effect |
|---|---|
| `ledger init` | Create `.ledger/` in the current repository, exclude it from Git, create the root key in `~/.config/ledger/keys/` if needed, and record its public key. Safe to repeat. |
| `ledger session …` | Create, end, or recover a signing session. |
| `ledger intent …` | Create, inspect, or close an intent. |
| `ledger workspace …` | Fork, inspect, propose, rework, or close a workspace. |
| `ledger system status` | Report the daemon's view of every subject and any pending session recovery. |
| `ledger daemon` | Run the per-repository daemon in the foreground; `ledger daemon stop` stops it. |
| `ledger status` | List lifecycle subjects with their derived state and any conflict, read directly from the index. |
| `ledger recover` | Finish interrupted operations, quarantine corrupt record files, reconcile `.ledger/objects` with `refs/ledger/records`, and index complete groups. |
| `ledger reindex` | Rebuild the index from the record files in one database transaction. |
| `ledger version` | Print the version. |

The session, intent, workspace, system, and daemon-stop commands go through the daemon, which the CLI starts on demand. [`docs/workflow.md`](docs/workflow.md) walks through a complete session and describes every operation, observation, proposals, recovery, and the socket protocol.

## Storage

- `.ledger/objects/`: immutable signed records in canonical JSON, named by SHA-256. These and their copy on `refs/ledger/records` are the source of truth.
- `.ledger/index.db`: a SQLite index of committed records plus local runtime state. It can always be rebuilt with `ledger reindex`.
- `.ledger/journal/`, `.ledger/quarantine/`, `.ledger/runtime/`: in-flight operations, damaged files kept for inspection, and the daemon's socket, log, workspaces, and caches.

Share records with `git push origin refs/ledger/records` and fetch them with `git fetch origin refs/ledger/records:refs/ledger/records`; `ledger recover` then imports what arrived. Proposal revisions are kept at `refs/ledger/proposals/*` and shared the same way.

## Demos

`demos/m0.sh` exercises the record store in a temporary repository: idempotent init, stable hashing, multi-process writers, crash recovery, a concurrently moved ref, quarantine, racing transitions, imported conflicts, and reindex.

`demos/m1.sh` drives the public CLI through signed sessions, workspace observation, three fixed-base revisions, retained refs and garbage collection, restart and rebinding, and safe close.

`tools/observerbench` measures the daemon's observation latency on ten workspaces of a real project; see [`docs/benchmarks/`](docs/benchmarks/).

## Design

Architecture decisions are in [`docs/adr/`](docs/adr/). The record, recovery, and verification contracts are summarized in [`docs/contracts/`](docs/contracts/).
