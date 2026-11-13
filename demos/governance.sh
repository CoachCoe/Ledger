#!/usr/bin/env bash
# C.1 governance demo: the M0-M3 story end to end on a fixture in a native
# repository, through the real binary. Exits non-zero on any unmet
# expectation.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=demos/lib.sh
. "$ROOT/demos/lib.sh"
need jq
WORK=$(mkwork)
trap 'if [ -n "${REPO:-}" ]; then (cd "$REPO" && "$LEDGER" daemon stop >/dev/null 2>&1 || true); fi; chmod -R u+rwx "$WORK" 2>/dev/null || true; rm -rf "$WORK"' EXIT
build_ledger "$WORK"
export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/cfg"
mkdir -p "$HOME" "$XDG_CONFIG_HOME"
LEDGER="$WORK/ledger"
REPO="$WORK/repo"
new_fixture "$REPO"

step "M0: initialise the ledger; the root key lives outside the repository"
FP=$(lg init | jq -r .result.root_fingerprint)
[ -n "$FP" ] && [ "$FP" != null ] || fail "no root fingerprint"
# A teammate's copy from before the change, to import into later.
copy_repository "$REPO" "$WORK/earlier"
cat >"$REPO/.ledger/checks.json" <<'JSON'
[{"name":"unit","category":"mechanical","setup":null,"command":"test -f package.json","working_directory":".","env_allowlist":[],"timeout_seconds":30,"output_limit_bytes":1024},
 {"name":"gate","category":"mechanical","setup":null,"command":"test -f release-notes.md","working_directory":".","env_allowlist":[],"timeout_seconds":30,"output_limit_bytes":1024}]
JSON

step "M2: the root issues a session scoped to src/billing/**"
S=$(lg session create '{"agent_name":"billing-agent","allow":["src/billing/**"],"deny":["src/billing/private/**"],"may_add_dependencies":false}')
SID=$(jq -r .result.result.session_id <<<"$S")
CAP=$(jq -r .result.result.capability_id <<<"$S")

step "M1: intent with a passing check, a failing check and a human review"
I=$(lg intent create '{"description":"Billing change","criteria":[{"id":"tests","statement":"package exists","verification":{"kind":"check","check":"unit"}},{"id":"gate","statement":"release notes exist","verification":{"kind":"check","check":"gate"}},{"id":"review","statement":"human review","verification":{"kind":"judgment","reviewer":"human"}}]}')
INT=$(jq -r .result.result.intent_id <<<"$I")
W=$(lg workspace fork --session "$SID" "{\"intent_id\":\"$INT\",\"capability_id\":\"$CAP\"}")
WSP=$(jq -r .result.result.workspace_id <<<"$W")
WPATH=$(jq -r .result.result.path <<<"$W")

step "M2: a proposal outside the granted scope is denied"
cp "$REPO/tools/report.py" "$WPATH/unauthorized.py"
D=$(lg_fail workspace propose --session "$SID" "{\"workspace_id\":\"$WSP\"}")
expect_eq "proposal denial" "$(jq -r .error.code <<<"$D")" scope_violation
rm "$WPATH/unauthorized.py"

step "M1: an in-scope proposal succeeds, bound to its tree and operation log"
cp "$REPO/src/billing/tax.js" "$WPATH/src/billing/added.js"
P=$(lg workspace propose --session "$SID" "{\"workspace_id\":\"$WSP\"}")
CHANGE=$(jq -r .result.result.change_id <<<"$P")
MANIFEST=$(jq -r .result.result.manifest <<<"$P")
TREE=$(jq -r .result.result.tree <<<"$P")
[ "$(jq -r .result.result.op_head <<<"$P")" != null ] || fail "proposal pins no operation log head"

step "M3: independent evidence for the exact revision (author cannot verify itself)"
R=$(lg evidence resolve "{\"change_id\":\"$CHANGE\"}")
UNIT=$(jq -r '.result.checks[]|select(.name=="unit").definition_hash' <<<"$R")
GATE=$(jq -r '.result.checks[]|select(.name=="gate").definition_hash' <<<"$R")
A=$(lg evidence run "{\"manifest\":\"$MANIFEST\",\"check_definition\":\"$UNIT\"}")
expect_eq "unit run result" "$(jq -r .result.result.result <<<"$A")" pass
ATT_PASS=$(jq -r .result.result.attestation <<<"$A")
expect_eq "verifier is not the author" "$([ "$(jq -r .result.result.verifier_session <<<"$A")" != "$SID" ] && echo yes)" yes
E=$(lg_fail evidence run --session "$SID" "{\"manifest\":\"$MANIFEST\",\"check_definition\":\"$UNIT\"}")
expect_eq "author refused as verifier" "$(jq -r .error.code <<<"$E")" unauthorized

