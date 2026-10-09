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

# Unprivileged-port guard. The image runs as the unprivileged "app" user
# without CAP_NET_BIND_SERVICE, so any listener below 1024 dies at bind
# time with "permission denied" (Blocky on its default :53 logs:
# "start udp listener failed: listen udp :53: bind: permission denied")
# and the container crash-loops. Refuse such configurations up front with
# an error that names the offending setting instead.
require_unprivileged() {
  what=$1
  value=$2
  p=${value##*:}
  # strip surrounding quotes, brackets and whitespace
  p=$(printf '%s' "$p" | sed 's/^[^0-9]*//; s/[^0-9]*$//')
  case $p in
    ''|*[!0-9]*) return 0 ;;  # non-numeric: let the daemon report its own error
  esac
  if [ "$p" -lt 1024 ]; then
    printf 'entrypoint: refusing to start: %s requests privileged port :%s,\n' "$what" "$p" >&2
    printf 'entrypoint: the container runs as user "app" without CAP_NET_BIND_SERVICE.\n' >&2
    printf 'entrypoint: use a port >= 1024 (see config.yml and PORT), or grant CAP_NET_BIND_SERVICE.\n' >&2
    exit 1
  fi
}

BLOCKY_CONFIG=/etc/blocky/config.yml
# Listener values under the "ports:" block of the pinned Blocky config.
# shellcheck disable=SC2046
for value in $(awk '
  /^ports:/ { inports=1; next }
  inports && /^[^[:space:]]/ { inports=0 }
  inports && /^[[:space:]]+(dns|http|tls|https):/ {
    sub(/^[[:space:]]*(dns|http|tls|https):[[:space:]]*/, "")
    print
  }
' "$BLOCKY_CONFIG"); do
  require_unprivileged "$BLOCKY_CONFIG listener" "$value"
done
if [ -n "${PORT:-}" ]; then
  require_unprivileged "PORT environment variable" "$PORT"
fi

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
