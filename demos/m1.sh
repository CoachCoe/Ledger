#!/usr/bin/env bash
# Public CLI acceptance flow; no model SDK, daemon internals, or paid agents.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
(cd "$ROOT" && go build -o "$WORK/ledger" ./cmd/ledger && go build -o "$WORK/m1driver" ./demos/m1driver)
"$WORK/m1driver" "$WORK/ledger"
