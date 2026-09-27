#!/bin/sh
set -eu

BLOCKY_PID=0
GATEWAY_PID=0

shutdown() {
  set +e
  if [ "$GATEWAY_PID" -gt 0 ]; then kill -TERM "$GATEWAY_PID" 2>/dev/null || true; fi
  if [ "$BLOCKY_PID" -gt 0 ]; then kill -TERM "$BLOCKY_PID" 2>/dev/null || true; fi
  wait "$GATEWAY_PID" 2>/dev/null || true
  wait "$BLOCKY_PID" 2>/dev/null || true
}
trap shutdown INT TERM EXIT

# Blocky 0.25 binds only to loopback; the public port is handled by doh-gateway.
/blocky --config /etc/blocky/config.yml &
BLOCKY_PID=$!

# Give Blocky a brief head-start without adding a heavyweight supervisor.
sleep 0.2

/doh-gateway &
GATEWAY_PID=$!

wait -n
status=$?
exit "$status"
