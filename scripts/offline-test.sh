#!/bin/bash
#
# offline-test.sh — briefly deregister cellular to exercise the
# connectivity classifier's online→offline debounce and GPS mode
# fallback to standalone.
#
# Ships a self-firing safety watchdog: even if this script dies,
# the watchdog re-registers cellular after SAFETY_TIMEOUT seconds
# and restarts librescoot-modem. Intended to be runnable from an
# SSH session without risk of stranding the scooter offline.
#
# Usage:
#   offline-test.sh [duration-seconds]
#
# Default duration: 240s (enough to cross the 3-min offline debounce).

set -u

DURATION=${1:-240}
SAFETY_TIMEOUT=$((DURATION + 120))
WATCHDOG_LOG=/tmp/offline-test-watchdog.log

log() { printf '[%s] %s\n' "$(date -u +%FT%TZ)" "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || {
    log "FATAL: $1 not found"
    exit 1
  }
}

need mmcli
need systemctl

recover() {
  log "Recovery: AT+COPS=0 + restart librescoot-modem"
  mmcli -m any --command='AT+COPS=0' >/dev/null 2>&1 || true
  systemctl restart librescoot-modem >/dev/null 2>&1 || true
}

log "Starting watchdog (+${SAFETY_TIMEOUT}s absolute)"
(
  sleep "$SAFETY_TIMEOUT"
  printf '[%s] WATCHDOG FIRED\n' "$(date -u +%FT%TZ)" >>"$WATCHDOG_LOG"
  mmcli -m any --command='AT+COPS=0' >>"$WATCHDOG_LOG" 2>&1 || true
  systemctl restart librescoot-modem >>"$WATCHDOG_LOG" 2>&1 || true
) &
WATCHDOG=$!
disown $WATCHDOG 2>/dev/null || true
log "Watchdog pid=$WATCHDOG; log=$WATCHDOG_LOG"

cleanup() {
  recover
  kill "$WATCHDOG" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

log "Deregistering cellular: AT+COPS=2"
mmcli -m any --command='AT+COPS=2'
log "Offline for ${DURATION}s — watch:  redis-cli HGET modem connectivity"
sleep "$DURATION"
log "Re-registering on exit (trap handles it)"
