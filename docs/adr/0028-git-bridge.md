# ADR 0028: Git bridge

- Status: Proposed
- Date: 2026-11-10

## Context

Every code host, CI system and reviewer speaks Git. A product nobody can try
against an existing repository, or whose results cannot reach GitHub, will not
be adopted. The bridge must make Git a place Ledger reads from and writes to,
without Git's model leaking back into the core.

## Decision

**`internal/bridge` is the only package that runs `git`.** It reuses Phase 1's
hardening: global and system config disabled, hooks pointed at `/dev/null`,
`transfer.fsckObjects` on, and work in an inert bare staging repository
(ADR 0021).

- **Import.** `ledger init --from-git <path> [--ref main]` converts the ref's
  first-parent history into native trees, writes one root-signed
  `bridge_mapping` per commit, and keeps Git author, date and message in that
  record. Other branches import on demand with `ledger git import <ref>`.
  Imported history is labelled imported: it carries no intent, capability or
  evidence and satisfies no criterion.
- **Tree mapping.** Git modes map one to one: `100644` to `file`, `100755` to
  `executable`, `120000` to `symlink` and `040000` to `tree`. Submodule entries
  (`160000`) become opaque `gitlink` entries. They are kept so a round trip is
  exact, but are never materialized, and changing them stays unsupported, as
  in Phase 1.
- **Deterministic export.** `ledger git export --line main --to <repo>` writes
  one Git commit per `line_advance`. Author, committer and dates come from the
  signed records, and the message ends with fixed trailers:
  `Ledger-Line-Advance`, `Ledger-Manifest`, `Ledger-Intent`,
  `Ledger-Selection`. The same records always produce the same commit hashes,
  so two machines exporting independently agree.
- **Proposals for review.** `--proposals` also exports each open manifest as a
  branch `ledger/<intent>/<workspace>`, so a reviewer can open a pull request
  on GitHub without knowing Ledger exists.
- **Mirroring.** `ledger git mirror --remote origin` exports and then runs an
  ordinary `git push` of the exported refs. Ledger never force-pushes. A
  remote that moved unexpectedly is reported as `ref_moved`, as Phase 1 does
  for branches.
- **Changes made in Git.** `ledger git sync` imports new commits on a mirrored
  ref as `foreign_change` records. If both the line and the Git ref advanced,
  sync stops and reports `diverged` with both heads. Phase 2 never merges them
  automatically.
- **Bundles.** `ledger export --with-git` adds a Git bundle of the mapped
  commits, so a v2 bundle can still be checked against a Git host's history.

## Alternatives considered

- **Two-way live sync.** Rejected for Phase 2: it needs merge to resolve
  divergence, which is Phase 3's job.
- **Export a Git commit for every operation log entry.** Rejected: it floods
  the host with machine noise. Line advances are the units people review and
  CI runs on.
- **libgit2 instead of the `git` CLI.** Rejected: Phase 1 already validated the
  CLI across Git 2.25 to 2.54, and cgo is already limited to tree-sitter and
  FSEvents.

## Scope of the current build

`init --from-git`, `git import`, `git export --line`, `--proposals`,
`git mirror` and `git sync` work as described above, and P2-AC-030 to
P2-AC-034 are covered by tests on small fixtures. `export --with-git` is
not built yet. Details the decision left open:

- The staging repository is `.ledger/runtime/bridge/bridge-<format>.git`,
  one per Git object format, and only local repositories can be read.
- A repository not imported from Git exports its first tree as a root
  commit by `Ledger root`; its export mapping names neither an advance nor
  a manifest.
- Identities use `@ledger.invalid` addresses: the session id for a session
  and the first 16 hex digits of the fingerprint for root. Dates are whole
  seconds in UTC.
- Mirroring pushes with the user's Git configuration, so credential helpers
  work, with hooks disabled.
- Sync moves the line: running it is the root's acceptance of what was
  committed in Git. Each new first-parent commit becomes a root-signed
  `foreign_change` (`{line, participant: bridge, mapping, base_tree, tree,
  actor, at}`) and a `line_advance` that names it in place of a manifest
  and selection. Only root may sign either, and the advance must still go
  from the line's head and its tree.
- Export writes no commit for a foreign advance: the line's commit is the
  imported one, so commits made in Git keep their ids. Sync reads only
  local repositories, so commits from a remote are fetched first and taken
  from the remote-tracking ref.
- A proposal branch is `ledger/<intent>/<workspace>` with full ids, and
  holds one commit on top of the line commit for the manifest's base tree.
  It is replaced only while it still holds a commit Ledger exported for
  that workspace, and mirrored with `--force-with-lease`. Mirror pushes
  proposals as well, since a pull request needs the branch on the host.
- `ledger verify --git-commit SHA` resolves trailers to the manifest
  without the Git objects: it checks that the export mapping hashes to the
  commit id, and that the Git tree id computed from the native tree in the
  bundle is the commit's tree.

## Validation

- **P2-AC-030:** importing the NestJS fixture at its pinned commit and
  exporting it again reproduces byte-identical Git tree hashes for every
  first-parent commit.
- **P2-AC-031:** two independent exports of the same records produce identical
  commit hashes.
- **P2-AC-032:** a repository with hooks, `trailer.*` config and a malicious
  `.gitattributes` filter runs none of them during import or export.
- **P2-AC-033:** sync after a Git-side commit records one `foreign_change`;
  sync after both sides advanced reports `diverged` and changes nothing.
- **P2-AC-034:** an exported pull-request branch carries trailers that
  `ledger verify --bundle` resolves to the signed manifest and its evidence.
