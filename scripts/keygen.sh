#!/bin/bash
# Generate the master key (once). Usage: ./scripts/keygen.sh
set -euo pipefail
cd "$(dirname "$0")/.."
./scripts/build.sh >/dev/null
KEYFILE="${KEYFILE:-keys.json}"
./bin/encrypt --gen-key --key-file "$KEYFILE"
chmod 600 "$KEYFILE"
echo "Keep $KEYFILE safe and backed up. Lose it = lose the videos."
