#!/bin/bash
# Watch without a browser: ./scripts/watch.sh [name]
# Menu mode by default; with a name it plays once and exits.
# Needs mpv (brew install mpv) and the server running (./scripts/run.sh).
set -euo pipefail
cd "$(dirname "$0")/.."
[[ -x bin/player ]] || { ./scripts/build.sh >/dev/null; }
command -v mpv >/dev/null 2>&1 || { echo "mpv not found — brew install mpv"; exit 1; }
if [[ $# -ge 1 ]]; then
  exec ./bin/player --play "$1"
else
  exec ./bin/player
fi
