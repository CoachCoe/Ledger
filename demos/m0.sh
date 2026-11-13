#!/usr/bin/env bash
# M0 demo: the record store's acceptance criteria, end to end, in a fresh
# native repository. Exits non-zero on the first failed expectation.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export XDG_CONFIG_HOME="$WORK/config"
(cd "$ROOT" && go build -o "$WORK/bin/ledger" ./cmd/ledger && go build -o "$WORK/bin/m0driver" ./demos/m0driver)
export PATH="$WORK/bin:$PATH"
OUT="$WORK/out"
mkdir -p "$OUT"

step() { printf '\n== %s\n' "$*"; }
field() { sed -n "s/.*\"$1\": \"\{0,1\}\([^\",]*\).*/\1/p" | head -1; }
disk() { find .ledger/objects -type f | wc -l | tr -d ' '; }
logged() { wc -l <.ledger/records.log | tr -d ' '; }
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

mkdir "$WORK/repo"
cd "$WORK/repo"

step "Schemas validate every example record and the independent signature fixtures"
(cd "$ROOT" && go test -count=1 -run 'TestExamplesRoundTrip|TestIndependentSignatureFixtures' ./internal/envelope)

step "ledger init is idempotent"
first=$(ledger init | field root_key_receipt)
second=$(ledger init | field root_key_receipt)
expect "$second" "$first" "a second init replays the first instead of writing"
expect "$(disk)" 3 "one root key record, the repository record and their receipt"
expect "$(logged)" 1 "the records head log lists the one group"
check '! test -e .git' "init needs no Git"

step "The same record hashes the same regardless of key order and whitespace"
printf '{"b":1,"a":[1,2],"c":"\\u00e9"}' >"$OUT/a.json"
printf '{\n  "c" : "é",\n  "a" : [ 1, 2 ],\n  "b" : 1\n}\n' >"$OUT/b.json"
m0driver hash "$OUT/a.json" "$OUT/b.json" | tee "$OUT/hashes"
expect "$(cut -d' ' -f1 "$OUT/hashes" | sort -u | wc -l | tr -d ' ')" 1 "both documents have one hash"

step "Parallel writers in separate processes lose no records"
for _ in 1 2 3 4; do m0driver write 10 >/dev/null & done
wait
expect "$(disk)" 83 "4 processes x 10 operations x (record + receipt), plus init"
expect "$(logged)" 41 "the head log lists every group"

step "A crash after the receipt but before the head log append is repaired"
m0driver crash log || true
expect "$(disk)" 85 "the crashed operation's record and receipt are on disk"
expect "$(logged)" 41 "but not yet in the head log"
ledger recover >"$OUT/recover"
expect "$(logged)" 42 "recovery appended its group"

step "A group from another installation arriving mid-write is accepted, not lost"
m0driver external
ledger recover >"$OUT/recover"
expect "$(disk)" 89 "the local and the external record and receipt are on disk"
expect "$(logged)" 44 "both groups are in the head log"

step "A truncated record file is quarantined and its group reported missing"
victim=$(grep -l '"kind":"intent"' $(find .ledger/objects -type f) | sort | head -1)
hash=$(basename "$(dirname "$victim")")$(basename "$victim" .json)
head -c 20 "$victim" >"$OUT/truncated" && mv "$OUT/truncated" "$victim"
ledger recover >"$OUT/recover"
check 'grep -q "\"$hash\"" "$OUT/recover"' "recovery reports $hash as quarantined"
expect "$(find .ledger/quarantine -name "$hash.*" | wc -l | tr -d ' ')" 1 "the damaged file is kept in quarantine"
ledger reindex >"$OUT/reindex"
check 'grep -q "\"record_missing\"" "$OUT/reindex"' "once reindexed, its group is reported missing by the head log"
expect "$(logged)" 44 "the head log keeps the entry rather than dropping it"

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
