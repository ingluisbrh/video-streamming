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

## Notes

- If a video buffers instead of starting fast, its `moov` atom is at the
  end. `add-video.sh` auto-fixes with ffmpeg when available, else it warns.
- `videos/` holds only `.cenc`; `keys.json` is git-ignored. Binds localhost only.
