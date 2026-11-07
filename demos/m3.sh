#!/usr/bin/env bash
# Reproducible M3 acceptance: evidence, selection, bundle and import failure
# cases (named tests), the public CLI script, and a real-binary portability
# run (AC-173, AC-250): verify a bundle in a fresh directory with the network
# disabled and the original .ledger/ and Git storage unavailable, then import it
# explicitly into a fresh repository.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
# shellcheck source=demos/lib.sh
. "$ROOT/demos/lib.sh"
need git; need jq; need python3

# Every M3 acceptance criterion is a named TestAC<nnn> assertion (see
# internal/verify/testdata/m3_acs.txt, enforced by TestM3ACCoverage).
AC='TestAC(004|033|040|041|046|056|057|058|059|060|078|088|089|090|091|092|093|094|095|096|099|103|107|110|111|112|113|114|115|116|167|168|169|170|171|172|174|175|176|177|178|179|180|209|210|213|230|231|232|235|236|237|247|251)'
for pkg in evidence selection verify record core index; do
  go test -count=1 -v "./internal/$pkg" -run "$AC"
done
go test -count=1 -v ./internal/verify -run '^TestM3ACCoverage$'
# Recovery rows: interruption before/after every publication boundary (AC-103).
go test -count=1 -v ./internal/evidence ./internal/core ./internal/verify ./internal/record -run 'TestVerificationSetupRecovery|TestEvidenceProcessFailure|TestEvidencePublication|TestSelectionPublication|TestExportInterrupted|TestImportStaging|TestImportPublication'
# Bundle failure cases: traversal, links, duplicates, oversize, tampering, forks.
go test -count=1 -v ./internal/verify -run '^FuzzBundleStage$'
go test -count=1 -v ./internal/cli -run '^TestScripts/m3$'

# --- Real binary: portable verification and explicit import -----------------
WORK=$(mkwork)
trap 'if [ -n "${REPO:-}" ] && [ -d "${REPO:-}" ]; then (cd "$REPO" && "$LEDGER" daemon stop >/dev/null 2>&1 || true); fi; chmod -R u+rwx "$WORK" 2>/dev/null || true; rm -rf "$WORK"' EXIT
build_ledger "$WORK"
LEDGER="$WORK/ledger"
export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/cfg"
mkdir -p "$HOME"
REPO="$WORK/repo"
new_fixture "$REPO"

step "Author a change, run independent evidence, judge, select, export"
FP=$(lg init | jq -r .result.root_fingerprint)
cat >"$REPO/.ledger/checks.json" <<'JSON'
[{"name":"unit","category":"mechanical","setup":null,"command":"test -f package.json","working_directory":".","env_allowlist":[],"timeout_seconds":30,"output_limit_bytes":1024}]
JSON
S=$(lg session create '{"agent_name":"author"}')
SID=$(jq -r .result.result.session_id <<<"$S"); CAP=$(jq -r .result.result.capability_id <<<"$S")
INT=$(lg intent create '{"description":"M3 demo","criteria":[{"id":"tests","statement":"package exists","verification":{"kind":"check","check":"unit"}},{"id":"review","statement":"review","verification":{"kind":"judgment","reviewer":"human"}}]}' | jq -r .result.result.intent_id)
W=$(lg workspace fork --session "$SID" "{\"intent_id\":\"$INT\",\"capability_id\":\"$CAP\"}")
WSP=$(jq -r .result.result.workspace_id <<<"$W")
cp "$REPO/tools/report.py" "$(jq -r .result.result.path <<<"$W")/tools/extra.py"
P=$(lg workspace propose --session "$SID" "{\"workspace_id\":\"$WSP\"}")
MANIFEST=$(jq -r .result.result.manifest <<<"$P"); COMMIT=$(jq -r .result.result.commit <<<"$P")
DEF=$(lg evidence resolve "{\"manifest\":\"$MANIFEST\",\"commit\":\"$COMMIT\"}" | jq -r '.result.checks[0].definition_hash')
A=$(lg evidence run "{\"manifest\":\"$MANIFEST\",\"commit\":\"$COMMIT\",\"check_definition\":\"$DEF\"}")
expect_eq "check result" "$(jq -r .result.result.result <<<"$A")" pass
ATT=$(jq -r .result.result.attestation <<<"$A")
lg evidence judge "{\"manifest\":\"$MANIFEST\",\"commit\":\"$COMMIT\",\"criterion_id\":\"review\",\"result\":\"pass\",\"reason\":\"reviewed\"}" >/dev/null
SEL=$(lg selection record "{\"manifest\":\"$MANIFEST\",\"commit\":\"$COMMIT\",\"evidence\":[\"$ATT\"]}")
expect_eq "selection is not acceptance" "$(jq -r .result.result.accepted <<<"$SEL")" false
lg daemon stop >/dev/null
BUNDLE="$WORK/m3.ledger"
expect_eq "export complete" "$(lg export --out "$BUNDLE" | jq -r .result.complete)" true
# Export refuses to overwrite and a missing bundle is unavailable, not valid.
expect_eq "export exists" "$(lg_fail export --out "$BUNDLE" | jq -r .error.code)" export_exists
expect_eq "missing bundle" "$(lg_fail verify --bundle "$WORK/none.ledger" | jq -r .error.code)" bundle_unreadable

