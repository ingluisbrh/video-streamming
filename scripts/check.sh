#!/bin/bash
# Smoke-test Range streaming: ./scripts/check.sh <name> [base-url]
# Needs the server running. Auto-unlocks with keys.json if the server is locked.
set -euo pipefail
BASE="${2:-http://127.0.0.1:8080}"
NAME="${1:?usage: $0 <name> [base-url]}"
JAR="$(mktemp)"; trap 'rm -f "$JAR"' EXIT

STATUS="$(curl -s "$BASE/api/status" || true)"
[[ -n "$STATUS" ]] || { echo "server not reachable at $BASE (start it with ./scripts/run.sh)"; exit 1; }
if echo "$STATUS" | python3 -c "import json,sys; sys.exit(0 if json.load(sys.stdin)['locked'] else 1)"; then
  echo "== server locked, unlocking with keys.json =="
  KEYHEX="$(python3 -c "import json; print(json.load(open('keys.json'))['masterKeyHex'])" 2>/dev/null || cat keys.json)"
  curl -sf -c "$JAR" -X POST "$BASE/api/unlock" \
    -H 'Content-Type: application/json' \
    -d "{\"keyHex\":\"$KEYHEX\"}" > /dev/null
  echo "unlocked"
else
  touch "$JAR"
fi
CURL=(curl -sf -b "$JAR")

echo "== list =="
"${CURL[@]}" "$BASE/api/videos" | python3 -m json.tool
SIZE=$("${CURL[@]}" "$BASE/api/videos" | python3 -c "import json,sys; print(next(i['size'] for i in json.load(sys.stdin) if i['name']=='$NAME'))")
echo "plaintext size: $SIZE"

echo "== HEAD =="
curl -s -b "$JAR" -I "$BASE/video/$NAME" | grep -i -E '^(HTTP|Content-|Accept-Ranges)'

echo "== range 0-1023 =="
curl -sf -b "$JAR" -r 0-1023 -o /tmp/range0.bin -D - "$BASE/video/$NAME" | grep -i -E '^(HTTP|Content-Range)'

MID=$((SIZE/2))
echo "== range MID-1024..MID+1023 =="
curl -sf -b "$JAR" -r $((MID-1024))-$((MID+1023)) -o /tmp/range1.bin -D - "$BASE/video/$NAME" | grep -i -E '^(HTTP|Content-Range)'

echo "== suffix last 512 =="
curl -sf -b "$JAR" -r -512 -o /tmp/range2.bin -D - "$BASE/video/$NAME" | grep -i -E '^(HTTP|Content-Range)'
ls -l /tmp/range0.bin /tmp/range1.bin /tmp/range2.bin

echo "== lock =="
curl -sf -b "$JAR" -X POST "$BASE/api/lock" > /dev/null
curl -s -o /dev/null -w 'videos after lock: %{http_code} (want 401)\n' "$BASE/api/videos"
echo OK
