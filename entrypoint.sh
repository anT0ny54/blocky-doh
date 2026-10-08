#!/bin/sh
set -eu

BLOCKY_PID=0
GATEWAY_PID=0
SHUTDOWN_DONE=0

# Deterministic status directory: stale directories left behind by
# previously SIGKILLed containers are removed before ours is created.
STATUS_ROOT="${TMPDIR:-/tmp}"
rm -rf "$STATUS_ROOT"/doh-gateway-status.* 2>/dev/null || :
STATUS_DIR="$STATUS_ROOT/doh-gateway-status.$$"
( umask 077 && mkdir -p "$STATUS_DIR" )
BLOCKY_STATUS="$STATUS_DIR/blocky"
GATEWAY_STATUS="$STATUS_DIR/gateway"

run_supervised() {
  status_file=$1
  shift
  child_pid=0
  forward_term() {
    trap '' INT TERM
    if [ "$child_pid" -gt 0 ]; then
      kill -TERM "$child_pid" 2>/dev/null || :
      wait "$child_pid" 2>/dev/null || :
    fi
    exit 143
  }
  trap forward_term INT TERM
  "$@" &
  child_pid=$!
  status=0
  wait "$child_pid" || status=$?
  printf '%s\n' "$status" > "$status_file"
  exit "$status"
}

shutdown() {
  # Idempotent: the INT/TERM handler and the EXIT trap can race, and a
  # normal child exit also routes through here; after the first run this
  # is a no-op instead of re-signalling already-reaped PIDs.
  if [ "$SHUTDOWN_DONE" -eq 1 ]; then
    return 0
  fi
  SHUTDOWN_DONE=1
  trap - EXIT
  trap '' INT TERM
  set +e
  if [ "$GATEWAY_PID" -gt 0 ]; then
    kill -TERM "$GATEWAY_PID" 2>/dev/null
    wait "$GATEWAY_PID" 2>/dev/null
  fi
  if [ "$BLOCKY_PID" -gt 0 ]; then
    kill -TERM "$BLOCKY_PID" 2>/dev/null
    wait "$BLOCKY_PID" 2>/dev/null
  fi
  rm -rf "$STATUS_DIR"
}

on_signal() {
  shutdown
  exit 0
}

trap on_signal INT TERM
trap shutdown EXIT

run_supervised "$BLOCKY_STATUS" /blocky --config /etc/blocky/config.yml &
BLOCKY_PID=$!
run_supervised "$GATEWAY_STATUS" /doh-gateway &
GATEWAY_PID=$!

status=0
while :; do
  if [ -s "$BLOCKY_STATUS" ]; then
    status=$(cat "$BLOCKY_STATUS")
    break
  fi
  if [ -s "$GATEWAY_STATUS" ]; then
    status=$(cat "$GATEWAY_STATUS")
    break
  fi
  # Wait on a background sleep instead of running it in the foreground: the
  # shell defers trap handlers until a foreground command finishes, so a plain
  # `sleep 1` delayed INT/TERM handling by up to a second. `wait` returns as
  # soon as a trapped signal arrives.
  sleep 1 &
  wait "$!" 2>/dev/null || :
done
exit "$status"
