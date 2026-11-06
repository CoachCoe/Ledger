# Phase 1 security model

Ledger's local enforcement is cooperative. Direct filesystem writes already
happen before Ledger observes them. Out-of-scope writes produce runtime
notifications; `workspace.propose` rejects unauthorized contents before signing
and publishing a proposal. An agent can bypass Ledger entirely and use Git or
edit its local configuration. This is not a filesystem sandbox.

## Scope and delegation

`session.create` and root-authorized `session.recover` accept `allow`, `deny`,
`may_add_dependencies`, and `expires_at`. For compatibility, omitted fields mean
`["**"]`, `[]`, dependency changes allowed, and a 24-hour expiry. Request explicit
narrow authority for an agent:

```sh
ledger session create '{"allow":["src/billing/**"],"deny":["src/billing/private/**"],"may_add_dependencies":false}'
```

`capability.issue` requires root authorization. `capability.delegate --session ID`
requires the parent holder. Both take an existing `session_id`, `allow`, `deny`,
`may_add_dependencies`, and `expires_at`; delegation also takes
`parent_capability_id`. Use the returned capability id when forking a workspace.
Session key creation remains root authorized. Delegation never widens parent
allow patterns, expiry, or dependency authority. Parent denies always apply.
Every capability in the chain must remain unexpired at proposal time.

Patterns are exact repository-relative POSIX paths, directory prefixes `dir/**`,
or `**`. Other wildcards, absolute paths, empty segments, `.` and `..` segments,
backslashes, colons and NUL are rejected. Matching normalizes Unicode NFC and uses
the case policy detected by `init`. A prefix matches descendants, not the directory
entry itself. Link paths and fully resolved targets must both be in scope. Symlink
cycles and targets outside the workspace fail closed; nonexistent in-workspace
targets still require scope. Deletions also check their base-tree symlink targets.
The immutable proposed tree is authorized, including mode changes, both sides of
renames, deleted paths and changes committed directly by an agent. Observation
caches never authorize a proposal.

When dependency changes are forbidden, Ledger conservatively denies changes of
any kind to recognized manifest/lockfile names at any directory depth:

- JS/TS: `package.json`, `package-lock.json`, `npm-shrinkwrap.json`, `yarn.lock`,
  `pnpm-lock.yaml`, `bun.lock`, `bun.lockb`, `deno.json`, `deno.jsonc`, `deno.lock`.
- Python: `pyproject.toml`, `Pipfile`, `Pipfile.lock`, `poetry.lock`, `uv.lock`,
  `pdm.lock`, `setup.py`, `setup.cfg`, and `requirements*.txt` / `constraints*.txt`.

This filename rule does not detect every runtime dependency, imports, executable
install commands, vendored code, custom manifest names or unsupported ecosystems.
No optional `workspace.write` API is provided in this phase.

## Keys, recovery and historical verification

The human root is a locally configured Ed25519 key outside the repository.
Session keys are generated freshly, held only in the daemon's memory, and discarded
on session end or daemon exit. A session signature means “Ledger recorded this
under this session and capability”; it does not prove which agent process wrote
the files or held a private key. Local timestamps are signed claims, not trusted
timestamps. Root trust is configured separately from cryptographic integrity.

After key loss, explicit root recovery creates a new session and root capability
and appends a workspace binding. It does not revive an old key or expired authority.
Earlier manifests keep their original author. Open/proposed workspaces can recover;
terminal workspaces cannot. To prevent concurrent recoveries from replacing each
other's choice, pass the observed `previous_binding` hash. New proposals must
revalidate the complete base-relative snapshot under the replacement capability.

The `internal/verify` M2 interface verifies manifest signatures, authenticated
sessions, delegation, historical workspace bindings, and claimed file scope from
immutable records without an index. It reports `integrity` and `trust` separately.
It does not yet verify exported bundles or Git tree/object completeness; M3 owns
those checks. A trusted key cannot make a tampered record valid.

A hosted service would need sandboxed execution, filesystem and network controls,
secret isolation, per-agent key custody and authenticated API callers to enforce
an agent boundary. Phase 1 does not provide isolated evidence execution.

## Notifications

Use `coordination notifications --session ID '{"session_id":"ID"}'` to poll and
`coordination ack --session ID '{"session_id":"ID","notification_id":"ntf_…"}'`
to acknowledge. Root may inspect and acknowledge predecessor queues during
recovery. Notifications retain original recipient attribution. Poll results
include acknowledged entries; `since` is an inclusive creation timestamp; deduplicate repeated ids.
Warnings are deduplicated per binding/path/reason even after acknowledgment.

Queues and acknowledgments survive ordinary restart. Explicit reindex reports
`discarded_runtime.notifications`; the next daemon observation regenerates current
warnings. Transient violations already reverted and delivery history cannot be
reconstructed. These runtime documents are never exported as provenance.
