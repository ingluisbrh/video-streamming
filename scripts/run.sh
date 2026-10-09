#!/bin/bash
# Run the server: ./scripts/run.sh [port]
# Starts unlocked if keys.json exists, LOCKED otherwise.
# LOCKED=1 ./scripts/run.sh  -> force locked start (paste key in browser).
# VIDEOS_DIR=/mnt/secure/movies ./scripts/run.sh  -> serve videos from elsewhere.
set -euo pipefail
cd "$(dirname "$0")/.."
PORT="${1:-8080}"
[[ -x bin/server ]] || ./scripts/build.sh
ARGS=(--addr "127.0.0.1:$PORT" --videos "${VIDEOS_DIR:-videos}")
[[ "${LOCKED:-0}" == "1" ]] && ARGS+=(--locked-at-start)
echo "-> http://127.0.0.1:$PORT  (Ctrl-C to stop)"
exec ./bin/server "${ARGS[@]}"
