#!/usr/bin/env bash
# M5: speculation runs with scripted attempts (no model): evaluation against
# a pinned check, a command scorer, every way a run can end, and offline
# verification, in a native repository. Nothing is merged.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
source "$ROOT/demos/lib.sh"
need jq; need python3
WORK=$(mkwork)
REPO="$WORK/repo"
LEDGER="$WORK/ledger"
cleanup() { if [ -d "$REPO/.ledger" ]; then lg daemon stop >/dev/null 2>&1 || true; fi; rm -rf "$WORK"; }
trap cleanup EXIT
export XDG_CONFIG_HOME="$WORK/config"

step 'M5 acceptance: run validation, scoring, recovery and publication atomicity'
(cd "$ROOT" && go test -race ./internal/speculate ./internal/core -run 'TestAC(119|120|188|189|190|194|196)_|Test(ParseScores|WinnerTieRule|CheckScoring|CheckAttempt|ValidateRun)$' -count=1)

build_ledger "$WORK"
new_fixture "$REPO"
lg init >/dev/null
cat >"$REPO/.ledger/checks.json" <<'EOF'
[{"name":"unit","category":"mechanical","setup":null,"command":"test -f ok.txt","working_directory":".","env_allowlist":[],"timeout_seconds":30,"output_limit_bytes":1024}]
EOF
# The scorer prefers the longer ok.txt. It refuses to score while the
# outage marker exists, to show an interrupted resolution.
cat >"$WORK/score.py" <<'EOF'
import json, os, sys
if os.path.exists(os.environ["SCORER_OUTAGE"]):
    sys.exit("scoring service unavailable")
doc = json.load(open(os.environ["LEDGER_SCORER_INPUT"]))
def size(c):
    return os.path.getsize(os.path.join(os.environ["LEDGER_CANDIDATES"], c["id"], "ok.txt"))
json.dump({"scores": {c["id"]: size(c) for c in doc["candidates"]}}, open(os.environ["LEDGER_SCORER_OUTPUT"], "w"))
EOF
export SCORER_OUTAGE="$WORK/outage"
intent=$(lg intent create '{"description":"M5 speculation","criteria":[{"id":"ok","statement":"ok.txt exists","verification":{"kind":"check","check":"unit"}}]}' | jq -r '.result.result.intent_id')
scorer=$(jq -cn --arg py "$WORK/score.py" '{kind:"command",command:("python3 " + $py),timeout_seconds:60,env_allowlist:["PATH","SCORER_OUTAGE"]}')

# start N DEADLINE_SECONDS SCORER prints the handoff of a run with one
# attempt for each of the first N strategies.
start() {
  lg speculation start "$(jq -cn --arg i "$intent" --argjson n "$1" --argjson d "$2" --argjson s "$3" \
    '{intent_id:$i,deadline_seconds:$d,scorer:$s,strategies:[{name:"careful",spec:"write a thorough ok.txt"},{name:"quick",spec:"write a short ok.txt"},{name:"forgetful",spec:"forget ok.txt"}][:$n]}')"
}
# attempt HANDOFF I FIELD
attempt() { jq -r --argjson i "$2" ".result.result.attempts[\$i].$3" <<<"$1"; }
# submit HANDOFF I submits attempt I from its own session.
submit() {
  lg speculation submit --session "$(attempt "$1" "$2" session_id)" \
    "$(jq -cn --arg r "$(jq -r '.result.result.run_id' <<<"$1")" --arg w "$(attempt "$1" "$2" workspace_id)" '{run_id:$r,workspace_id:$w}')" >/dev/null
}
run_of() { jq -cn --arg r "$(jq -r '.result.result.run_id' <<<"$1")" '{run_id:$r}'; }
outcomes() { lg speculation status "$1" | jq -c '[.result.attempts[] | {strategy, outcome}]'; }

step 'Three scripted attempts: one fails the check, one loses on score, one is selected'
h=$(start 3 600 "$scorer")
printf 'a thorough and complete answer\n' >"$(attempt "$h" 0 path)/ok.txt"
printf 'short\n' >"$(attempt "$h" 1 path)/ok.txt"
printf 'no ok.txt here\n' >"$(attempt "$h" 2 path)/notes.txt"
for i in 0 1 2; do submit "$h" "$i"; done
out=$(lg speculation evaluate "$(run_of "$h")")
expect_eq 'run state' "$(jq -r '.result.result.state' <<<"$out")" resolved
expect_eq 'winner strategy' "$(jq -r '.result.result.attempts[] | select(.outcome=="selected") | .strategy' <<<"$out")" careful
expect_eq 'loser outcome' "$(jq -r '.result.result.attempts[] | select(.strategy=="quick") | .outcome' <<<"$out")" lost_tiebreaker
expect_eq 'ineligible outcome' "$(jq -r '.result.result.attempts[] | select(.strategy=="forgetful") | .outcome' <<<"$out")" failed_criteria
expect_eq 'accepted' "$(jq -r '.result.result.accepted' <<<"$out")" false
expect_eq 'merged' "$(jq -r '.result.result.merged' <<<"$out")" false
winner=$(jq -r '.result.result.winner.manifest' <<<"$out")
expect_eq 'winner binding frozen' "$(lg evidence resolve "$(jq -cn --arg m "$winner" '{manifest:$m}')" | jq -r .result.frozen)" true
echo "ok: winner $winner keeps its frozen tree; losers closed with signed attempt records"

