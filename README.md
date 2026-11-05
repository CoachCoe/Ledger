# Ledger

Ledger is a local control plane for agent-driven development over Git. Git stores code; Ledger stores signed, content-addressed records of intent, authority, proposals, evidence, and lifecycle history alongside it, in `.ledger/` and on the `refs/ledger/records` ref.

## Build

Requires Go 1.27.3 or later and Git 2.25 or later. macOS builds require cgo and Apple’s command-line C toolchain for FSEvents.

```sh
make build   # bin/ledger
make test    # go test -race ./...
make lint    # go vet + golangci-lint
```

## Commands

Every command prints one JSON document: `{"ok":true,"result":...}`, or `{"ok":false,"error":{"code":...,"message":...}}` with exit status 1.

| Command | Effect |
|---|---|
| `ledger init` | Create `.ledger/` in the current repository, exclude it from Git, create the root key in `~/.config/ledger/keys/` if needed, and record its public key. Safe to repeat. |
| `ledger status` | List lifecycle subjects (intents, sessions, workspaces, runs) with their derived state and any conflict. |
| `ledger recover` | Finish interrupted operations, quarantine corrupt record files, reconcile `.ledger/objects` with `refs/ledger/records`, and index complete groups. |
| `ledger reindex` | Rebuild derived tables from the record files in one database transaction. |
| `ledger version` | Print the version. |

For authenticated sessions, intents, workspaces, proposal/rework and daemon recovery, see the [M1 workflow/API guide](docs/m1-api.md). The CLI auto-starts the per-repository daemon.

## Storage

- `.ledger/objects/`: immutable signed records in canonical JSON, named by SHA-256. These and their copy on `refs/ledger/records` are the source of truth.
- `.ledger/index.db`: a SQLite index of committed records plus local runtime state. It can always be rebuilt with `ledger reindex`.
- `.ledger/journal/`, `.ledger/quarantine/`, `.ledger/runtime/`: in-flight operations, damaged files kept for inspection, and local caches.

Share records with `git push origin refs/ledger/records` and fetch them with `git fetch origin refs/ledger/records:refs/ledger/records`; `ledger recover` then imports what arrived.

## Demos

`demos/m0.sh` exercises the record store end to end in a temporary repository: idempotent init, stable hashing, multi-process writers, crash recovery, a concurrently moved ref, quarantine, racing transitions, imported conflicts, and reindex.

`demos/m1.sh` drives the public CLI through signed sessions, workspace observation, three fixed-base revisions, retained refs/GC, restart/rebinding and safe close. `tools/benchmark-observer.py` measures the production daemon on ten workspaces of a real project with dependencies installed.

## Design

Architecture decisions are in [`docs/adr/`](docs/adr/). The record, recovery, and verification contracts are summarized in [`docs/contracts/`](docs/contracts/).
