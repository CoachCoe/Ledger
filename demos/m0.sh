#!/usr/bin/env bash
# M0 demo: the record store's acceptance criteria, end to end, in a fresh
# repository. Exits non-zero on the first failed expectation.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export XDG_CONFIG_HOME="$WORK/config" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
(cd "$ROOT" && go build -o "$WORK/bin/ledger" ./cmd/ledger && go build -o "$WORK/bin/m0driver" ./demos/m0driver)
export PATH="$WORK/bin:$PATH"
OUT="$WORK/out"
mkdir -p "$OUT"

step() { printf '\n== %s\n' "$*"; }
field() { sed -n "s/.*\"$1\": \"\{0,1\}\([^\",]*\).*/\1/p" | head -1; }
disk() { find .ledger/objects -type f | wc -l | tr -d ' '; }
ref() { git ls-tree -r refs/ledger/records | wc -l | tr -d ' '; }
check() {
	if ! eval "$1" >/dev/null; then
		echo "FAIL: $2"
		exit 1
	fi
	echo "ok: $2"
}
expect() {
	if [ "$1" != "$2" ]; then
		echo "FAIL: $3 (got '$1', want '$2')"
		exit 1
	fi
	echo "ok: $3"
}

git init -q "$WORK/repo"
cd "$WORK/repo"

step "Schemas validate every example record and the independent signature fixtures"
(cd "$ROOT" && go test -count=1 -run 'TestExamplesRoundTrip|TestIndependentSignatureFixtures' ./internal/envelope)

step "ledger init is idempotent"
first=$(ledger init | field root_key_receipt)
second=$(ledger init | field root_key_receipt)
expect "$second" "$first" "a second init replays the first instead of writing"
expect "$(disk)" 2 "one root key record and its receipt"
expect "$(git status --porcelain)" "" "Ledger state is invisible to git status"

step "The same record hashes the same regardless of key order and whitespace"
printf '{"b":1,"a":[1,2],"c":"\\u00e9"}' >"$OUT/a.json"
printf '{\n  "c" : "é",\n  "a" : [ 1, 2 ],\n  "b" : 1\n}\n' >"$OUT/b.json"
m0driver hash "$OUT/a.json" "$OUT/b.json" | tee "$OUT/hashes"
expect "$(cut -d' ' -f1 "$OUT/hashes" | sort -u | wc -l | tr -d ' ')" 1 "both documents have one hash"

step "Parallel writers in separate processes lose no records"
for _ in 1 2 3 4; do m0driver write 10 >/dev/null & done
wait
expect "$(disk)" 82 "4 processes x 10 operations x (record + receipt), plus init"
expect "$(ref)" "$(disk)" "the records ref holds every object"

step "A crash after the object write but before the ref update is repaired"
m0driver crash ref || true
expect "$(disk)" 84 "the crashed operation's record and receipt are on disk"
expect "$(ref)" 82 "but not yet on the ref"
ledger recover >"$OUT/recover"
expect "$(ref)" "$(disk)" "recovery added them to the ref"

step "A ref moved externally between read and update is reconciled, not overwritten"
m0driver move-ref
expect "$(ref)" "$(disk)" "local and external records are both present"
expect "$(disk)" 88 "the external record and receipt were materialized on disk"

step "A truncated object file is quarantined and reported"
victim=$(find .ledger/objects -type f | sort | head -1)
hash=$(basename "$(dirname "$victim")")$(basename "$victim" .json)
head -c 20 "$victim" >"$OUT/truncated" && mv "$OUT/truncated" "$victim"
ledger recover >"$OUT/recover"
check 'grep -q "\"$hash\"" "$OUT/recover"' "recovery reports $hash as quarantined"
expect "$(find .ledger/quarantine -name "$hash.*" | wc -l | tr -d ' ')" 1 "the damaged file is kept in quarantine"
expect "$(field materialized <"$OUT/recover")" 1 "the intact copy was restored from the ref"

step "Two concurrent transitions from the same previous event: exactly one is accepted"
m0driver race-close | tee "$OUT/race"
expect "$(grep -c ': accepted' "$OUT/race")" 1 "one writer accepted"
check 'grep -q stale_predecessor "$OUT/race"' "the other was rejected as stale"

step "An imported divergent history is reported as a conflict, not resolved"
m0driver import-fork
ledger recover >"$OUT/recover"
check 'grep -q "\"kind\": \"lifecycle_fork\"" "$OUT/recover"' "recovery reports the lifecycle fork"

step "Deleting index.db and reindexing reproduces identical state"
ledger status >"$OUT/before"
rm .ledger/index.db
ledger reindex >"$OUT/reindex"
check 'grep -q "\"kind\": \"lifecycle_fork\"" "$OUT/reindex"' "reindex reports the same conflict"
ledger status >"$OUT/after"
check 'cmp "$OUT/before" "$OUT/after"' "subjects, states and index digest are identical"

printf '\nM0 demo passed.\n'
