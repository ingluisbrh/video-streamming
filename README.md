# Local encrypted video streaming (AES-256-GCM chunked, seekable)

Key never leaves the server. Browser only sees `http://127.0.0.1:8080/video/<name>`.

## Once

```bash
./scripts/keygen.sh        # create keys.json (back it up — lose it = lose videos)
```

## Add videos (no complex commands)

```bash
./scripts/add-video.sh movie.mp4            # -> videos/movie.cenc
./scripts/add-video.sh movie.mp4 myname     # custom name
./scripts/migrate-old.sh oldmovie.enc       # legacy whole-file .enc -> .cenc
./scripts/run.sh                            # serve http://127.0.0.1:8080
./scripts/check.sh moviename                # verify ranges (server must run)
```

New files appear on page refresh — no restart, no DB. Delete = `rm videos/x.cenc`.

## Add images (same format, shown in the browser)

Images use the exact same `.cenc` encryption — they just live in their own
folder and get their own endpoint:

```bash
./scripts/add-image.sh photo.jpg            # -> images/photo.jpg.cenc
./scripts/add-image.sh scan.png logo        # custom name
IMAGES_DIR=/mnt/secure/pics ./scripts/add-image.sh photo.jpg
```

Open the page: videos keep their buttons, images appear as a thumbnail grid
below — click one for the full-size view (Esc or click to close).

- Served by `GET /image/<name>`, gated exactly like `/video/<name>`: `401`
  while locked, session cookie otherwise. The browser only ever receives
  decrypted bytes.
- The `Content-Type` is sniffed from the plaintext magic bytes
  (JPEG/PNG/GIF/WebP/AVIF/HEIF/BMP), falling back to the file extension —
  so a name with no extension (`add-image.sh scan.png logo`) still shows
  correctly as `image/png`.
- Unlocking is verified against *any* `.cenc`, so a library of images alone
  still works for the paste-the-key flow.
- `--images` flag > `IMAGES_DIR` env > `images/`.

## Store videos outside the repo (VIDEOS_DIR)

Point everything at any folder — server, `add-video.sh` and `migrate-old.sh`
all honor the same env var:

```bash
export VIDEOS_DIR=/mnt/secure/movies   # or ~/Private/videos, an external disk, ...
./scripts/add-video.sh movie.mp4       # -> $VIDEOS_DIR/movie.cenc (dir auto-created)
./scripts/run.sh                       # server serves $VIDEOS_DIR
```

- One-shot: `VIDEOS_DIR=/mnt/secure/movies ./scripts/run.sh`
- Or flag only: `./bin/server --videos /mnt/secure/movies` (flag > env > `videos`).
- The folder can live outside the repo; only `*.cenc` files are listed/served,
  name allowlisting still applies — no path traversal. Keep `keys.json` safe:
  the external folder holds ciphertext, but the key stays where it is.

## Key handling: unlock on watch

- Default: key loads from `keys.json` at startup, no login needed.
- `LOCKED=1 ./scripts/run.sh` (or no `keys.json` at all): server boots
  **locked**. Open the page, paste the 64-hex-char master key, watch.
  Key lives only in server RAM — Lock button or 30 min idle wipes it
  (`--idle-lock 0` disables auto-lock).
- A pasted key is checked against the library on unlock (typo → `401`).

## Watch without a browser (mpv, no disk cache)

```bash
./scripts/run.sh              # terminal 1: server (locked or open)
./scripts/watch.sh            # terminal 2: menu (unlock → list → play)
./scripts/watch.sh moviename  # one-shot: play and exit
```

Needs `mpv` (`brew install mpv`). Key prompt hides input; `STREAM_KEY_HEX`
env skips the prompt (scripting only). Player locks the server on quit —
unless the server was already open, then it leaves it as found.

## Notes

- If a video buffers instead of starting fast, its `moov` atom is at the
  end. `add-video.sh` auto-fixes with ffmpeg when available, else it warns.
- `videos/` holds only `.cenc`; `keys.json` is git-ignored. Binds localhost only.
