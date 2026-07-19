# Project status / session handoff

Read this first when resuming work on this repo — it's the "where we left
off" doc. Update it whenever a session ends with meaningful progress, so the
next session (human or agent) doesn't have to reconstruct context from git
log and guesswork.

## Current state (as of 2026-07-19)

**HLS on-the-fly adaptive streaming** — implemented, tested, committed, and
deployed to the user's home server as a private GHCR image.

- Full feature contract, architecture, and settings reference:
  [`docs/HLS_FEATURE_NOTES.md`](./HLS_FEATURE_NOTES.md) — read that file for
  anything HLS-specific rather than duplicating it here.
- Backend: `pkg/hls/`, `service/explorer/hls.go`, settings in
  `pkg/setting/provider.go`, routes in `routers/router.go`.
- Frontend: `assets/src/component/Viewers/Video/VideoViewer.tsx` +
  `MusicPlayer.tsx` try HLS first, fall back to direct playback. Admin UI in
  `assets/src/component/Admin/Settings/Media/Media.tsx`.
- Test tooling: `scripts/` (see below) — reusable API helpers + a full
  fresh-deploy smoke test, not ad hoc `curl`.
- Production image: `docker/Dockerfile.prod`, built and pushed to
  `ghcr.io/theboss9345/cloudreve-hls:latest` (**private** — home server needs
  a `read:packages`-scoped PAT to pull it; see Part 5 of
  `HLS_FEATURE_NOTES.md`).

### Committed history on this branch (local `master`, ahead of upstream `origin`)

1. `feat(hls): add on-the-fly adaptive-bitrate HLS transcoding for video/audio` — backend
2. `docs: add AGENTS.md codebase conventions guide`
3. `chore(dev): add local Docker dev environment and API test scripts for HLS`
4. (frontend changes are a separate commit in the `assets` submodule's own history)

### Remotes (important — do not push to `origin`)

`origin` on both this repo and the `assets` submodule points at the
**official upstream** projects (`cloudreve/cloudreve`, `cloudreve/frontend`),
which the user does not own. All of this work is pushed to the user's own
GitHub account (`THEBOSS9345`) instead:

- Main repo → `myfork` remote → `https://github.com/THEBOSS9345/cloudreve` (a real GitHub fork)
- `assets` submodule → `myfork` remote → `https://github.com/THEBOSS9345/cloudreve-frontend` (a **plain new repo, not a fork** — user explicitly wanted a copy here, not a fork)

### Known outstanding items

- Deployed to the home server (`root@100.68.206.122` via Tailscale SSH,
  container name `cloudreve`, bind-mounted `/root/cloudreve/data`). Two
  stopped backup containers are preserved there in case of rollback:
  `cloudreve-official-backup` (original `cloudreve/cloudreve:latest`) and
  `cloudreve-hls-v1-backup` (first custom build, before the defaults fix
  below). Rollback: `docker stop cloudreve && docker rm cloudreve && docker
  rename cloudreve-official-backup cloudreve && docker start cloudreve`.
- **Fixed 2026-07-19**: `hls_*` settings were missing from
  `inventory.DefaultSettings`, so the admin Settings UI showed blank values
  instead of defaults on install/upgrade (the feature worked functionally
  regardless, since `pkg/setting/provider.go`'s individual getters carry
  their own hardcoded fallback — this only affected the admin-UI-visible
  default). Fixed in commit `fix(hls): register hls_* settings with defaults
  in inventory.DefaultSettings`, rebuilt, and redeployed.
- User also reported the admin HLS section showing raw i18n keys
  (`settings.hls`, `settings.hlsDes`) instead of translated labels. The
  translation keys and `useTranslation("dashboard")` namespace in
  `Media.tsx` were verified correct in source — likely a **stale PWA service
  worker cache** in the browser (Cloudreve uses `vite-plugin-pwa`), since the
  backend image was swapped in place at the same URL. Ask the user to hard
  refresh / clear the service worker (DevTools → Application → Service
  Workers → Unregister, and Clear storage) and confirm whether that resolves
  it before assuming it's a source bug.
- HLS master playlist can advertise a `RESOLUTION`/`BANDWIDTH` higher than
  the real encoded output for ladder rungs taller than the source (see
  `HLS_FEATURE_NOTES.md` Part 3, "Known limitations").
- No automated Go test suite for the HLS feature — coverage is the shell
  self-tests only.
- A separate Claude agent may be working on adding HLS playback to the
  **desktop app** using the client-integration contract in
  `docs/HLS_FEATURE_NOTES.md` Part 1. Check in on that separately — it's a
  different codebase, not tracked in this repo.

## Repo layout notes (for anyone reorienting)

- `docker/` — local dev + production Docker assets added for this work
  (`Dockerfile.dev`, `Dockerfile.prod`, `docker-compose.dev.yml`). The
  original upstream `Dockerfile` and `docker-compose.yml` stay at the repo
  root untouched — those are the official release artifacts.
- `docs/` — this file and `HLS_FEATURE_NOTES.md`.
- `scripts/` — reusable local test tooling, not upstream:
  - `dev.sh` — docker lifecycle (`up|rebuild|restart|down|reset|status|logs|exec|wait-ready`)
  - `lib/cloudreve-api.sh` — shared API helpers (login, upload, generic authenticated request) other scripts source
  - `api.sh` — generic authenticated CLI for ad hoc endpoint calls
  - `hls-selftest.sh` — HLS-specific end-to-end test
  - `smoke-test.sh` — full fresh-deploy test suite (the "does everything still work" command)
- `AGENTS.md` — codebase conventions for coding agents working in this repo.

## Working conventions for this user (see Claude memory for full detail)

- Prefer `scripts/` over ad hoc `curl` when testing anything against the
  running app.
- No `Co-Authored-By` trailer on commits — attribute solely to the user.
- Never push to `origin` (upstream) — always check `git remote -v` first and
  use the user's own fork/copy via `gh`.
- These and more are tracked in this Claude Code session's persistent memory
  (not part of this repo) — this file is the repo-local complement for
  anyone/anything without access to that.
