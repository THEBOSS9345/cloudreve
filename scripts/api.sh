#!/usr/bin/env bash
# api.sh - generic authenticated CLI for one-off calls against the local
# Cloudreve API, for cases a dedicated feature test script doesn't exist yet.
# Handles login/token caching so ad-hoc exploration during development doesn't
# need hand-written curl + Authorization headers.
#
# Usage:
#   scripts/api.sh login                          # register/login, cache token
#   scripts/api.sh get  /me
#   scripts/api.sh post /file/create '{"uri":"cloudreve://my/newfolder","type":"folder"}'
#   scripts/api.sh put  /file/upload '{"uri":"cloudreve://my/x.txt","size":3,"mime_type":"text/plain"}'
#   scripts/api.sh delete /file '{"uris":["cloudreve://my/x.txt"]}'
#   scripts/api.sh raw GET /file/hls?uri=cloudreve%3A%2F%2Fmy%2Fx.mp4   # skip the /api/v4 prefix assumption if needed
#
# Pretty-prints JSON responses when node is available. Env overrides same as
# lib/cloudreve-api.sh: BASE_URL, TEST_EMAIL, TEST_PASSWORD.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/cloudreve-api.sh
source "$SCRIPT_DIR/lib/cloudreve-api.sh"

pretty() {
  node -e "
    let s='';
    process.stdin.on('data', d => s += d);
    process.stdin.on('end', () => {
      try { console.log(JSON.stringify(JSON.parse(s), null, 2)); }
      catch { process.stdout.write(s); }
    });
  " 2>/dev/null || cat
}

cmd="${1:-}"
shift || true
case "$cmd" in
  login) login "$@" ;;
  register) register "$@" ;;
  get) api GET "$1" | pretty ;;
  post) api POST "$1" "${2:-}" | pretty ;;
  put) api PUT "$1" "${2:-}" | pretty ;;
  delete) api DELETE "$1" "${2:-}" | pretty ;;
  patch) api PATCH "$1" "${2:-}" | pretty ;;
  raw) curl -s -X "$1" "$BASE_URL$2" -H "Authorization: Bearer $(token)" ${3:+-H "Content-Type: application/json" -d "$3"} | pretty ;;
  *) echo "usage: $0 {login|register|get|post|put|delete|patch <path> [json-body]} | {raw <METHOD> <path> [json-body]}" >&2; exit 1 ;;
esac
