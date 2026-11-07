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
M3 extends it to evidence, selections, exported bundles and the bound Git
commits (trailer, tree and fixed-base parent are reported separately). A trusted
key cannot make a tampered record valid.

A hosted service would need sandboxed execution, filesystem and network controls,
secret isolation, per-agent key custody and authenticated API callers to enforce
an agent boundary. Phase 1 does not provide isolated evidence execution; the next
section states exactly what that means.

## Evidence is not isolated

`evidence.run` executes a check definition's setup and command with `/bin/sh -c`
as the same operating-system user that runs the Ledger daemon. In Phase 1 there is
no network, filesystem or process isolation. A check can read and write anything
that user can, reach any network that user can, read the user's other files, and
start processes. Every attestation therefore carries `isolation: "none"`, and the
record schema accepts no other value. Do not describe Phase 1 evidence as produced
in an isolated, sandboxed or hermetic environment.

What Phase 1 does provide is fidelity and bounded execution, not containment:

- A fresh detached worktree at the bound commit under `.ledger/runtime/verify/<run-id>/`,
  never the author's workspace. Tracked content is compared with the manifest tree
  before the run, after setup and after the check; any difference fails the run.
  Untracked and ignored files (dependency installs, build output) are allowed and are
  described in the attestation environment, not prevented.
- A reduced child environment: only variables on the definition's allowlist are
  passed, and the attestation records a digest of each value, never the value.
  Allowlisted variables are still visible to the check, which may leak them.
- A timeout per phase, a bounded output capture, and process-group termination so a
  backgrounded child does not outlive the run. A process that deliberately leaves
  its group (for example with `setsid`) is not tracked.
- A verifier session distinct from the author, created per run by the daemon with no
  path authority and ended afterwards. This separates who signed the evidence from who
  wrote the change. It does not stop a check from reading the daemon's files; the
  verifier key lives only in daemon memory, but the checks run as the same user and
  could attempt to read that process's memory.

A hosted service that runs untrusted agent code and checks would have to put a
boundary around every run. At minimum:

- a per-run container or virtual machine, destroyed after the run;
- no network by default, with an explicit allowlist for dependency installation;
- read-only source: the bound commit mounted read-only, with an ephemeral writable
  filesystem for build output;
- per-run credentials with the least authority, never the user's or the daemon's;
- CPU, memory, process-count, disk and wall-clock limits enforced outside the process;
- separate verifier key custody, so the key that signs attestations is held by the
  service outside the sandbox and never exposed to the code under test; and
- authenticated API callers, so an agent cannot ask for evidence about its own work as
  another party.

Until those exist, treat an attestation as "this command exited with this status on
this tree on this machine", signed by a session that is not the author. It is not proof
that the check was tamper-proof, or that the code under test could not influence the
machine that ran it.

## Bundles, trust and import
A bundle is untrusted input. Its integrity (`verified`, `invalid`, `unavailable`)
is computed from its bytes alone; its trust (`trusted`, `untrusted`, `unknown`)
comes only from the verifier's local trust list or `--trust` fingerprints. Root
and session public keys inside a bundle are needed to check signatures but never
add trust, and the local repository's own root is not trusted implicitly. A
record that is missing, or a Git commit absent from the bundle, makes the result
`unavailable`, never `verified`. `verify` exits 0 with the report unless
`--strict` is given.
Import treats the archive as hostile:
- Staging is a private `0700` directory under `.ledger/runtime/import/`. Only
  `bundle.json`, `records/<aa>/<hash>.json` and `git/objects.bundle` are accepted
  as regular files; absolute, `..`, backslash, duplicate and case-folded alias
  names, links, extended headers, a missing end-of-archive marker, and sizes or
  counts over the limits are rejected before anything is written to the
  repository.
- Git objects are fetched into an inert bare repository with global and system
  config disabled, hooks pointed at `/dev/null` and `transfer.fsckObjects`. No
  check, hook or configuration from the bundle runs or is read.
- Publication creates only `refs/ledger/proposals/<manifest>` (create-only; an
  existing ref at a different commit is reported as `ref_conflict`, never
  overwritten) and appends records through the RecordStore. Code branches, Git
  config, hooks, keys, the trust list, sessions and runtime state are never
  imported. Imported sessions are history: they cannot sign.
- Divergent histories (lifecycle forks, duplicate creations, operation-id
  conflicts) are kept and reported as conflicts; the affected subjects cannot
  mutate, and arrival order or timestamps choose nothing.
What this does not give you: a bundle signed by a root you trust still only says
what that root's sessions recorded; it does not make evidence isolated or prove
which process wrote the code. Fetching `refs/ledger/records` from a remote is
equivalent to importing a bundle and deserves the same trust judgement.
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
