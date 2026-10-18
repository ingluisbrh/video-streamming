# AGENTS.md

## Toolchain

- `go` is Homebrew-only here: prefix commands with `export PATH=/opt/homebrew/bin:$PATH`.
- Module `streaming` (go 1.27.1). Server/encrypt/chunked/legacy are **stdlib only**; `cmd/player` additionally uses the Bubble Tea v2 stack (`charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, `charm.land/lipgloss/v2`) — don't pull those into the rest. `gofmt -l .` must be empty; run `go vet ./...`, `go test ./...`.
- Single package with tests: `go test ./internal/chunked/`. Server auth flow: `go test ./cmd/server/`. Player helpers/TUI: `go test ./cmd/player/`. No CI, no lint config.

## Workflow (use wrappers, not raw binaries)

- `./scripts/build.sh` → `bin/server`, `bin/encrypt`. `./scripts/keygen.sh` (respects `KEYFILE=`), `./scripts/add-video.sh IN.mp4 [name]`, `./scripts/add-image.sh IN.jpg [name]` (keeps the original extension in the name), `./scripts/migrate-old.sh OLD.enc [name]`, `./scripts/decrypt.sh NAME [outdir]` (reverses `.cenc` → original, needs same key), `./scripts/run.sh [port]`, `./scripts/check.sh <name>` (needs server running).
- Video dir: `VIDEOS_DIR` env (default `videos`) is honored by `run.sh`, `add-video.sh`, `migrate-old.sh` and by the server's `--videos` default (`config.VideosDir()`); the `--videos` flag overrides the env. Dir is created by the add/migrate scripts if missing.
- Image dir: `IMAGES_DIR` env (default `images`) is honored by `add-image.sh` and by the server's `--images` default (`config.ImagesDir()`); the `--images` flag overrides the env.
- Key precedence (`internal/config/keys.go`): `--key-file` > `STREAM_KEY_HEX` env > `keys.json` > `config/keys.json`.

## Architecture

- `internal/chunked/` owns the `.cenc` format. Header 29B: `CENC|ver=1|chunkSize u32 BE|baseNonce 12|plainSize u64 BE`; then `[len u32][GCM(chunk)]` per chunk. Nonce = baseNonce XOR counter (`nonceFor`). Chunk file offsets are arithmetic (full chunk = 4 + chunkSize + 16 tag) — no index; see `ChunkFileOffset`.
- `internal/legacy/aes.go` is the original whole-file GCM (`nonce+ciphertext`), migration-only. Whole-file GCM is **not seekable** — never serve Ranges off `.enc`; migrate first.
- `cmd/server` serves `web/index.html` from disk relative to cwd — always run from repo root. Single `Range` only; multipart/malformed → `416`. Names allowlisted to `[A-Za-z0-9][A-Za-z0-9._-]*` (traversal → 404). `GET /api/videos` re-reads the dir per request (no restart needed); corrupt/partial `.cenc` skipped silently. Videos and images are the same format in different dirs: `serveCenc` is the shared decrypt+`Range` core, `GET /api/images` + `GET|HEAD /image/{name}` mirror the video handlers, and the image `Content-Type` comes from `sniffImage` (first ≤512 plaintext bytes: JPEG/PNG/GIF/WebP/AVIF/HEIF/BMP) → `mime.TypeByExtension` → `application/octet-stream`. `listCenc`/`resolveIn` are pure helpers over a dir argument.
- Auth: `keyStore` (RAM-only key, `open` vs session mode, idle auto-lock). Boot with configured key → open mode (no cookie); `--locked-at-start` or no key → locked, unlock via `POST /api/unlock` (verified best-effort with `chunked.VerifyKey` against `firstCenc()`, which finds *any* `.cenc` — videos first, then images, so an image-only library still verifies). `/api/videos` + `/video/*` + `/api/images` + `/image/*` gated → `401` with `{"error":"locked"}`. Handlers snapshot the key via `use()` — never hold `ks.key` across calls. `wipeLocked` zeroes + sets `open=false`.
- `cmd/player` (Bubble Tea v2, needs `mpv` binary): unlock → full-screen list (videos + images, type-to-filter) → drives mpv. Split across `main.go` (flags/boot), `api.go` (HTTP + `mediaItem`), `mpv.go` (IPC), `tui.go` (the UI). The session cookie reaches mpv via IPC `set http-header-fields` (**string**, not array — "String list" type); the `--play` one-shot and `t` terminal mode pass it via argv (local `ps`-visible, documented in code). `mpvCtl` owns the background mpv: closing its window quits mpv, so the next command respawns it (re-sets the header) instead of writing to a dead socket. `mpv.go`'s `ipc.call` sends a `request_id` and **skips `{"event":...}` lines** — mpv interleaves events with replies, and mistaking one for a reply breaks the progress poll. The TUI needs a TTY (errors otherwise); `--play` still works piped. Playback is mpv's window (default) or the current terminal via `--vo=tct` (`t` toggle, hands the tty over with `tea.ExecProcess`). Any post-unlock failure must go through `failf`/error returns so the session locks — never call bare `fatalf` after unlock; the TUI locks after `p.Run()` returns (Ctrl-C included).
- `cmd/encrypt` uses `O_EXCL` — refuses to overwrite; `EncryptFile` streams at ~chunkSize RAM, but `--migrate-old` holds full plaintext in RAM (one-time cost). `--decrypt` (`chunked.DecryptFile`) streams chunk-by-chunk back to the original and deletes the partial output on any auth failure. `faststartStatus` is a pure-Go box scan (no ffmpeg here); `--fix-faststart` needs ffmpeg or only warns.

## Constraints

- Never commit secrets or media: `bin/`, `keys.json`, `config/keys.json`, `*.cenc` are git-ignored; `videos/`, `images/`, `incoming/` exist via `.gitkeep`.
- Server binds localhost by default (`-addr 127.0.0.1:8080`); key never leaves the server — don't add endpoints that expose it.
- 0-byte videos list with size 0 but fail playback (`StreamRange` errors); don't "fix" by special-casing unless asked.
