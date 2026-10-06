#!/bin/sh
set -eu
BLOCKY_PID=0
GATEWAY_PID=0
STATUS_DIR=$(mktemp -d)
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
  sleep 1
done
exit "$status"