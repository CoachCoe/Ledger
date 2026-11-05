# ADR 0015: Asynchronous speculation with frozen candidates and atomic resolution

- Status: Accepted
- Date: 2026-11-05

## Context

D0 records the revised Phase 1 build prompt's execution contracts before M0. Git remains canonical for code; signed records govern provenance. This decision specifies the existing local Phase 1 scope.

## Decision

Signed run creation names stable run id, intent, strategies, pinned scorer definition hash, deadline and candidate bindings as they become frozen through committed records. Creation begins `created`; lifecycle follows created→collecting→evaluating→resolved with cancellation/interruption paths. Resolved/cancelled are terminal. Interrupted preserves the last collecting/evaluating stage and may resume only that stage under replacement authority. Generic lifecycle/receipt schemas exist in M0; orchestration starts in M5.

| From | Allowed next states |
|---|---|
| created | collecting, cancelled |
| collecting | evaluating, cancelled, interrupted |
| evaluating | resolved, cancelled, interrupted |
| interrupted | cancelled, or the recorded prior collecting/evaluating state |
| resolved / cancelled | none |

A crash while still created resumes setup from its journal; it does not invent a prior evaluation stage. Submission's signed operation receipt freezes the run creation hash/id, attempt/workspace id, manifest/commit and check-definition hashes in its result. This supplies immutable candidate bindings during collection without mutating run creation or introducing a new record kind. The evaluating transition names the complete frozen candidate set; subsequent scoring/resolution must use exactly that set. Conflicting submissions remain receipt conflicts, not last-write-wins updates.

Speculate requires a check criterion and returns run/workspace/session/capability handoffs immediately. Submit/status/cancel are provider-neutral asynchronous APIs. Ledger never launches agents. Every attempt has independently authorized session/capability and one frozen manifest/commit at submission. While collecting/evaluating/interrupted, a submitted workspace cannot rework/re-propose. Direct disk edits remain possible but cannot change the frozen input. Reconcile lost keys through ADR 0011; preserve deadlines, strategies and check-definition hashes across restart.

Run independent evidence on frozen revisions. Every check criterion must be satisfied for eligibility. Judgment remains outstanding unless applicable independent evidence exists. An explicit scorer command/submission API consumes frozen candidate JSON and pinned scorer configuration and returns bounded safe-integer scores keyed by exactly the candidate ids. Malformed/missing/extra output, timeout or scorer failure interrupts resolution without selecting. Retry uses identical frozen inputs. Equal scores choose ascending manifest-hash order. All-fail/cancellation selects nothing.

One idempotent receipt group publishes winner selection, terminal run state, winner selected event, loser close events and attempt records. Cancellation/deadline/never-proposed attempts carry explicit outcomes; a loser need not have a manifest. Cleanup occurs after committed closure and is recoverable. No duplicate winner or terminal attempt outcome may appear after retries/restart during collection, evidence, scoring, publication or cleanup. A speculation run is a selecting context; its actual signing principal remains an authorized root/session, never an unauthenticated run id. No record authorizes a merge.

## Alternatives considered

Blocking until agents finish prevents usable CLI/MCP orchestration. Scoring a moving branch retargets evidence. Publishing winner and loser state independently admits partial outcomes.

## Consequences and validation

M5.1–M5.5: immediate handoff, judgment-only rejection, frozen rework denial, all scorer error cases/ties, timeout/cancel/all-fail, restart each stage, atomic terminal group and cleanup; M6 exposes the same APIs.

These are acceptance obligations, not claims that product code already implements them. The untracked milestone plans assign each case to its implementation step.
