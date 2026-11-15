#!/bin/sh
# The signal and shutdown handlers below are only ever invoked through trap
# handlers, which ShellCheck cannot see.
# shellcheck disable=SC2317
set -eu

BLOCKY_PID=0
GATEWAY_PID=0
SHUTDOWN_DONE=0

# The listener guard accepts only the project's fixed loopback DNS and HTTP
# scalar listeners. It recognizes list continuations only to reject values
# that would change the fixed listener cardinality. Extra TLS or
# HTTPS listeners and PROXY protocol are incompatible with this gateway.
# YAML aliases, merges, tags, block scalars, and multiline quoted values are
# refused because this small parser cannot safely resolve them.
BLOCKY_CONFIG=/etc/blocky/config.yml
awk '
  BEGIN { single_quote = sprintf("%c", 39) }

  function trim_space(value) {
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
    return value
  }

  # Remove YAML comments only when # is outside a quoted scalar and is
  # preceded by whitespace. This keeps quoted '#' characters intact.
  function strip_yaml_comment(value, i, ch, quote, escaped) {
    quote = ""
    escaped = 0
    for (i = 1; i <= length(value); i++) {
      ch = substr(value, i, 1)
      if (quote == "\"") {
        if (ch == "\\" && !escaped) {
          escaped = 1
          continue
        }
        if (ch == "\"" && !escaped) quote = ""
        escaped = 0
      } else if (quote == single_quote) {
        if (ch == single_quote) {
          if (substr(value, i + 1, 1) == single_quote) {
            i++
            continue
          }
          quote = ""
        }
      } else if (ch == "\"" || ch == single_quote) {
        quote = ch
      } else if (ch == "#" && i > 1 && substr(value, i - 1, 1) ~ /[[:space:]]/) {
        return substr(value, 1, i - 1)
      }
    }
    return value
  }

  function is_ports_key(line) {
    return line ~ /^ports[[:space:]]*:/ ||
      line ~ /^"ports"[[:space:]]*:/ ||
      line ~ ("^" single_quote "ports" single_quote "[[:space:]]*:")
  }

  function is_listener_key(line) {
    return line ~ /^[[:space:]]+(dns|http|tls|https)[[:space:]]*:/ ||
      line ~ /^[[:space:]]+"(dns|http|tls|https)"[[:space:]]*:/ ||
      line ~ ("^[[:space:]]+" single_quote "(dns|http|tls|https)" single_quote "[[:space:]]*:")
  }

  function is_doh_path_key(line) {
    return line ~ /^[[:space:]]+dohPath[[:space:]]*:/ ||
      line ~ /^[[:space:]]+"dohPath"[[:space:]]*:/ ||
      line ~ ("^[[:space:]]+" single_quote "dohPath" single_quote "[[:space:]]*:")
  }

  function emit_values(value, count, values, i, item, first, last) {
    value = trim_space(strip_yaml_comment(value))
    if (value == "") return
    if (substr(value, 1, 1) == "[" && substr(value, length(value), 1) == "]") {
      value = substr(value, 2, length(value) - 2)
      count = split(value, values, ",")
    } else {
      values[1] = value
      count = 1
    }
    for (i = 1; i <= count; i++) {
      item = trim_space(values[i])
      first = substr(item, 1, 1)
      last = substr(item, length(item), 1)
      if (length(item) >= 2 &&
          ((first == "\"" && last == "\"") || (first == single_quote && last == single_quote))) {
        item = substr(item, 2, length(item) - 2)
        item = trim_space(item)
      }
      # This small parser intentionally refuses aliases, anchors, tags, null
      # values and YAML merge keys rather than guessing at effective values.
      if (item ~ /^[&*!]/ || item ~ /(^|[[:space:]])[&*!][[:alnum:]_-]+/) {
        unsupported_yaml = 1
      }
      if (item == "null" || item == "Null" || item == "NULL" || item == "~") {
        unsupported_yaml = 1
      }
      if (item != "") {
        if (listener == "dns" && listener_direct) dns_value_count++
        if (listener == "http" && listener_direct) http_value_count++
        if ((listener == "tls" || listener == "https") && listener_direct) forbidden_listener = 1
      }
    }
  }

  function is_boolean_scalar(value) {
    # Blocky configuration declares freeBind as a YAML bool. Quoted true/false
    # are YAML strings, so accepting them here would pass the guard but fail
    # the typed config decoder later.
    value = trim_space(strip_yaml_comment(value))
    return value == "true" || value == "false"
  }

  {
    line = $0
    # Blocky (YAML) accepts a leading UTF-8 byte-order mark; strip it so the
    # guard reads the same first key that the parser does.
    if (NR == 1 && substr(line, 1, 3) == sprintf("%c%c%c", 239, 187, 191)) line = substr(line, 4)

    # Blocky merges multiple YAML documents in document order. This limited
    # line-oriented guard cannot safely resolve keys introduced or overridden
    # across documents, so reject document markers and YAML directives rather
    # than validating only one visible ports mapping.
    document_line = trim_space(strip_yaml_comment(line))
    if (document_line ~ /^---([[:space:]]|$)/ ||
        document_line ~ /^\.\.\.([[:space:]]|$)/ ||
        document_line ~ /^%YAML([[:space:]]|$)/ ||
        document_line ~ /^%TAG([[:space:]]|$)/) {
      unsupported_yaml = 1
      next
    }

    # Collect multi-line flow lists before interpreting their values. Otherwise
    # a privileged listener could be hidden on a continuation line.
    if (flow_pending) {
      flow_value = flow_value " " strip_yaml_comment(line)
      flow_trimmed = trim_space(flow_value)
      if (substr(flow_trimmed, length(flow_trimmed), 1) == "]") {
        emit_values(flow_value)
        flow_pending = 0
        flow_value = ""
      }
      next
    }

    # Refuse root-level YAML merge keys too. Even when an explicit ports
    # mapping would take precedence, resolving merged root mappings is outside
    # this deliberately small YAML subset.
    if (!inports && line ~ /^<<[[:space:]]*:/) {
      unsupported_yaml = 1
      next
    }

    if (is_ports_key(line)) {
      ports_block_count++
      ports_value = line
      sub(/^ports[[:space:]]*:[[:space:]]*/, "", ports_value)
      sub(/^"ports"[[:space:]]*:[[:space:]]*/, "", ports_value)
      sub(("^" single_quote "ports" single_quote "[[:space:]]*:[[:space:]]*"), "", ports_value)
      ports_value = trim_space(ports_value)
      if (ports_value != "" && substr(ports_value, 1, 1) != "#") {
        inline_ports = 1
        next
      }
      inports = 1
      listener = ""
      listener_direct = 0
      ports_indent = -1
      next
    }
    if (inports && line ~ /^[^[:space:]#]/) {
      inports = 0
      listener = ""
      listener_direct = 0
    }

    # The project configuration uses ordinary top-level mapping keys. YAML
    # also permits explicit, tagged, anchored, aliased, and escaped quoted
    # keys; those forms can decode to a second "ports" key while the simple
    # line-oriented guard sees only the safe literal mapping. Refuse those
    # top-level key forms rather than relying on the downstream YAML decoder
    # to resolve a duplicate or hidden listener mapping.
    if (!inports && (line ~ /^[&*!?]/ ||
        line ~ /^"[^"]*\\[^"]*"[[:space:]]*:/)) {
      unsupported_yaml = 1
      next
    }
    if (!inports || line ~ /^[[:space:]]*($|#)/) next

    # YAML forbids tab indentation. Blocky would fail to parse such a block, so
    # refuse it here as well, so the verdict matches the documented contract.
    if (line ~ /^ *\t/) {
      unsupported_yaml = 1
      next
    }

    if (line ~ /^[[:space:]]+<<[[:space:]]*:/ ||
        line ~ /^[[:space:]]+[&*!?]/) {
      # Merge keys, tagged/anchored/aliased keys, and explicit YAML mapping
      # keys can hide listener names from this deliberately small parser.
      unsupported_yaml = 1
      listener = ""
      listener_direct = 0
      next
    }

    prefix = line
    sub(/[^[:space:]].*$/, "", prefix)
    current_indent = length(prefix)
    if (ports_indent < 0) ports_indent = current_indent

    if (is_doh_path_key(line)) {
      value = line
      sub(/^[[:space:]]+/, "", value)
      if (substr(value, 1, 1) == "\"") {
        sub(/^"dohPath"[[:space:]]*:[[:space:]]*/, "", value)
      } else if (substr(value, 1, 1) == single_quote) {
        sub(("^" single_quote "dohPath" single_quote "[[:space:]]*:[[:space:]]*"), "", value)
      } else {
        sub(/^dohPath[[:space:]]*:[[:space:]]*/, "", value)
      }
      trimmed = trim_space(strip_yaml_comment(value))
      first = substr(trimmed, 1, 1)
      last = substr(trimmed, length(trimmed), 1)
      if (current_indent != ports_indent) {
        doh_path_invalid = 1
      } else {
        doh_path_count++
        if ((first == "\"" && last == "\"") || (first == single_quote && last == single_quote)) {
          trimmed = substr(trimmed, 2, length(trimmed) - 2)
        }
        if (trimmed != "/dns-query") doh_path_invalid = 1
      }
      listener = ""
      listener_direct = 0
      next
    }

    if (is_listener_key(line)) {
      value = line
      sub(/^[[:space:]]+/, "", value)
      if (substr(value, 1, 1) == "\"") {
        sub(/^"(dns|http|tls|https)"[[:space:]]*:[[:space:]]*/, "", value)
      } else if (substr(value, 1, 1) == single_quote) {
        sub(("^" single_quote "(dns|http|tls|https)" single_quote "[[:space:]]*:[[:space:]]*"), "", value)
      } else {
        sub(/^(dns|http|tls|https)[[:space:]]*:[[:space:]]*/, "", value)
      }
      value = strip_yaml_comment(value)
      trimmed = trim_space(value)

      key = line
      sub(/^[[:space:]]+/, "", key)
      quote = substr(key, 1, 1)
      if (quote == "\"" || quote == single_quote) {
        key = substr(key, 2)
        sub((quote "[[:space:]]*:.*$"), "", key)
      } else {
        sub(/[[:space:]]*:.*$/, "", key)
      }
      listener = key
      listener_direct = current_indent == ports_indent
      if (!listener_direct) unsupported_yaml = 1
      # TLS/HTTPS are forbidden by key presence, even when YAML gives the
      # setting an empty or otherwise false-looking value.
      if ((listener == "tls" || listener == "https") && listener_direct) {
        forbidden_listener = 1
      }
      if (listener == "dns" && listener_direct) {
        dns_key_count++
        dns_candidate = trimmed
        dns_first = substr(trimmed, 1, 1)
        dns_last = substr(trimmed, length(trimmed), 1)
        if ((dns_first == "\"" && dns_last == "\"") ||
            (dns_first == single_quote && dns_last == single_quote)) {
          dns_candidate = substr(trimmed, 2, length(trimmed) - 2)
        }
        if (dns_candidate == "127.0.0.1:5353") dns_exact = 1
        else dns_exact = 0
      }
      if (listener == "http" && listener_direct) {
        http_key_count++
        http_candidate = trimmed
        http_first = substr(http_candidate, 1, 1)
        http_last = substr(http_candidate, length(http_candidate), 1)
        if ((http_first == "\"" && http_last == "\"") ||
            (http_first == single_quote && http_last == single_quote)) {
          http_candidate = substr(http_candidate, 2, length(http_candidate) - 2)
        }
        if (http_candidate == "127.0.0.1:8053") http_exact = 1
        else http_exact = 0
      }

      # Block scalars and multi-line quoted values are valid YAML but are not
      # parsed by this small listener guard. Fail closed instead of skipping
      # their contents.
      if (trimmed ~ /^[|>][0-9+-]*$/) {
        unsupported_yaml = 1
        next
      }
      first = substr(trimmed, 1, 1)
      last = substr(trimmed, length(trimmed), 1)
      if ((first == "\"" || first == single_quote) && last != first) {
        unsupported_yaml = 1
        next
      }

      if (first == "[" && last != "]" && trimmed !~ /^\[[^]]+\]:[0-9]+$/) {
        flow_pending = 1
        flow_value = value
      } else {
        emit_values(value)
      }
      next
    }

    # A quoted/escaped or otherwise unfamiliar key may decode to a listener
    # name. The only additional supported field is freeBind, which must be a
    # direct scalar boolean; nested mappings, aliases, tags, and nulls are not
    # part of the accepted ports subset.
    key = trim_space(line)
    quote = substr(key, 1, 1)
    if (quote == "\"" || quote == single_quote) {
      rest = substr(key, 2)
      key_end = index(rest, quote)
      if (key_end > 0 && substr(rest, key_end + 1) ~ /^[[:space:]]*:/) {
        key_name = substr(rest, 1, key_end - 1)
        value = substr(rest, key_end + 1)
        sub(/^[[:space:]]*:[[:space:]]*/, "", value)
        if (key_name == "freeBind" && current_indent == ports_indent) free_bind_count++
        if (key_name != "freeBind" || current_indent != ports_indent || free_bind_count > 1 || !is_boolean_scalar(value)) {
          unsupported_yaml = 1
        }
        listener = ""
        listener_direct = 0
        next
      }
    }

    if (line ~ /^[[:space:]]+[[:alnum:]_-]+[[:space:]]*:/) {
      key = trim_space(line)
      sub(/[[:space:]]*:.*$/, "", key)
      value = line
      sub(/^[[:space:]]+[[:alnum:]_-]+[[:space:]]*:[[:space:]]*/, "", value)
      # Only non-listener fields that do not affect the gateway fixed
      # backend contract are allowed here. In particular, PROXY protocol
      # changes how Blocky accepts the HTTP connection from this gateway.
      if (key == "freeBind" && current_indent == ports_indent) free_bind_count++
      if (key != "freeBind" || current_indent != ports_indent || free_bind_count > 1 || !is_boolean_scalar(value)) {
        unsupported_yaml = 1
      }
      listener = ""
      listener_direct = 0
      next
    }
    if (listener != "" && line ~ /^[[:space:]]+-[[:space:]]*/) {
      value = line
      sub(/^[[:space:]]*-[[:space:]]*/, "", value)
      value = strip_yaml_comment(value)
      trimmed = trim_space(value)
      if (trimmed ~ /^[|>][0-9+-]*$/) {
        unsupported_yaml = 1
        next
      }
      first = substr(trimmed, 1, 1)
      last = substr(trimmed, length(trimmed), 1)
      if ((first == "\"" || first == single_quote) && last != first) {
        unsupported_yaml = 1
        next
      }
      emit_values(value)
      next
    }

    # A scalar listener (or freeBind boolean) cannot have further indented
    # content. Reject plain continuations instead of accidentally interpreting
    # malformed YAML as a second valid scalar.
    if (line ~ /^[[:space:]]+/) {
      unsupported_yaml = 1
      listener = ""
      listener_direct = 0
      next
    }
  }

  END {
    # Blocky defaults ports.dns to 53. Require one exact scalar DNS listener,
    # one exact scalar HTTP backend, and one top-level ports mapping. Permit at
    # most one scalar ports.dohPath (if set), and reject extra TLS/HTTPS or
    # PROXY-protocol listeners and syntax this guard cannot resolve.
    if (flow_pending || inline_ports || unsupported_yaml || forbidden_listener ||
        ports_block_count != 1 || dns_key_count != 1 || dns_value_count != 1 || !dns_exact ||
        http_key_count != 1 || http_value_count != 1 || !http_exact ||
        doh_path_count > 1 || doh_path_invalid) exit 2
  }

' "$BLOCKY_CONFIG" || {
  # Fail closed: an unreadable, incomplete, or unsupported config must not
  # skip the listener-port guard.
  printf 'entrypoint: refusing to start: cannot safely validate the required loopback listener configuration in %s (unreadable, incomplete, conflicting, or unsupported YAML syntax)\n' "$BLOCKY_CONFIG" >&2
  exit 1
}
# The public listener must use a fixed, unprivileged TCP port. Port zero and
# a bare ':' are accepted by Go as requests for an ephemeral port, but the
# platform health check probes the configured port and would never reach it.
validate_public_port() {
  raw_port=${PORT:-}
  port_value=$raw_port
  # Match strings.TrimSpace in main.go for leading/trailing shell whitespace.
  port_value=${port_value#"${port_value%%[![:space:]]*}"}
  port_value=${port_value%"${port_value##*[![:space:]]}"}
  [ -n "$port_value" ] || return 0
  case $port_value in
    :*) port_value=${port_value#:} ;;
  esac
  case $port_value in
    ''|*[!0-9]*)
      printf 'entrypoint: refusing to start: PORT must be one TCP port from 1024 through 65535 (blank selects 8080)\n' >&2
      exit 1
      ;;
  esac
  while :; do
    case $port_value in
      0?*) port_value=${port_value#0} ;;
      *) break ;;
    esac
  done
  if [ -z "$port_value" ] || [ "${#port_value}" -gt 5 ] ||
      [ "$port_value" -lt 1024 ] || [ "$port_value" -gt 65535 ]; then
    printf 'entrypoint: refusing to start: PORT must be one TCP port from 1024 through 65535 (blank selects 8080)\n' >&2
    exit 1
  fi
}
validate_public_port

# The first-pass YAML guard above validates exactly one scalar ports.http key
# at the fixed loopback backend address (127.0.0.1:8053), so no second parse is
# needed and the two parsers cannot drift apart.

# Create supervision state only after configuration guards pass, so a refused
# privileged-port configuration does not leave a stale directory in /tmp.
STATUS_ROOT="${TMPDIR:-/tmp}"
STATUS_DIR=$(umask 077; mktemp -d "$STATUS_ROOT/doh-gateway-status.XXXXXX")
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

run_supervised "$BLOCKY_STATUS" /blocky --config "$BLOCKY_CONFIG" &
BLOCKY_PID=$!
run_supervised "$GATEWAY_STATUS" /doh-gateway &
GATEWAY_PID=$!

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