step "Lose the source: rename and lock the repository, .ledger/ and Git storage"
mv "$REPO" "$WORK/original"; chmod 000 "$WORK/original"
if [ "$(id -u)" = 0 ]; then
  echo "note: running as root, so chmod cannot deny access; the source is hidden by rename only"
else
  ls "$WORK/original" >/dev/null 2>&1 && fail "original repository is still readable"
  echo "original repository is unreadable"
fi

step "Verify in a fresh directory, scrubbed environment, network disabled"
FRESH=$(mktemp -d "$WORK/f.XXXX"); mkdir "$FRESH/home"
verify() { (cd "$FRESH" && offline env -i PATH="$PATH" HOME="$FRESH/home" HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 ALL_PROXY=http://127.0.0.1:9 "$LEDGER" verify --bundle "$BUNDLE" "$@"); }
V=$(verify --strict)
expect_eq "integrity" "$(jq -r .result.integrity <<<"$V")" verified
expect_eq "trust (no trusted root)" "$(jq -r .result.trust <<<"$V")" untrusted
V=$(verify --trust "$FP")
expect_eq "trust (--trust root)" "$(jq -r .result.trust <<<"$V")" trusted
# Tampering is detected: flip one byte inside a record's data in place (the
# tar headers stay valid; padding is not hashed, so the byte must be data).
cp "$BUNDLE" "$WORK/tampered.ledger"
python3 - "$WORK/tampered.ledger" <<'PY'
import sys, tarfile
p = sys.argv[1]
with tarfile.open(p) as t:
    m = next(m for m in t.getmembers() if m.name.startswith("records/"))
b = bytearray(open(p, "rb").read())
b[m.offset_data + m.size // 2] ^= 0x01
open(p, "wb").write(b)
PY
T=$(cd "$FRESH" && env -i PATH="$PATH" HOME="$FRESH/home" "$LEDGER" verify --bundle "$WORK/tampered.ledger" || true)
[ "$(jq -r '.result.integrity // .error.code' <<<"$T")" != verified ] || fail "tampered bundle verified"
echo "ok: tampered bundle is not verified ($(jq -r '.result.integrity // .error.code' <<<"$T"))"

step "Explicit import into a fresh repository (a different machine has its own root)"
chmod 700 "$WORK/original"
export XDG_CONFIG_HOME="$WORK/cfg2"
REPO="$WORK/fresh-repo"; new_fixture "$REPO"
lg init >/dev/null
I1=$(lg import "$BUNDLE")
I2=$(lg import "$BUNDLE")
echo "first import:  $(jq -c '.result' <<<"$I1" | head -c 300)"
echo "second import: $(jq -c '.result' <<<"$I2" | head -c 300)"
git -C "$REPO" rev-parse --verify -q "refs/ledger/proposals/$MANIFEST" >/dev/null || fail "proposal revision not retained by import"
git -C "$REPO" rev-parse --verify -q refs/heads/ledger >/dev/null 2>&1 && fail "import created a code branch"
V=$(lg verify --strict --trust "$FP")
expect_eq "integrity after import" "$(jq -r .result.integrity <<<"$V")" verified
D1=$(lg status | jq -S -c .); lg import "$BUNDLE" >/dev/null; D2=$(lg status | jq -S -c .)
expect_eq "re-import is idempotent" "$D2" "$D1"

step "M3 acceptance checklist"
for ac in 004 033 040 041 046 056 057 058 059 060 078 088 089 090 091 092 093 094 095 096 099 103 107 110 111 112 113 114 115 116 167 168 169 170 171 172 173 174 175 176 177 178 179 180 209 210 213 230 231 232 235 236 237 247 250 251; do
  printf 'AC-%s ok\n' "$ac"
done
echo "AC-173 (acceptance) and AC-250 (runnable demo) are asserted by this script."
echo "Not covered here: Linux (run in Docker/CI); AC-051 and the speculation half of AC-060/232/247 belong to M5."
