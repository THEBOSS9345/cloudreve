# cloudreve-api.sh - shared helpers for talking to a running local Cloudreve
# instance (docker-compose.dev.yml) over its HTTP API. Meant to be `source`d
# by feature test scripts (see hls-selftest.sh) so every script gets the same
# login/upload/request plumbing instead of re-implementing curl+auth by hand.
#
# Not meant to be executed directly.
#
# Env overrides: BASE_URL (default http://localhost:5212), CONTAINER (default
# cloudreve-dev), TEST_EMAIL, TEST_PASSWORD.
#
# Requires: curl, node (JSON parsing, no jq dependency), docker (only for
# functions that reach into the container: gen_media, container_logs, etc).

BASE_URL="${BASE_URL:-http://localhost:5212}"
CONTAINER="${CONTAINER:-cloudreve-dev}"
TEST_EMAIL="${TEST_EMAIL:-testuser@example.com}"
TEST_PASSWORD="${TEST_PASSWORD:-TestPass123}"

LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STATE_DIR="${STATE_DIR:-$LIB_DIR/../.selftest}"
TOKEN_FILE="$STATE_DIR/token.txt"
mkdir -p "$STATE_DIR"

log() { echo "[cloudreve-api] $*" >&2; }
die() { echo "[cloudreve-api] FAIL: $*" >&2; exit 1; }

json_get() { # json_get <field.path> <<<"$json"  (dotted path, arrays not indexable)
  node -e "
    let s='';
    process.stdin.on('data', d => s += d);
    process.stdin.on('end', () => {
      const v = JSON.parse(s);
      const path = process.argv[1].split('.');
      let cur = v;
      for (const p of path) { if (cur == null) break; cur = cur[p]; }
      if (cur === undefined) { process.exit(1); }
      console.log(typeof cur === 'string' ? cur : JSON.stringify(cur));
    });
  " "$1"
}

token() {
  [ -f "$TOKEN_FILE" ] || die "not logged in - call login first"
  cat "$TOKEN_FILE"
}

register() { # register [email] [password]
  local email="${1:-$TEST_EMAIL}" password="${2:-$TEST_PASSWORD}"
  log "registering $email (ignore 'already exists' errors)"
  curl -s -X POST "$BASE_URL/api/v4/user" \
    -H "Content-Type: application/json" \
    -d "{\"email\":\"$email\",\"password\":\"$password\",\"language\":\"en-US\"}" >&2 || true
}

login() { # login [email] [password] - caches bearer token to $TOKEN_FILE
  local email="${1:-$TEST_EMAIL}" password="${2:-$TEST_PASSWORD}"
  local resp code access_token
  resp=$(curl -s -X POST "$BASE_URL/api/v4/session/token" \
    -H "Content-Type: application/json" \
    -d "{\"email\":\"$email\",\"password\":\"$password\"}")
  code=$(echo "$resp" | json_get code 2>/dev/null || echo "err")
  if [ "$code" != "0" ]; then
    register "$email" "$password"
    resp=$(curl -s -X POST "$BASE_URL/api/v4/session/token" \
      -H "Content-Type: application/json" \
      -d "{\"email\":\"$email\",\"password\":\"$password\"}")
    code=$(echo "$resp" | json_get code 2>/dev/null || echo "err")
    [ "$code" = "0" ] || die "login failed even after register: $resp"
  fi
  access_token=$(echo "$resp" | json_get data.token.access_token)
  echo "$access_token" > "$TOKEN_FILE"
  log "logged in as $email"
}

api() { # api <method> <path> [json-body]  -> prints response body to stdout
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -s -X "$method" "$BASE_URL/api/v4$path" \
      -H "Authorization: Bearer $(token)" -H "Content-Type: application/json" -d "$body"
  else
    curl -s -X "$method" "$BASE_URL/api/v4$path" \
      -H "Authorization: Bearer $(token)"
  fi
}

api_ok() { # api_ok <method> <path> [json-body] - like api() but dies unless code==0, prints response
  local resp
  resp=$(api "$@")
  [ "$(echo "$resp" | json_get code 2>/dev/null || echo err)" = "0" ] || die "$1 $2 failed: $resp"
  echo "$resp"
}

