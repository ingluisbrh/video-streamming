#!/bin/bash
# Run the server: ./scripts/run.sh [port]
set -euo pipefail
cd "$(dirname "$0")/.."
PORT="${1:-8080}"
[[ -x bin/server ]] || ./scripts/build.sh
[[ -f keys.json ]] || { echo "no keys.json — run ./scripts/keygen.sh first"; exit 1; }
echo "-> http://127.0.0.1:$PORT  (Ctrl-C to stop)"
exec ./bin/server --addr "127.0.0.1:$PORT" --videos videos
