# Changelog

Entries use the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) style. Only the latest modification set is kept here.

## [Unreleased] - 2026-11-15

No project version bump. `go.mod`, `Dockerfile`, `config.yml`, `docker-compose.yml`, `.dockerignore`, the workflow, and `keep-alive.txt` are unchanged.

### Fixed

- `entrypoint.sh`: a tab-indented line inside the `ports` block was accepted by the listener guard, although YAML forbids tab indentation and the README states the guard rejects it. The guard now refuses such a block with its normal "unsupported YAML syntax" message, so the failure happens at startup in the guard rather than later in Blocky's parser. Space indentation and tab separators after a colon remain accepted.

### Changed

- `main.go`: `limiterKey` skips the `netip` parse for colon-free (IPv4) identities. These are already canonical text from `clientIdentity`, so the key is identical for every input; one parse and one string allocation are saved per DoH and `/healthz` request.
- `main.go`: upstream DNS replies are read into a pooled fixed-size buffer (`sync.Pool`) instead of an `io.ReadAll` buffer that grows as it fills. Reply bytes are unchanged. In a benchmark with a 620-byte reply, allocations per DoH request fell from 61 to 58 (about 5%).

### Tests

- `main_test.go`: new `TestEntrypointConfigGuard` case rejecting tab-indented listeners. It fails on the previous `entrypoint.sh`.
- `main_test.go`: new `TestLimiterKeyIPv4FastPathKeepsIdentityText` covering IPv4, unparseable, empty, and IPv6 identities.

### Documentation

- `README.md`: the tab-indentation sentence now states that the rule applies inside the `ports` block.

### Review

- Read all 13 tracked files in full before judging.
- Dead and redundant code: none found. Every function, constant, variable, and type is referenced beyond its declaration, so nothing was removed.
- Bugs: none reproduced in the Go code. The race detector is clean.
- Conflicts: one, the tab-indentation claim fixed above. The README's status codes, limits, and defaults were checked against the running gateway and match.

### Verified

- `gofmt`, `go vet`, `go test`, `go test -race`, and `go test -cover` (83.8% of statements) pass. The toolchain was Go 1.22.2 on a scratch copy whose `go` directive was lowered from 1.27 to 1.22.
- `shellcheck -s sh entrypoint.sh` and `sh -n entrypoint.sh` pass. ShellCheck caught an apostrophe in a new awk comment that closed the single-quoted program; it was fixed before release.
- Against a stub backend, the built gateway returned `200` for POST and GET `/dns-query`, `204` for OPTIONS, `301` for `//dns-query`, `404` for unknown paths, `405` for PUT and for POST on `/healthz`, `400` for a padded `dns=`, `415` for `text/plain`, `429` with `Retry-After` once the window is full, and `200` for GET and HEAD `/healthz`. Reply bytes are identical before and after the optimization.

### Not verified

- The Go 1.27.4 toolchain, the Docker image build, and Blocky v0.35.0's `validate` command need network access or Docker, which were unavailable here.
- The `actions/checkout@v7` pin in `Keep-Alive.yml` could not be confirmed offline.
- Whether Blocky's Makefile reads `GO_BUILD_FLAGS` and `GO_SKIP_GENERATE` was not checked.