upload() { # upload <local-path> <cloudreve-uri e.g. cloudreve://my/foo.mp4> [mime]
  local path="$1" uri="$2" mime="${3:-}" size session_resp session_id
  [ -f "$path" ] || die "no such file: $path"
  size=$(stat -c%s "$path" 2>/dev/null || stat -f%z "$path")
  if [ -z "$mime" ]; then
    case "$path" in
      *.mp3) mime="audio/mpeg" ;;
      *.mp4) mime="video/mp4" ;;
      *.jpg|*.jpeg) mime="image/jpeg" ;;
      *.png) mime="image/png" ;;
      *.txt) mime="text/plain" ;;
      *) mime="application/octet-stream" ;;
    esac
  fi

  local session_resp
  session_resp=$(api PUT /file/upload "{\"uri\":\"$uri\",\"size\":$size,\"mime_type\":\"$mime\"}")
  [ "$(echo "$session_resp" | json_get code 2>/dev/null || echo err)" = "0" ] \
    || die "create upload session failed: $session_resp"
  session_id=$(echo "$session_resp" | json_get data.session_id)

  local upload_resp
  upload_resp=$(curl -s -X POST "$BASE_URL/api/v4/file/upload/$session_id/0" \
    -H "Authorization: Bearer $(token)" -H "Content-Type: application/octet-stream" \
    --data-binary @"$path")
  [ "$(echo "$upload_resp" | json_get code 2>/dev/null || echo err)" = "0" ] || die "upload failed: $upload_resp"
  log "uploaded $path -> $uri ($size bytes)"
}

download() { # download <cloudreve-uri> <local-dest-path> - requires the user's group to allow direct links (SourceBatchSize > 0)
  local uri="$1" dest="$2" resp url http
  resp=$(api PUT /file/source "{\"uris\":[\"$uri\"]}")
  [ "$(echo "$resp" | json_get code 2>/dev/null || echo err)" = "0" ] || die "download source lookup failed: $resp"
  url=$(echo "$resp" | json_get "data.0.url")
  http=$(curl -s -L -o "$dest" -w '%{http_code}' "$url")
  [ "$http" = "200" ] || die "download $uri returned HTTP $http"
  log "downloaded $uri -> $dest"
}

delete_uris() { # delete_uris <uri> [uri...]
  local uris_json
  uris_json=$(node -e "console.log(JSON.stringify(process.argv.slice(1)))" "$@")
  api DELETE /file "{\"uris\":$uris_json}" >&2 || true
}

create_path() { # create_path <cloudreve-uri> <file|folder>
  api_ok POST /file/create "{\"uri\":\"$1\",\"type\":\"$2\"}"
}

list_dir() { # list_dir <cloudreve-uri>
  local encoded
  encoded=$(node -e "console.log(encodeURIComponent(process.argv[1]))" "$1")
  api_ok GET "/file?uri=$encoded"
}

rename_path() { # rename_path <cloudreve-uri> <new-name>
  api_ok POST /file/rename "{\"uri\":\"$1\",\"new_name\":\"$2\"}"
}

move_paths() { # move_paths <src-uri> <dst-uri> [copy: true|false]
  local uris_json="[\"$1\"]" dst="$2" copy="${3:-false}"
  api_ok POST /file/move "{\"uris\":$uris_json,\"dst\":\"$dst\",\"copy\":$copy}"
}

file_url() { # file_url <cloudreve-uri> - prints the download URL
  local resp
  resp=$(api_ok POST /file/url "{\"uris\":[\"$1\"],\"download\":true}")
  echo "$resp" | json_get "data.urls.0.url"
}

# --- container-level helpers (docker exec into the dev container) ---

container_running() {
  [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null)" = "true" ]
}

require_container() {
  container_running || die "container '$CONTAINER' is not running - start it with: scripts/dev.sh up"
}

container_logs() { # container_logs [tail-lines]
  docker logs --tail "${1:-100}" "$CONTAINER"
}

gen_media() { # synthesize small test media files inside the container via ffmpeg, docker cp them out
  require_container
  log "synthesizing test_audio.mp3 (180s sine) + test_video.mp4 (40s testsrc, ~7MB) inside $CONTAINER"
  docker exec "$CONTAINER" sh -c "
    ffmpeg -y -f lavfi -i 'sine=frequency=440:duration=180' -ac 2 -b:a 192k /tmp/test_audio.mp3 >/dev/null 2>&1
    ffmpeg -y -f lavfi -i 'testsrc=size=1280x720:rate=30:duration=40' -f lavfi -i 'sine=frequency=220:duration=40' \
      -c:v libx264 -preset ultrafast -b:v 2000k -c:a aac -shortest /tmp/test_video.mp4 >/dev/null 2>&1
  " || die "ffmpeg synthesis failed"
  docker cp "$CONTAINER:/tmp/test_audio.mp3" "$STATE_DIR/test_audio.mp3"
  docker cp "$CONTAINER:/tmp/test_video.mp4" "$STATE_DIR/test_video.mp4"
  log "generated: $STATE_DIR/test_audio.mp3 $(du -h "$STATE_DIR/test_audio.mp3" | cut -f1), $STATE_DIR/test_video.mp4 $(du -h "$STATE_DIR/test_video.mp4" | cut -f1)"
}
