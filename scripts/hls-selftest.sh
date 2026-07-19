#!/usr/bin/env bash
# hls-selftest.sh - reusable, non-interactive self-test harness for the HLS
# on-the-fly transcoding feature against the local docker-compose.dev.yml
# environment. Drives the whole flow over the HTTP API (via lib/cloudreve-api.sh)
# so it can be re-run after any backend/frontend change to confirm HLS still
# actually works end to end, not just that it compiles.
#
# Usage:
#   scripts/hls-selftest.sh full          # login, gen media, upload,
#                                          # verify HLS for both, clean up
#   scripts/hls-selftest.sh login         # register (if needed) + login, caches token
#   scripts/hls-selftest.sh gen-media     # synthesize test_audio.mp3 + test_video.mp4
#                                          # via ffmpeg inside the running container
#   scripts/hls-selftest.sh upload <local-path> <cloudreve-uri>
#   scripts/hls-selftest.sh check <cloudreve-uri>   # GET /file/hls availability
#   scripts/hls-selftest.sh verify <cloudreve-uri>  # check + fetch master/rendition/segment
#   scripts/hls-selftest.sh clean         # delete the test files this script uploaded
#
# Env overrides: BASE_URL, CONTAINER, TEST_EMAIL, TEST_PASSWORD (see lib/cloudreve-api.sh).
# Requires: curl, node, docker (gen-media only).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/cloudreve-api.sh
source "$SCRIPT_DIR/lib/cloudreve-api.sh"

check() { # check <cloudreve-uri>  -> prints the /file/hls availability JSON, returns nonzero if unavailable
  local uri="$1" encoded resp available
  encoded=$(node -e "console.log(encodeURIComponent(process.argv[1]))" "$uri")
  resp=$(curl -s "$BASE_URL/api/v4/file/hls?uri=$encoded" -H "Authorization: Bearer $(token)")
  echo "$resp"
  available=$(echo "$resp" | json_get data.available 2>/dev/null || echo "false")
  [ "$available" = "true" ]
}

verify() { # verify <cloudreve-uri> - full check: availability, master playlist, one rendition, one segment
  local uri="$1" resp url base playlist_body first_rendition_dir seg_http
  resp=$(curl -s "$BASE_URL/api/v4/file/hls?uri=$(node -e "console.log(encodeURIComponent(process.argv[1]))" "$uri")" \
    -H "Authorization: Bearer $(token)")
  [ "$(echo "$resp" | json_get data.available 2>/dev/null || echo false)" = "true" ] \
    || die "$uri: not available for HLS - $resp"
  url=$(echo "$resp" | json_get data.url)
  log "$uri: available, url=$url"

  playlist_body=$(curl -s -w '\n%{http_code}' "$url")
  local playlist_code=$(echo "$playlist_body" | tail -1)
  playlist_body=$(echo "$playlist_body" | sed '$d')
  [ "$playlist_code" = "200" ] || die "$uri: master.m3u8 returned HTTP $playlist_code"
  echo "$playlist_body" | grep -q '^#EXTM3U' || die "$uri: master.m3u8 doesn't look like a playlist:\n$playlist_body"
  log "$uri: master.m3u8 OK ($(echo "$playlist_body" | grep -c '#EXT-X-STREAM-INF') renditions)"
  if echo "$playlist_body" | grep -q 'RESOLUTION='; then
    log "$uri: classified as VIDEO (has RESOLUTION attr)"
  else
    log "$uri: classified as AUDIO-ONLY (no RESOLUTION attr)"
  fi

  base="${url%/master.m3u8}"
  first_rendition_dir=$(echo "$playlist_body" | grep -v '^#' | grep -v '^$' | head -1 | sed 's#/playlist.m3u8##')
  [ -n "$first_rendition_dir" ] || die "$uri: could not find a rendition in master playlist"

  # `available=true` only guarantees the first segment of every rendition is
  # ready (Job.WaitReady), not that encoding has finished - poll for
  # #EXT-X-ENDLIST instead of asserting it immediately, or this flakes
  # depending on how far transcoding has gotten by the time we ask.
  local rendition_body waited=0 timeout=60
  while true; do
    rendition_body=$(curl -s "$base/$first_rendition_dir/playlist.m3u8")
    echo "$rendition_body" | grep -q '#EXT-X-ENDLIST' && break
    waited=$((waited + 2))
    [ "$waited" -lt "$timeout" ] || die "$uri: rendition $first_rendition_dir playlist missing #EXT-X-ENDLIST after ${timeout}s (not finalized)"
    sleep 2
  done
  log "$uri: rendition $first_rendition_dir playlist OK, finalized (waited ${waited}s)"

  local first_seg
  first_seg=$(echo "$rendition_body" | grep '\.ts$' | head -1)
  [ -n "$first_seg" ] || die "$uri: no .ts segment listed in rendition playlist"

  seg_http=$(curl -s -o "$STATE_DIR/_lastseg.ts" -w '%{http_code}' "$base/$first_rendition_dir/$first_seg")
  [ "$seg_http" = "200" ] || die "$uri: segment $first_seg returned HTTP $seg_http"
  local magic
  magic=$(od -An -tx1 -N1 "$STATE_DIR/_lastseg.ts" | tr -d ' ')
  [ "$magic" = "47" ] || die "$uri: segment $first_seg doesn't start with MPEG-TS sync byte 0x47 (got 0x$magic)"
  log "$uri: segment $first_seg OK ($(stat -c%s "$STATE_DIR/_lastseg.ts" 2>/dev/null || stat -f%z "$STATE_DIR/_lastseg.ts") bytes, valid TS)"
  rm -f "$STATE_DIR/_lastseg.ts"

  log "$uri: PASS"
}

clean() {
  delete_uris cloudreve://my/test_audio.mp3 cloudreve://my/test_video.mp4
  log "cleaned up test files"
}

full() {
  require_container
  login
  gen_media
  upload "$STATE_DIR/test_audio.mp3" cloudreve://my/test_audio.mp3
  upload "$STATE_DIR/test_video.mp4" cloudreve://my/test_video.mp4
  verify cloudreve://my/test_audio.mp3
  verify cloudreve://my/test_video.mp4
  clean
  log "ALL PASS"
}

cmd="${1:-full}"
shift || true
case "$cmd" in
  login) login ;;
  register) register ;;
  gen-media) gen_media ;;
  upload) upload "$@" ;;
  check) check "$@" ;;
  verify) verify "$@" ;;
  clean) clean ;;
  full) full ;;
  *) die "unknown command: $cmd (expected: login|register|gen-media|upload|check|verify|clean|full)" ;;
esac
