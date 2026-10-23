# Changelog

Entries use the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) style. Only the latest modification set is kept here.

## [Unreleased] - 2026-10-23

Full review of every file (`main.go`, `main_test.go`, `entrypoint.sh`, `Dockerfile`, `config.yml`, `docker-compose.yml`, `.dockerignore`, the workflow, `go.mod`, `keep-alive.txt`, README). No functional bugs, deadlocks, lock-order conflicts or unreachable code paths were found; the changes below are small redundancy removals and one allocation optimization. Behavior is unchanged.

### Changed

- `clientIP` walks `X-Forwarded-For` with `strings.Cut` instead of `strings.Split`, removing a slice allocation per proxied request. Same first-valid-address result.

### Removed

- `maxConcurrentLimit` constant: it duplicated `defaultMaxConcurrent` (both `8`). `defaultMaxConcurrent` is now also the hard cap for `MAX_CONCURRENT`.
- `GOGC=100` from the Dockerfile `ENV`: it is the Go runtime default, so the setting had no effect.

### Documentation

- README, Configuration: removed `GOGC=100` from the list of Dockerfile `ENV` values and noted that `GOGC` stays at the Go default.

These changes were made without access to a Go toolchain, so run `go vet ./... && go test -race ./...` before release (the Docker build also runs `go vet` and `go test`). No project or dependency version was bumped; pinned Go, Alpine and Blocky versions were not re-verified.
