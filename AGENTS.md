# AGENTS.md

## Toolchain

- `go` is Homebrew-only here: prefix commands with `export PATH=/opt/homebrew/bin:$PATH`.
- Module `streaming` (go 1.27.1), stdlib only. `gofmt -l .` must be empty; run `go vet ./...`, `go test ./...`.
- Single package with tests: `go test ./internal/chunked/`. No CI, no lint config.

## Workflow (use wrappers, not raw binaries)

- `./scripts/build.sh` → `bin/server`, `bin/encrypt`. `./scripts/keygen.sh` (respects `KEYFILE=`), `./scripts/add-video.sh IN.mp4 [name]`, `./scripts/migrate-old.sh OLD.enc [name]`, `./scripts/run.sh [port]`, `./scripts/check.sh <name>` (needs server running).
- Key precedence (`internal/config/keys.go`): `--key-file` > `STREAM_KEY_HEX` env > `keys.json` > `config/keys.json`.

## Architecture

- `internal/chunked/` owns the `.cenc` format. Header 29B: `CENC|ver=1|chunkSize u32 BE|baseNonce 12|plainSize u64 BE`; then `[len u32][GCM(chunk)]` per chunk. Nonce = baseNonce XOR counter (`nonceFor`). Chunk file offsets are arithmetic (full chunk = 4 + chunkSize + 16 tag) — no index; see `ChunkFileOffset`.
- `internal/legacy/aes.go` is the original whole-file GCM (`nonce+ciphertext`), migration-only. Whole-file GCM is **not seekable** — never serve Ranges off `.enc`; migrate first.
- `cmd/server` serves `web/index.html` from disk relative to cwd — always run from repo root. Single `Range` only; multipart/malformed → `416`. Names allowlisted to `[A-Za-z0-9][A-Za-z0-9._-]*` (traversal → 404). `GET /api/videos` re-reads the dir per request (no restart needed); corrupt/partial `.cenc` skipped silently.
- `cmd/encrypt` uses `O_EXCL` — refuses to overwrite; `EncryptFile` streams at ~chunkSize RAM, but `--migrate-old` holds full plaintext in RAM (one-time cost). `faststartStatus` is a pure-Go box scan (no ffmpeg here); `--fix-faststart` needs ffmpeg or only warns.

## Constraints

- Never commit secrets or media: `bin/`, `keys.json`, `config/keys.json`, `videos/*.cenc` are git-ignored; `videos/`, `incoming/` exist via `.gitkeep`.
- Server binds localhost by default (`-addr 127.0.0.1:8080`); key never leaves the server — don't add endpoints that expose it.
- 0-byte videos list with size 0 but fail playback (`StreamRange` errors); don't "fix" by special-casing unless asked.
