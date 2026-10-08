#!/bin/bash
# Add a video: ./scripts/add-video.sh INPUT.mp4 [name]
# No complex flags: builds, encrypts to videos/<name>.cenc, refresh browser.
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ $# -lt 1 ]]; then echo "usage: $0 INPUT.mp4 [name]"; exit 2; fi
IN="$1"
NAME="${2:-$(basename "$IN" | sed 's/\.[^.]*$//')}"
# sanitize: keep alnum dot dash underscore
NAME="$(echo "$NAME" | tr ' ' '_' | tr -cd 'A-Za-z0-9._-')"
OUT="videos/${NAME}.cenc"

[[ -f "$IN" ]] || { echo "input not found: $IN"; exit 1; }
[[ -f keys.json ]] || { echo "no keys.json — run ./scripts/keygen.sh first"; exit 1; }
[[ -e "$OUT" ]] && { echo "$OUT exists. rm it first or pick another name."; exit 1; }
[[ -x bin/encrypt ]] || ./scripts/build.sh

ARGS=(--in "$IN" --out "$OUT")
command -v ffmpeg >/dev/null 2>&1 && ARGS+=(--fix-faststart)
./bin/encrypt "${ARGS[@]}"
echo "done. Open http://127.0.0.1:8080 and pick '$NAME'."
