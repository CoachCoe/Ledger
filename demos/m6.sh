#!/usr/bin/env bash
# M6 engineering acceptance: contracts, queries, retries and real MCP flow.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
go test -race -p 1 ./internal/mcp ./internal/core -run 'TestAC(006|199|202|201)_|TestWorkspaceWrite|TestQueryPaths|TestM6' -count=1 -v
bash demos/phase1.sh
