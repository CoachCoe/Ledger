# ADR 0014: Symbol locators, content revisions, and conservative overlap

- Status: Accepted
- Date: 2026-11-05

## Context

Overlap detection needs symbol identities that stay stable across edits, and confidence labels that never present missing information as safety.

## Decision

A symbol locator combines normalized repository-relative path, language, lexical qualified name, declaration kind, and a disambiguator where needed. A separate revision hash identifies content. Compare base and candidate syntax trees: signature/body edits preserve a locator; rename/move is delete/add with optional advisory links. Same names in other files/scopes are distinct. Ambiguity remains unresolved; do not claim general semantic identity.

Analyze exact base/proposed Git blobs at proposal time, including deletions and agent-declared context references. Cache by blob SHA (ADR 0006). Read references mean static references from written code plus declared context, not observation of what an agent actually read. Preserve confidence and observation source for direct/static-inferred/dynamic-observed/unresolved edges. Do not invent dynamic observation in Phase 1 where none occurred.

Live observations reanalyze write sets and detect write/write and write/read overlap across active open/proposed workspaces. Unsupported extensions, parser failures and unmappable changes use explicitly labeled file-level fallback where applicable. Missing edges never prove safety. Support JS/TS/TSX/Python; abstract TypeScript methods are covered in M4, not a Stage 0 completeness claim.

Both affected sessions receive notifications within five seconds of the second edit with watchers. Reuse M2's queue/ack APIs; deduplicate stable notifications, enforce recipient ownership, make repeated ack safe, preserve ordinary restart delivery, and report/regenerate reindex gaps. Advisory path/symbol leases warn both parties and never block writes; expiry, session end/restart, and reindex remove stale leases consistently. Durable queries return hashes; ephemeral coordination returns observation time and a canonical response snapshot digest, explicitly runtime and unsigned.

## Alternatives considered

Using content hashes as identity makes every body edit appear to be a new symbol. Treating unresolved calls or parser errors as safe creates false confidence. M4 is coordination, not semantic merge.

## Validation

M4.0–M4.8 and M6.2: signature/body/deletion/rename/move/ambiguity fixtures; same-name modules, unrelated functions, fallbacks, all-language signature/call overlap, 5-second delivery, queue/lease recovery.