step "M3: a failing run is evidence too"
B=$(lg evidence run "{\"manifest\":\"$MANIFEST\",\"check_definition\":\"$GATE\"}")
expect_eq "gate run result" "$(jq -r .result.result.result <<<"$B")" fail
ATT_FAIL=$(jq -r .result.result.attestation <<<"$B")
lg evidence judge "{\"manifest\":\"$MANIFEST\",\"criterion_id\":\"review\",\"result\":\"pass\",\"reason\":\"reviewed by owner\"}" >/dev/null

step "M3: criteria status lists contrary evidence"
C=$(lg intent criteriaStatus "{\"manifest\":\"$MANIFEST\"}")
expect_eq "tests" "$(jq -r '.result.criteria[]|select(.criterion_id=="tests").status' <<<"$C")" satisfied
expect_eq "gate" "$(jq -r '.result.criteria[]|select(.criterion_id=="gate").status' <<<"$C")" failed
expect_eq "review" "$(jq -r '.result.criteria[]|select(.criterion_id=="review").status' <<<"$C")" satisfied
jq -e --arg h "$ATT_FAIL" '.result.contrary_evidence|index($h)' <<<"$C" >/dev/null || fail "the failing run is not listed as contrary evidence"
echo "contrary evidence: $(jq -c .result.contrary_evidence <<<"$C")"

step "M3: selection records the recomputed statuses; it is NOT acceptance"
SEL=$(lg selection record "{\"manifest\":\"$MANIFEST\",\"evidence\":[\"$ATT_PASS\"]}")
expect_eq "accepted" "$(jq -r .result.result.accepted <<<"$SEL")" false
# Acceptance is a separate signed line_advance; a selection writes none.
grep -l '"kind":"line_advance"' $(find "$REPO/.ledger/objects" -type f) >/dev/null 2>&1 && fail "selection advanced a line"
echo "selection advanced no line and merged nothing (selection is not acceptance)"
lg daemon stop >/dev/null

step "M3: export, then lose the source and verify offline in a fresh directory"
BUNDLE="$WORK/governance.ledger"
EX=$(lg export --out "$BUNDLE")
expect_eq "export complete" "$(jq -r .result.complete <<<"$EX")" true
mv "$REPO" "$WORK/repo.gone"; chmod 000 "$WORK/repo.gone"
FRESH=$(mktemp -d "$WORK/f.XXXX"); mkdir "$FRESH/empty-home"
verify() { (cd "$FRESH" && offline env -i PATH="$PATH" HOME="$FRESH/empty-home" HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 "$LEDGER" verify --bundle "$BUNDLE" "$@"); }
V=$(verify)
expect_eq "integrity" "$(jq -r .result.integrity <<<"$V")" verified
expect_eq "trust without a trusted root" "$(jq -r .result.trust <<<"$V")" untrusted
V=$(verify --trust "$FP")
expect_eq "trust with --trust" "$(jq -r .result.trust <<<"$V")" trusted

step "M3: explicit import into an earlier copy of the repository"
chmod 700 "$WORK/repo.gone"
REPO="$WORK/earlier"
lg import "$BUNDLE" >/dev/null
V=$(lg verify --strict --trust "$FP") || { echo "$V" | head -c 3000; fail "verify after import"; }
expect_eq "imported tree" "$(jq -r --arg m "$MANIFEST" '.result.manifests[]|select(.hash==$m).tree' <<<"$V")" "$TREE"
echo "imported history verifies in the earlier copy; nothing was trusted by import"
step "governance demo passed"
echo "Proved: scoped authority, proposal denial, independent exact-revision evidence, contrary evidence, selection without acceptance, offline provenance with integrity/trust separation, explicit import."
echo "Not proved: isolation (cooperative enforcement only), hosted trust, multi-machine operation. See docs/security-model.md."
