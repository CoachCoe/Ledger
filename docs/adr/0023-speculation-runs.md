# ADR 0023: Speculation runs

Status: implemented. Refines ADR 0015.

## Design

`speculation.start` publishes one receipt group containing, for each attempt, a root-delegated capability, a session and a workspace forked at the run's base commit, then the signed `speculation_run` creation and its `created → collecting` event. The response is the handoff: run id, deadline, scorer hash, pinned check hashes and each attempt's workspace, session, capability, strategy, spec and path. Ledger never launches an agent. Strategies are assigned to attempts round-robin; `n` defaults to the number of strategies.

The run creation is complete when signed. It lists the attempt workspaces with their strategies, the intent's check-definition hashes, the scorer definition and its JCS hash, the base commit, the deadline and the creation time. ADR 0015 let candidate bindings accumulate through submission receipts; binding the attempt workspaces at creation instead means a validator, an exporter and an offline verifier can each tell which workspaces belong to a run from the creation alone. A run requires at least one check criterion on an open intent (`no_check_criterion`). `deadline` or `deadline_seconds`, exactly one, is required.

### Submission is the frozen candidate

`speculation.submit` is a workspace proposal made by the attempt's own session. The resulting change manifest is the candidate; its hash is the candidate id. The index accepts a run workspace's manifest only while the run is collecting, only before the deadline at receipt time, and only if the workspace has none yet. A run workspace accepts no other lifecycle event: rework, close and manual selection fail with `run_active` until the run ends, and the only transitions to `selected` or `closed` are those inside the group that ends the run. A frozen candidate cannot be retargeted. Later disk edits in the worktree are observed but never change it.

Attempt capabilities expire ten minutes after the deadline, so a session can still read status and observe its workspace while the run is evaluated. The deadline itself is enforced by the validator, not by capability expiry, and a late submission fails with `deadline_passed`.

### Evaluation

`speculation.evaluate` (root only) moves the run to `evaluating` with reason `collection_closed`, or `deadline_passed` once the deadline has passed. It then runs every pinned check on every frozen candidate through the M3 evidence path: a fresh verification worktree and a per-run verifier session that is not the author. Evidence runs are sequential because concurrent `git worktree add` in one repository is unsafe. Each run uses an operation id derived from the run, manifest, check and a generation counter, so a retry after a crash reuses the committed attestation instead of running the check again. A journal left interrupted by a crash moves to the next generation, up to 16.

A candidate is eligible when every check criterion is satisfied for its exact revision. Judgment criteria do not affect eligibility. The scorer receives only eligible candidates:

- A `command` scorer runs with `/bin/sh -c` in a scratch directory under `.ledger/runtime/speculate/` with the scorer timeout. It reads the frozen candidate JSON from `$LEDGER_SCORER_INPUT` and writes `{"scores":{"<candidate id>":<integer>,...}}` to `$LEDGER_SCORER_OUTPUT`. Its environment is `LEDGER_REPOSITORY`, `LEDGER_RUN_ID`, those two paths and the definition's allowlist; it has no implicit `PATH`. In a native repository, which has no Git to read candidates from, `LEDGER_CANDIDATES` names a directory holding a fresh checkout of each eligible candidate's tree, named by candidate id; the candidate document itself is unchanged. The result is limited to 1 MiB.
- A `submission` scorer is a person or tool that calls `speculation.score` after `speculation.evaluate` returns `awaiting_scores: true` with the eligible ids and the same candidate document. It has no timeout: the run waits in `evaluating` until scores arrive or the run is cancelled.

Scores must be safe integers, in canonical JSON, with exactly the eligible ids. Anything else, including a scorer that fails, times out or is cancelled, interrupts the run with no winner (`invalid_scores`, `scorer_failed`, `scorer_timeout`). `speculation.resume` returns an interrupted run to the stage it was interrupted in. The run creation pins the candidates, checks and scorer, so a retry uses exactly the same inputs. The highest score wins; equal scores resolve to the lowest manifest hash. With no eligible candidate, the scorer is not run and the run resolves with no winner.

Evaluation runs detached from the caller's connection. A client that disconnects does not cancel checks or the scorer; only `speculation.cancel` does. Cancel publishes the cancellation first, then stops any in-flight evaluation, which then fails with `run_cancelled`.

### One group ends the run

Resolution and cancellation each publish one receipt group, signed by the run's creator, under the caller's operation id:

1. the run's terminal event (`resolved` or `cancelled`);
2. for a winner, a selection decision with selector `{kind: "speculation_run", id: <run>}`, its competitors (the other frozen candidates) and its scoring (scorer hash, eligible ids, scores, tie rule), followed by the winner's `proposed → selected` event;
3. for every other attempt, an `attempt_record` with its outcome (`failed_criteria`, `lost_tiebreaker`, `timeout`, `not_proposed`, `cancelled`), its evidence, failed check criteria and score, followed by its `closed` event;
4. `ended` events for the attempt sessions.

The index rejects a group that leaves any attempt without exactly one outcome, selects more than one candidate, selects one while cancelled, records a score that differs from the selection's scoring, names evidence for another revision, or selects a candidate the scores and tie rule do not choose. Before publishing, `resolve` checks again that the frozen candidates have not changed. The root signs every run record. A run id is never a signing principal. The selection records a choice and is not acceptance: nothing is merged, and the winner's proposal branch is left for a person or another tool to merge.

Worktree removal for losers is an after-effect of the committed group. Removal is forced only when the worktree's `HEAD` is still the base and its content equals the submitted tree recorded in the effect. A worktree holding anything else, such as an edit after submission or an unsubmitted attempt's work, is preserved. In that case the call returns `cleanup_pending` with `committed: true`. The run is still ended.

### Restart

Attempt session keys exist only in daemon memory. After a restart, a collecting run stays collecting, and root-authorized `session.recover` rebinds an attempt workspace to a replacement session that can still submit. A run that was `evaluating` when the daemon stopped is interrupted with reason `daemon_restart`, because the evaluation's in-memory state is gone. `speculation.resume` and `speculation.evaluate` then repeat it, reusing committed attestations.

### Verification

`ledger verify` and `ledger export` handle runs without SQLite. Bundles carry each run's creation, events, attempt records, selection, manifests and evidence. The verifier checks the creation against its intent, derives the run state from its events, requires that an ended run accounts for every attempt exactly once, and rechecks a run selection's competitors, check criteria, scorer hash and winner. Each run is reported under `runs` with its own integrity.

## Validation

`internal/speculate` unit tests cover score parsing, the tie rule, scoring and attempt checks. `internal/core/speculation_test.go` covers AC-119, AC-120 and AC-188 to AC-196: handoff and retry to one run, the check-criterion requirement, frozen submission, deadline timeout, all-fail, cancellation (including an in-flight evaluation), scorer failures, resume with identical inputs, restart in collection and evaluation, a fault at every record-store stage of resolution, forged groups, command scoring with independent evidence, and offline export and verification. `internal/cli/testdata/script/m5.txtar` and `demos/m5.sh` drive the same flows through the shipped binary.
