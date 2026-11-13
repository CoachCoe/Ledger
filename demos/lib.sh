#!/usr/bin/env bash
# Shared helpers for demos/m3.sh and demos/governance.sh (sourced, not run).
# Requires: bash, jq. Sets ROOT; callers create WORK and a trap.

need() { command -v "$1" >/dev/null || { echo "demo needs $1" >&2; exit 1; }; }

# step prints a narrative line; fail aborts the demo.
step() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# expect_eq LABEL ACTUAL WANT
expect_eq() { [ "$2" = "$3" ] || fail "$1: got '$2', want '$3'"; echo "ok: $1 = $3"; }

# mkwork prints a short temporary directory: daemon sockets have a ~104 byte
# path limit, which $TMPDIR on macOS often exceeds.
mkwork() { mktemp -d /tmp/ldg.XXXXXX; }

# build_ledger DIR builds the real binary into DIR/ledger.
build_ledger() { (cd "$ROOT" && go build -o "$1/ledger" ./cmd/ledger); }

# new_fixture DIR copies the polyglot fixture into a fresh directory, which
# ledger init turns into a native repository; no Git is involved.
new_fixture() {
  mkdir -p "$1"
  cp -R "$ROOT/testdata/fixtures/polyglot/." "$1/"
}

# copy_repository SRC DST copies a native repository as another machine
# holding it would: its files, records, store and operation logs, without
# the index or runtime state.
copy_repository() {
  mkdir -p "$2"
  (cd "$1" && tar --exclude ./.ledger/runtime --exclude ./.ledger/index.db --exclude ./.ledger/index.db-wal --exclude ./.ledger/index.db-shm --exclude ./.ledger/lock -cf - .) | (cd "$2" && tar -xf -)
}

# lg runs the ledger binary (LEDGER) in the repository REPO and prints stdout.
lg() { (cd "$REPO" && "$LEDGER" "$@"); }

# lg_fail runs a command that must fail and prints its stdout (the error document).
lg_fail() {
  local out
  if out=$(cd "$REPO" && "$LEDGER" "$@"); then fail "expected failure: ledger $*"; fi
  printf '%s\n' "$out"
}

# offline runs a command with the network denied by the kernel when possible.
# Prints the sandbox used to stderr so the demo output is honest about it.
offline() {
  if [ "$(uname -s)" = Linux ] && command -v unshare >/dev/null && unshare -rn true 2>/dev/null; then
    echo "network: disabled with unshare -rn" >&2
    unshare -rn "$@"
  elif [ "$(uname -s)" = Darwin ] && command -v sandbox-exec >/dev/null && sandbox-exec -p '(version 1)(allow default)(deny network*)' true 2>/dev/null; then
    echo "network: disabled with sandbox-exec (deny network*)" >&2
    sandbox-exec -p '(version 1)(allow default)(deny network*)' "$@"
  else
    echo "note: kernel network sandbox unavailable; proxies poisoned only" >&2
    "$@"
  fi
}
