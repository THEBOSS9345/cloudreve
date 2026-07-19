#!/usr/bin/env bash
# smoke-test.sh - full deploy + end-to-end smoke test. Rebuilds the Cloudreve
# dev image from current source (including uncommitted changes), (re)starts
# the container, and drives a battery of checks over the real HTTP API to
# confirm the whole app actually works - not just that it compiles. Meant to
# be the one command to run before/after any change to answer "did I break
# anything", instead of hand-rolled curl or clicking through the UI.
#
# Every section runs independently and is reported pass/fail - one failing
# section does not stop the others from running, so a single invocation gives
# a full picture instead of stopping at the first problem.
#
# Usage:
#   scripts/smoke-test.sh                # fresh deploy: wipe data volume, rebuild, full suite
#   scripts/smoke-test.sh --no-reset     # rebuild but keep existing data volume (faster iteration)
#   scripts/smoke-test.sh --no-build     # just restart the existing image, then run the suite
#   scripts/smoke-test.sh --skip-hls     # skip the (slower) HLS transcode section
#
# Env overrides: BASE_URL, CONTAINER, TEST_EMAIL, TEST_PASSWORD (see lib/cloudreve-api.sh).

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/cloudreve-api.sh
source "$SCRIPT_DIR/lib/cloudreve-api.sh"

RESET=1
BUILD=1
RUN_HLS=1
for a in "$@"; do
  case "$a" in
    --no-reset) RESET=0 ;;
    --no-build) BUILD=0 ;;
    --skip-hls) RUN_HLS=0 ;;
    *) die "unknown flag: $a (expected: --no-reset | --no-build | --skip-hls)" ;;
  esac
done

RESULTS=()
record() { RESULTS+=("$1|$2"); } # record <name> <PASS|FAIL>

run_section() { # run_section <name> <fn>
  local name="$1" fn="$2"
  log "=== $name ==="
  if ( set -e; "$fn" ); then
    record "$name" PASS
  else
    record "$name" FAIL
    log "!!! $name FAILED (continuing with remaining sections)"
  fi
}

# --- deploy ---

deploy() {
  if [ "$RESET" = "1" ]; then
    "$SCRIPT_DIR/dev.sh" reset
  fi
  if [ "$BUILD" = "1" ]; then
    "$SCRIPT_DIR/dev.sh" up
  else
    "$SCRIPT_DIR/dev.sh" restart 2>/dev/null || "$SCRIPT_DIR/dev.sh" up
  fi
}

# --- test sections ---

sec_health() {
  local resp
  resp=$(curl -sf "$BASE_URL/api/v4/site/ping") || die "ping failed"
  echo "$resp" | grep -q '"code":0' || die "ping returned non-zero code: $resp"
  log "backend version: $(echo "$resp" | json_get data)"
}

sec_auth() {
  rm -f "$TOKEN_FILE"
  login
  api_ok GET /user/me >/dev/null
  log "session confirmed via /user/me"
}

sec_filesystem_crud() {
  local base="cloudreve://my/smoketest_$$"
  create_path "$base" folder >/dev/null
  create_path "$base/sub" folder >/dev/null
  create_path "$base/sub/empty.txt" file >/dev/null

  local listing
  listing=$(list_dir "$base")
  echo "$listing" | node -e "
    let s=''; process.stdin.on('data',d=>s+=d); process.stdin.on('end',()=>{
      const files = JSON.parse(s).data.files.map(f=>f.name);
      if (!files.includes('sub')) { console.error('expected sub in ' + files); process.exit(1); }
    });" || die "listing $base did not contain 'sub'"
  log "create + list OK"

  rename_path "$base/sub" "renamed" >/dev/null
  listing=$(list_dir "$base")
  echo "$listing" | node -e "
    let s=''; process.stdin.on('data',d=>s+=d); process.stdin.on('end',()=>{
      const files = JSON.parse(s).data.files.map(f=>f.name);
      if (!files.includes('renamed')) { console.error('expected renamed in ' + files); process.exit(1); }
    });" || die "rename did not take effect"
  log "rename OK"

  move_paths "$base/renamed/empty.txt" "$base" false >/dev/null
  listing=$(list_dir "$base")
  echo "$listing" | node -e "
    let s=''; process.stdin.on('data',d=>s+=d); process.stdin.on('end',()=>{
      const files = JSON.parse(s).data.files.map(f=>f.name);
      if (!files.includes('empty.txt')) { console.error('expected empty.txt moved up, got ' + files); process.exit(1); }
    });" || die "move did not take effect"
  log "move OK"

  delete_uris "$base"
  log "cleanup OK"
}

sec_upload_download_roundtrip() {
  local dest="$STATE_DIR/roundtrip_src.txt" got="$STATE_DIR/roundtrip_got.txt" uri="cloudreve://my/roundtrip_$$.txt"
  node -e "console.log('smoke-test payload ' + Date.now())" > "$dest"
  upload "$dest" "$uri" text/plain
  local url
  url=$(file_url "$uri")
  [ -n "$url" ] && [ "$url" != "null" ] || die "file_url returned empty for $uri"
  curl -sf -o "$got" "$url" || die "download from $url failed"
  diff -q "$dest" "$got" >/dev/null || die "downloaded content does not match uploaded content"
  log "upload -> file/url -> download round-trip byte-identical"
  delete_uris "$uri"
  rm -f "$dest" "$got"
}

sec_hls() {
  "$SCRIPT_DIR/hls-selftest.sh" full
}

# --- run ---

deploy || { log "deploy failed, aborting"; exit 1; }

run_section "health"                    sec_health
run_section "auth"                      sec_auth
run_section "filesystem CRUD"           sec_filesystem_crud
run_section "upload/download roundtrip" sec_upload_download_roundtrip
if [ "$RUN_HLS" = "1" ]; then
  run_section "HLS streaming" sec_hls
fi

echo >&2
log "=== summary ==="
fail=0
for r in "${RESULTS[@]}"; do
  name="${r%%|*}"; status="${r##*|}"
  log "$status  $name"
  [ "$status" = "FAIL" ] && fail=1
done

if [ "$fail" = "1" ]; then
  log "SMOKE TEST FAILED"
  exit 1
fi
log "SMOKE TEST PASSED"
