# On-the-fly HLS streaming — feature reference & client integration guide

Status as of 2026-07-19: implemented, compiles, and passes full end-to-end
self-tests (`scripts/smoke-test.sh`, `scripts/hls-selftest.sh`) against a real
build of the app, including a from-scratch Docker deploy. Still **uncommitted**
on this branch and not yet code-reviewed/merged.

This file has two audiences:
- **Part 1** is self-contained and meant to be handed as-is to another agent/dev
  implementing an HLS-capable client (e.g. the desktop app) — it assumes no
  access to this backend repo, only a running server to talk to.
- **Parts 2–4** are backend implementation reference for continuing work on
  this repo.

---

## Part 1: Client integration contract

### What this is

Videos/audio previously played as a single full-file HTTP range-requested
stream (no adaptive quality). This feature adds: on first play, the server
transcodes the file in the background into a multi-bitrate HLS ladder and
serves *that* instead, cached on disk for all subsequent views. Any HLS-capable
player can consume it with no special client logic beyond a two-step URL
resolution.

### Step 1 — check availability and get the playlist URL

```
GET {baseURL}/api/v4/file/hls?uri=<cloudreve-uri, URL-encoded>
Authorization: Bearer <access_token>
```

`uri` is a Cloudreve-native URI, e.g. `cloudreve://my/Videos/movie.mp4`.

Response body:
```json
{"code": 0, "data": {"available": true, "url": "http://host/api/v4/file/hls/J1fY/MvJ7Hq.../master.m3u8"}, "msg": ""}
```

`available: false` (with no `url`) means: HLS is disabled server-side, the
file's extension isn't in the configured eligible list, the file is smaller
than the configured minimum size, or the path isn't a plain file. Treat this
as "not an error" — just fall back (see below).

### Step 2 — play the URL

The `url` from step 1 is a **fully self-authorizing absolute URL** — the
signature is embedded in the path itself (`/file/hls/<entityId>/<signature>/master.m3u8`),
valid for 24 hours from mint time. Hand it directly to any HLS-capable player:

- No `Authorization` header, cookies, or session needed for this URL or
  anything it references.
- The signature is scoped to the **entity**, not the exact sub-path, so every
  relative reference the master playlist makes (variant playlists, `.ts`
  segments, all living under the same URL prefix) inherits auth automatically.
  A player following relative URLs from the playlist just works — no masking,
  header injection, or URL rewriting needed on the client side.
- This is deliberately native-player-friendly: anything that can play a plain
  `.m3u8` URL (hls.js, ExoPlayer, AVFoundation/AVPlayer, VLC, mpv, native
  `<video>` on Safari, etc.) needs zero Cloudreve-specific code beyond step 1.

Reference web implementation: `assets/src/component/Viewers/Video/VideoViewer.tsx`
— calls the availability endpoint, and on success does `art.switchUrl(hls.url)`
(hls.js under the hood) with no further transformation of the URL.

### Playback characteristics to expect

- **First-play latency**: up to ~25s (server-side `DefaultReadyTimeout`) on a
  cold cache, while ffmpeg produces the *first segment of every rendition* in
  the background. Repeat views of the same file are served instantly from an
  on-disk cache (keyed by the file's internal entity ID — re-uploading creates
  a new entity ID, so cache invalidation is automatic and you never need to
  bust anything client-side).
- **Growing playlists**: variant (per-rendition) playlists are HLS "EVENT"
  type — they grow segment-by-segment as encoding progresses and only get a
  trailing `#EXT-X-ENDLIST` once that rendition finishes transcoding entirely.
  Standard HLS players (hls.js, ExoPlayer, AVPlayer) already handle this via
  their normal manifest-refresh/live-reload logic; you don't need custom
  polling. A naive one-shot manifest fetch, however, will only see whatever
  segments existed at that instant.
