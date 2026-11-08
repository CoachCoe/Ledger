#!/usr/bin/env bash
# Nine named assertions against a disposable JS/TS/Python Git repository.
# No model, commercial benchmark spend, merge or release approval is involved.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
go test -race ./internal/mcp -run '^TestAC204_Phase1$' -count=1 -v
go test -race ./internal/verify -run '^TestAC088_UnavailableNotInvalid$' -count=1 -v
echo 'Phase 1 engineering demo passed. B.1 commercial validation and owner release/customer gates remain separate.'