step 'All attempts fail the check: the run resolves with no winner'
h=$(start 2 600 "$scorer")
for i in 0 1; do printf 'x\n' >"$(attempt "$h" "$i" path)/other.txt"; submit "$h" "$i"; done
out=$(lg speculation evaluate "$(run_of "$h")")
expect_eq 'winner' "$(jq -c '.result.result.winner' <<<"$out")" null
expect_eq 'outcomes' "$(jq -c '[.result.result.attempts[].outcome] | unique' <<<"$out")" '["failed_criteria"]'

step 'Cancellation selects nothing'
h=$(start 2 600 "$scorer")
printf 'done\n' >"$(attempt "$h" 0 path)/ok.txt"; submit "$h" 0
out=$(lg speculation cancel "$(jq -c '. + {reason:"superseded"}' <<<"$(run_of "$h")")")
expect_eq 'run state' "$(jq -r '.result.result.state' <<<"$out")" cancelled
expect_eq 'outcomes' "$(outcomes "$(run_of "$h")" | jq -c '[.[].outcome] | unique')" '["cancelled"]'

step 'Deadline: an unsubmitted attempt times out and cannot submit late'
h=$(start 2 5 "$scorer")
printf 'on time\n' >"$(attempt "$h" 0 path)/ok.txt"; submit "$h" 0
until lg speculation status "$(run_of "$h")" | jq -e '.result.deadline_passed' >/dev/null; do sleep 0.2; done
printf 'late\n' >"$(attempt "$h" 1 path)/ok.txt"
late=$(lg_fail speculation submit --session "$(attempt "$h" 1 session_id)" "$(jq -cn --arg r "$(jq -r '.result.result.run_id' <<<"$h")" --arg w "$(attempt "$h" 1 workspace_id)" '{run_id:$r,workspace_id:$w}')")
expect_eq 'late submission' "$(jq -r '.error.code' <<<"$late")" deadline_passed
# The late edit is never discarded: closing the timed-out workspace records
# it in the workspace's operation log before the checkout is removed.
lg speculation evaluate "$(run_of "$h")" >/dev/null
[ -e "$(attempt "$h" 1 path)" ] && fail 'the timed-out checkout was kept'
# Its log holds more than the creation entry: a snapshot of the late edit.
[ "$(wc -l <"$REPO/.ledger/oplog/$(attempt "$h" 1 workspace_id).log")" -gt 1 ] || fail 'the late edit was not recorded'
expect_eq 'timed out' "$(outcomes "$(run_of "$h")" | jq -r '.[] | select(.strategy=="quick") | .outcome')" timeout
expect_eq 'selected' "$(outcomes "$(run_of "$h")" | jq -r '.[] | select(.strategy=="careful") | .outcome')" selected

step 'Scorer outage interrupts without a winner; resume retries the frozen candidates'
h=$(start 2 600 "$scorer")
printf 'longer answer\n' >"$(attempt "$h" 0 path)/ok.txt"
printf 'short\n' >"$(attempt "$h" 1 path)/ok.txt"
for i in 0 1; do submit "$h" "$i"; done
touch "$SCORER_OUTAGE"
out=$(lg_fail speculation evaluate "$(run_of "$h")")
expect_eq 'error' "$(jq -r '.error.code' <<<"$out")" scorer_failed
expect_eq 'state' "$(lg speculation status "$(run_of "$h")" | jq -r '.result.state')" interrupted
rm "$SCORER_OUTAGE"
expect_eq 'resume' "$(lg speculation resume "$(run_of "$h")" | jq -r '.result.result.state')" evaluating
out=$(lg speculation evaluate "$(run_of "$h")")
expect_eq 'winner strategy' "$(jq -r '.result.result.attempts[] | select(.outcome=="selected") | .strategy' <<<"$out")" careful

step 'Export and verify every run offline, without the daemon or index'
lg daemon stop >/dev/null
lg export --out "$WORK/m5.ledger" | jq -e '.result.complete' >/dev/null || fail 'export incomplete'
report=$(offline "$LEDGER" verify --bundle "$WORK/m5.ledger" --strict)
expect_eq 'integrity' "$(jq -r '.result.integrity' <<<"$report")" verified
expect_eq 'runs verified' "$(jq -c '[.result.runs[] | .integrity] | unique' <<<"$report")" '["verified"]'
expect_eq 'run states' "$(jq -c '[.result.runs[].state] | sort' <<<"$report")" '["cancelled","resolved","resolved","resolved","resolved"]'
echo 'M5 demo passed: a selection records a choice by score among eligible candidates; nothing is accepted or merged.'
