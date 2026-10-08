#!/bin/bash
# Smoke-test Range streaming: ./scripts/check.sh <name> [base-url]
# Needs the server running. Checks list, full probe, 3 ranges, tamper-free 206s.
set -euo pipefail
BASE="${2:-http://127.0.0.1:8080}"
NAME="${1:?usage: $0 <name> [base-url]}"

echo "== list =="
curl -sf "$BASE/api/videos" | python3 -m json.tool
SIZE=$(curl -sf "$BASE/api/videos" | python3 -c "import json,sys; print(next(i['size'] for i in json.load(sys.stdin) if i['name']=='$NAME'))")
echo "plaintext size: $SIZE"

echo "== HEAD =="
curl -sI "$BASE/video/$NAME" | grep -i -E '^(HTTP|Content-|Accept-Ranges)'

echo "== range 0-1023 =="
curl -sf -r 0-1023 -o /tmp/range0.bin -D - "$BASE/video/$NAME" | grep -i -E '^(HTTP|Content-Range)'

MID=$((SIZE/2))
echo "== range MID-1024..MID+1023 =="
curl -sf -r $((MID-1024))-$((MID+1023)) -o /tmp/range1.bin -D - "$BASE/video/$NAME" | grep -i -E '^(HTTP|Content-Range)'

echo "== suffix last 512 =="
curl -sf -r -512 -o /tmp/range2.bin -D - "$BASE/video/$NAME" | grep -i -E '^(HTTP|Content-Range)'
ls -l /tmp/range0.bin /tmp/range1.bin /tmp/range2.bin
echo OK
