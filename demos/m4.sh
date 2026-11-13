#!/usr/bin/env bash
# M4: real watcher delivery, symbol provenance and advisory leases, in a
# native repository.
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

step 'M4 acceptance: analyzers, mapping, overlap, queues and lease recovery'
(cd "$ROOT" && go test -race ./internal/symbols ./internal/coord ./internal/core -run 'TestAC(003_184|007_020|117_Locator|117_Rename|187_Parser|182_Python|183_184|117_187|029_065|106_185|186_|053_055)|TestM4(OverlapQueueFaults|LeaseRestartWithoutRoot)' -count=1)
step 'Real daemon: both sessions notified within five seconds, all supported grammars'
(cd "$ROOT" && go test ./internal/daemon -run '^TestAC187_WatcherNotificationsWithinFiveSeconds$' -count=1 -v)

build_ledger "$WORK"
new_fixture "$REPO"
printf 'export function total(x: number) { return x; }\n' > "$REPO/api.ts"
printf "import {total} from './api'; export function caller() { return 0; }\n" > "$REPO/caller.ts"
lg init >/dev/null
session_a=$(lg session create '{}'); session_b=$(lg session create '{}')
sid_a=$(jq -r '.result.result.session_id' <<<"$session_a"); sid_b=$(jq -r '.result.result.session_id' <<<"$session_b")
fork() {
  local intent cap sid
  sid=$(jq -r '.result.result.session_id' <<<"$1")
  cap=$(jq -r '.result.result.capability_id' <<<"$1")
  intent=$(lg intent create '{"description":"M4 coordination","criteria":[{"id":"review","statement":"review","verification":{"kind":"judgment","reviewer":"human"}}]}' | jq -r '.result.result.intent_id')
  lg workspace fork --session "$sid" "$(jq -cn --arg i "$intent" --arg c "$cap" '{intent_id:$i,capability_id:$c}')"
}
wa=$(fork "$session_a"); wb=$(fork "$session_b")
wid_a=$(jq -r '.result.result.workspace_id' <<<"$wa"); wid_b=$(jq -r '.result.result.workspace_id' <<<"$wb")
path_a=$(jq -r '.result.result.path' <<<"$wa"); path_b=$(jq -r '.result.result.path' <<<"$wb")

step 'Concurrent signature change and new call site produce symmetric write/read warnings'
printf 'export function total(x: number,y: number) { return x+y; }\n' > "$path_a/api.ts"
printf "import {total} from './api'; export function caller() { return total(1); }\n" > "$path_b/caller.ts"
for _ in $(seq 1 50); do
  qa=$(lg coordination notifications "$(jq -cn --arg s "$sid_a" '{session_id:$s}')")
  qb=$(lg coordination notifications "$(jq -cn --arg s "$sid_b" '{session_id:$s}')")
  if jq -e '.result.notifications | any(.kind=="overlap" and .overlap_kind=="write_read")' <<<"$qa" >/dev/null && jq -e '.result.notifications | any(.kind=="overlap" and .overlap_kind=="write_read")' <<<"$qb" >/dev/null; then break; fi
  sleep 0.1
done
for q in "$qa" "$qb"; do jq -e '.result.notifications | any(.kind=="overlap" and .overlap_kind=="write_read")' <<<"$q" >/dev/null || fail 'missing symmetric overlap'; done
echo 'ok: both sessions received symbol-level overlap with counterpart and confidence'

step 'An advisory lease warns both parties but leaves proposal publication available'
expires=$(python3 -c 'from datetime import datetime,timedelta,timezone; print((datetime.now(timezone.utc)+timedelta(hours=1)).isoformat(timespec="milliseconds").replace("+00:00","Z"))')
lease=$(lg coordination lease --session "$sid_a" "$(jq -cn --arg s "$sid_a" --arg e "$expires" '{session_id:$s,paths:["caller.ts"],symbols:[],reason:"review caller",expires_at:$e}')")
lease_id=$(jq -r '.result.lease.id' <<<"$lease")
lg coordination notifications "$(jq -cn --arg s "$sid_b" '{session_id:$s}')" | jq -e '.result.notifications | any(.kind=="lease_conflict")' >/dev/null || fail 'missing lease warning'
proposal=$(lg workspace propose --session "$sid_b" "$(jq -cn --arg w "$wid_b" '{workspace_id:$w}')")
manifest=$(jq -r '.result.result.manifest' <<<"$proposal")
record="$REPO/.ledger/objects/${manifest:0:2}/${manifest:2}.json"
jq -e '.payload.symbols_written | any(.name=="caller")' "$record" >/dev/null || fail 'written symbol missing'
jq -e '.payload.symbols_read | any(.path=="api.ts" and .name=="total")' "$record" >/dev/null || fail 'static reference missing'
echo 'ok: proposal succeeded despite lease and records frozen written/read symbols'
lg coordination release --session "$sid_a" "$(jq -cn --arg s "$sid_a" --arg l "$lease_id" '{session_id:$s,lease_id:$l}')" >/dev/null

step 'Batch acknowledgment suppresses redelivery'
ids=$(lg coordination notifications "$(jq -cn --arg s "$sid_b" '{session_id:$s}')" | jq -c '[.result.notifications[].id]')
lg coordination ack --session "$sid_b" "$(jq -cn --arg s "$sid_b" --argjson ids "$ids" '{session_id:$s,notification_ids:$ids}')" >/dev/null
lg coordination notifications "$(jq -cn --arg s "$sid_b" '{session_id:$s}')" | jq -e '.result.notifications | length==0' >/dev/null || fail 'acknowledged notifications redelivered'
lg coordination status "$(jq -cn --arg w "$wid_a" '{workspace_id:$w}')" | jq -e '.result.runtime and .result.analysis.advisory and (.result.snapshot_digest|length==64)' >/dev/null || fail 'runtime/advisory citation missing'
echo 'M4 demo passed: symbols are advisory; missing edges do not establish safety. No semantic merge or dynamic dependency observation is claimed.'