- **Master playlist shape**: one `#EXT-X-STREAM-INF` line per rendition:
  ```
  #EXT-X-STREAM-INF:BANDWIDTH=<bps>[,RESOLUTION=<w>x<h>],NAME="<name>"
  <name>/playlist.m3u8
  ```
  Video renditions carry `RESOLUTION`; **audio-only source files never do** —
  use presence/absence of `RESOLUTION` to detect "this is audio, don't render
  a video surface" if your player UI needs to branch on that. Rendition names
  look like `1080p`/`720p`/... for video or `320k`/`192k`/... for audio-only.
  Note `NAME=` is not a standard `EXT-X-STREAM-INF` attribute — harmless
  (unknown attributes are ignored per the HLS spec) but don't rely on your
  parser surfacing it; per-rendition quality is really conveyed by
  `BANDWIDTH`/`RESOLUTION`.
- **Segments**: fixed-duration MPEG-TS (`seg_00000.ts`, `seg_00001.ts`, ...),
  default 6s each (admin-configurable). Codec baseline is deliberately
  maximally-compatible: H.264 `main` profile / `yuv420p` + AAC 48kHz stereo —
  no HEVC/AV1/other exotic codecs, so any standard HLS decoder handles it
  without extra codec packs.
- **Headers on every playlist/segment response**: `Cache-Control: no-cache`
  (don't let an intermediary cache a playlist mid-transcode) and the correct
  `Content-Type` (`application/vnd.apple.mpegurl` for `.m3u8`, `video/mp2t`
  for `.ts`).

### Fallback path

If `available` is `false`, or the availability call errors/times out, fall
back to ordinary direct playback:

```
POST {baseURL}/api/v4/file/url
Authorization: Bearer <access_token>
{"uris": ["<cloudreve-uri>"], "download": false}
```
→ `data.urls[0].url` is a signed, single-file, range-request-seekable direct
URL (no adaptive bitrate). This is exactly the try-HLS-then-fall-back sequence
`VideoViewer.tsx` implements — mirror it.

### Error cases

Response envelope on error: `{"code": <non-zero>, "msg": "...", "data": null}`.

- Hitting the stream URL directly when HLS is server-side disabled →
  `code: 404`. Shouldn't occur if you honor `available:false` from step 1 first.
- Invalid/expired/tampered signature → `code: 40020` (credential invalid). Fix
  by re-calling step 1 to mint a fresh URL (don't try to construct/guess these
  URLs yourself — always mint via the API).
- Malformed `uri` param → `code: 40001` (bad parameter).

### Minimal client pseudocode

```
resp = GET /api/v4/file/hls?uri=<uri>  (with auth)
if resp.data.available:
    play(resp.data.url)   # hand straight to an HLS player, no further processing
else:
    resp2 = POST /api/v4/file/url {"uris":[uri],"download":false}  (with auth)
    play(resp2.data.urls[0].url)  # plain progressive/range-seekable playback
```

---

## Part 2: Server-side eligibility rules (admin-configurable settings)

| Setting (env override `CR_SETTING_<key>`) | Default | Meaning |
|---|---|---|
| `hls_enabled` | `false` | Master on/off switch. |
| `hls_exts` | `mp4,mkv,mov,avi,flv,webm,wmv,m4v,ts,m2ts,mpg,mpeg,3gp` | Extensions eligible as "video". |
| `hls_min_size` | `52428800` (50MB) | Minimum size for video-extension files. |
| `hls_audio_exts` | `mp3,m4a,aac,flac,wav,ogg,opus,wma` | Extensions eligible as "audio". |
| `hls_audio_min_size` | `5242880` (5MB) | Minimum size for audio-extension files. |
| `hls_resolutions` | `1080:5000k:160k,720:2800k:128k,480:1400k:128k,360:800k:96k` | Video ladder: `height:vbitrate:abitrate,...`. |
| `hls_audio_bitrates` | `320k,192k,128k,64k` | Audio-only ladder. |
| `hls_segment_duration` | `6` (seconds) | Target segment length. |
| `hls_extra_args` | `""` | Extra raw ffmpeg args, space-split, inserted right after `-y`. |
| `hls_cache_ttl` | `72` (hours) | Idle on-disk cache eviction age. |
| `hls_max_concurrent_jobs` | `1` | Global concurrency cap on simultaneous transcodes. |

A file matching *both* the video and audio extension lists (unusual, but
possible with custom settings) is treated as video for sizing purposes.
Whether a source is actually treated as "has video" vs "audio-only" for
*encoding* is decided by probing the real stream (`ffprobe`), not by
extension — e.g. an `.mp3` with embedded cover art is still audio-only.

Admin UI: Settings → Media, in `assets/src/component/Admin/Settings/Media/Media.tsx`.
Only English admin-UI translation strings were added; the other 13 locale
files were not touched.

---

## Part 3: Backend architecture (this repo)

- `pkg/hls/` — `ladder.go` (parses ladder settings into `Rendition`s),
  `manager.go` (runs ffmpeg per rendition in the background, disk-caches
  under `data/hls_cache/<entityID>/`, tracks job status via channels —
  `Job.WaitReady` blocks briefly for the *first* segment of every rendition,
  not full completion), `probe.go` (ffprobe-based has-video-stream check).
- No new DB/ent schema. Cache identity is the entity ID alone — a new upload
  gets a new entity ID, so cache invalidation is automatic. Completion is
  tracked via a `.complete` marker file on disk; `discoverRenditions` rebuilds
  in-memory job state from disk on server restart for already-finished jobs.
- Settings: interface + impl in `pkg/setting/provider.go` (see table above).
  Reuses the existing ffmpeg path setting (`thumb_ffmpeg_path` / `FFMpegPath()`)
  rather than adding a duplicate.
- `dep.HLSManager(ctx)` singleton in `application/dependency/dependency.go`,
  same pattern as `MimeDetector`/`MediaMetaExtractor`.
- `service/explorer/hls.go`: `FileHLSService.Get` (session-authenticated,
  checks eligibility by ext/size, mints the signed URL — see
  `hlsSignContent`/`dep.GeneralAuth()`) and `HLSStreamService.Serve` (no
  session, authorized purely by the path-embedded signature).
- Routes in `routers/router.go`: `GET /file/hls` (mint URL) and
  `GET /file/hls/:id/:sign/*path` (stream). Deliberately **not** nested under
  the existing `content` group, to avoid a static-vs-param gin route
  registration conflict with `content/:id/:speed/:name`. URL builder:
  `pkg/cluster/routes/routes.go:MasterFileHLSUrl`.
- Frontend: `assets/src/component/Viewers/Video/VideoViewer.tsx` asks
  `/file/hls` for a URL before falling back to direct playback; on success it
  hands hls.js the absolute URL directly (`Artplayer.tsx`'s masking logic
  already no-ops for absolute, non-masked URLs — no changes needed there).
  `hls.js` was already a dependency; only the server-side producer was new.

### Known limitations / deliberate v1 tradeoffs

- No persisted/resumable job queue — if the server restarts mid-transcode,
  the partial cache dir is discarded on next request and transcoding restarts
  cleanly (simplicity over the existing `pkg/queue` DB-backed task system).
- Only English admin-UI translations added.
- No automated Go test suite — coverage is the shell self-tests below, which
  exercise the real HTTP API against a real ffmpeg-backed server.
- Every configured ladder rung is always encoded and listed, even ones taller
  than the source. The ffmpeg filter (`scale=-2:min(ih,%d)`) correctly avoids
  *upscaling* the actual pixels, but the master playlist's advertised
  `BANDWIDTH`/`RESOLUTION` for that rung still reflects the configured target,
  not the real (smaller) encoded output — a client reading attributes off the
  master playlist without inspecting the actual stream could show a
  misleadingly high resolution for a low-res source. Not filtered out
  server-side; a future improvement could skip rungs taller than the probed
  source height in `resolveRenditions` (`pkg/hls/manager.go`).

---

## Part 4: How to run and test this yourself

No local Go/Node toolchain needed — everything builds inside Docker.

```
scripts/dev.sh up            # build + start (docker-compose.dev.yml, SQLite, no separate DB container)
scripts/smoke-test.sh        # full fresh-deploy test: wipes data volume, rebuilds, runs
                              # health/auth/filesystem/upload-download/HLS checks end to end
scripts/hls-selftest.sh full # HLS-only: synthesizes test audio+video via ffmpeg in-container,
                              # uploads, verifies master playlist + rendition finalization + a segment, cleans up
```

See `scripts/lib/cloudreve-api.sh` for the reusable API helpers (login,
upload, generic authenticated request) these scripts are built on — extend it
rather than hand-rolling `curl` when adding new checks. Full usage docs are in
each script's header comment.
