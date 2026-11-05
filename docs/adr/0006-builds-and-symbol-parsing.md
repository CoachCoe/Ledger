# ADR 0006: Release builds and tree-sitter symbol parsing

- Status: Accepted
- Date: 2026-11-05

## Context

Symbol analysis uses tree-sitter through cgo, which ties release builds to a C toolchain and to the target's C library. Ledger must install on the Linux distributions and macOS versions customers run.

## Decision

- **Parsing libraries.** `github.com/tree-sitter/go-tree-sitter` with the official grammar bindings: JavaScript (v0.25.0), TypeScript and TSX (v0.23.2), and Python (v0.25.0). Every parser and tree is closed explicitly.
- **macOS builds.** Built natively for arm64. The amd64 build is made on the same host with `CC="clang -arch x86_64"`.
- **Linux builds.** Fully static, built with `-linkmode external -extldflags -static` and the build tags `netgo,osusergo`. With those tags, Go's own networking (used for the daemon's Unix socket) and user lookup are used instead of glibc's, which do not work in a static binary.
- **Analysis caching.** Results are cached by Git blob SHA, so an unchanged file is parsed once no matter how many workspaces or revisions contain it.

## Evidence (Stage 0 spike S1)

- Declarations were extracted correctly:
  - JS: functions, arrow-function and function-expression constants, classes, methods, `export default class`.
  - TS: interfaces, type aliases, enums, abstract classes, methods with optional parameters.
  - TSX: components.
  - Python: functions, classes, methods, decorated methods, nested functions (e.g. `Invoice.refund.inner`).
- Edit-to-symbol mapping uses byte offsets and is correct after multi-byte UTF-8 text earlier in the file. Two edits in different functions of the same file map to different symbols. Module-level edits map to no symbol.
- Parse errors are reported for JS, TS, and Python, and unsupported extensions are rejected.
- Builds:
  - All four platform binaries build.
  - Dynamically linked Linux binaries built on Debian bookworm need glibc 2.34 and fail on Ubuntu 20.04.
  - Static binaries run on Alpine (musl).
- Throughput on a 217 KB TypeScript file (about 4,500 declarations):

| | Time | Rate |
|---|---|---|
| Parse alone | 50 ms | 4.3 MB/s |
| Parse plus extraction | 65 ms | 3.3 MB/s |

## Consequences

- Re-analyzing a changed file as it is observed is cheap: a typical 50 KB file takes about 10 ms.
- A first full index of a very large repository is slow (roughly 25 s per 100 MB of source), so analysis happens lazily and per blob, not upfront.
- Abstract method signatures in TypeScript (`abstract_method_signature`) are not yet mapped to symbols; M4 adds them.
- Release engineering needs per-platform CI runners, or a container build for each Linux architecture.
