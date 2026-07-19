#!/usr/bin/env bash
# dev.sh - lifecycle management for the local docker-compose.dev.yml Cloudreve
# instance. Wraps the docker/compose invocations that would otherwise need to
# be typed by hand while iterating on a feature, so the whole test loop
# (rebuild -> restart -> smoke test -> read logs) is a couple of one-line
# commands.
#
# Usage:
#   scripts/dev.sh up          # start (build only if image missing), detached
#   scripts/dev.sh rebuild     # rebuild image from current source + restart
#   scripts/dev.sh restart     # restart container without rebuilding
#   scripts/dev.sh down        # stop and remove the container
#   scripts/dev.sh reset       # down -v: also wipes the data volume (fresh
#                               # install wizard, empty DB) - use when you need
#                               # a truly clean slate, e.g. after schema changes
#   scripts/dev.sh status      # container state + health
#   scripts/dev.sh logs [-f] [tail]   # recent logs, optionally follow
#   scripts/dev.sh exec <cmd...>      # run a command inside the container
#   scripts/dev.sh wait-ready [timeout-seconds]  # block until HTTP 200 on /
#
# Env overrides: BASE_URL (default http://localhost:5212), CONTAINER (default
# cloudreve-dev), COMPOSE_FILE (default docker-compose.dev.yml).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-$ROOT_DIR/docker-compose.dev.yml}"
CONTAINER="${CONTAINER:-cloudreve-dev}"
BASE_URL="${BASE_URL:-http://localhost:5212}"

log() { echo "[dev] $*" >&2; }
die() { echo "[dev] FAIL: $*" >&2; exit 1; }

compose() { docker compose -f "$COMPOSE_FILE" "$@"; }

cmd_up() {
  log "starting (build only if no image cached yet)"
  compose up -d --build
  wait_ready 90
}

cmd_rebuild() {
  log "rebuilding image from current source and restarting"
  compose up -d --build --force-recreate
  wait_ready 90
}

cmd_restart() {
  compose restart
  wait_ready 60
}

cmd_down() {
  compose down
}

cmd_reset() {
  log "wiping data volume - next start will hit the setup wizard again"
  compose down -v
}

cmd_status() {
  docker ps -a --filter "name=$CONTAINER" --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
}

cmd_logs() {
  local follow="" tail="200"
  for a in "$@"; do
    case "$a" in
      -f) follow="-f" ;;
      *) tail="$a" ;;
    esac
  done
  docker logs $follow --tail "$tail" "$CONTAINER"
}

cmd_exec() {
  [ "$#" -gt 0 ] || die "usage: dev.sh exec <cmd...>"
  docker exec "$CONTAINER" "$@"
}

wait_ready() {
  local timeout="${1:-60}" waited=0
  log "waiting for $BASE_URL to answer (timeout ${timeout}s)"
  until curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/" 2>/dev/null | grep -q '^200$'; do
    sleep 2
    waited=$((waited + 2))
    if [ "$waited" -ge "$timeout" ]; then
      log "not ready after ${timeout}s - showing last 40 log lines:"
      docker logs --tail 40 "$CONTAINER" || true
      die "$BASE_URL never became ready"
    fi
  done
  log "ready ($BASE_URL, waited ${waited}s)"
}

cmd="${1:-status}"
shift || true
case "$cmd" in
  up) cmd_up ;;
  rebuild) cmd_rebuild ;;
  restart) cmd_restart ;;
  down) cmd_down ;;
  reset) cmd_reset ;;
  status) cmd_status ;;
  logs) cmd_logs "$@" ;;
  exec) cmd_exec "$@" ;;
  wait-ready) wait_ready "${1:-60}" ;;
  *) die "unknown command: $cmd (expected: up|rebuild|restart|down|reset|status|logs|exec|wait-ready)" ;;
esac
