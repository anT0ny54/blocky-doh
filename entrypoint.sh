#!/bin/sh
set -eu

BLOCKY_PID=0
GATEWAY_PID=0
STATUS_DIR=$(mktemp -d)
BLOCKY_STATUS="$STATUS_DIR/blocky"
GATEWAY_STATUS="$STATUS_DIR/gateway"

# Run each service behind a tiny supervisor. The supervisor is a direct child
# of this shell, forwards termination to the real service, and records the
# service's exit status before it exits. This lets the parent detect either
# child without relying on BusyBox wait -n.
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

# Stop the gateway first so it can drain in-flight requests (up to 5s) while
# Blocky is still answering them, and only then stop Blocky.
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

# Blocky v0.35.0 binds only to loopback; the public port is handled by doh-gateway.
run_supervised "$BLOCKY_STATUS" /blocky --config /etc/blocky/config.yml &
BLOCKY_PID=$!

# No startup delay is needed: the gateway answers 503 on /healthz and 502 on
# /dns-query until Blocky's loopback listener is up.
run_supervised "$GATEWAY_STATUS" /doh-gateway &
GATEWAY_PID=$!

# Exit as soon as either supervisor records that its service has exited.
# Unlike wait -n, this uses only POSIX wait plus a small status-file poll and
# therefore does not depend on BusyBox's wait -n implementation.
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
