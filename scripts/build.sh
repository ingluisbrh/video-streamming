#!/bin/bash
# Build both binaries into ./bin/
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH=/opt/homebrew/bin:$PATH
go build -o bin/server ./cmd/server
go build -o bin/encrypt ./cmd/encrypt
go build -o bin/player ./cmd/player
echo "built: bin/server bin/encrypt bin/player"
