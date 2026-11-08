#!/usr/bin/env bash
# Reproducible M2 acceptance: core fault/recovery cases and public CLI flow.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
go test -count=1 -v ./internal/capability -run 'Test(PathLanguage|PatternBoundariesAndCasePolicy|NarrowingAndInheritedDenies|DelegatedPathsNeverWiden|DependencyFiles|M2)'
go test -count=1 -v ./internal/workspace -run '^TestM2'
go test -count=1 -v ./internal/core -run '^TestM2'
go test -count=1 -v ./internal/record -run '^TestM2'
go test -count=1 -v ./internal/cli -run '^TestScripts/m2$'
