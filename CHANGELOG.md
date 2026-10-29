# Changelog

Entries use the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) style. Only the latest modification set is kept here.

## [Unreleased] - 2026-10-29

Second full maintenance pass: every file was re-read in full (`main.go`, `main_test.go`, `entrypoint.sh`, `Dockerfile`, `config.yml`, `docker-compose.yml`, `README.md`, the keep-alive workflow, and this changelog). No bugs or conflicts were found; the remaining dead/redundant code was removed and per-request peer parsing was consolidated. No version was bumped.

### Removed

- `main.go` `skipDNSName`: a single-use wrapper around `walkDNSName(message, offset, nil)`; the two call sites in `validateDNSMessage` now call `walkDNSName` directly.
- `main.go` `isLoopbackIP`: after `clientIdentity` was changed to parse the TCP peer once into a `net.IP`, the loopback test is `ip.IsLoopback()` on that same value; the helper had no remaining callers.
- `main.go` `clientIP` trust-proxy parameter: production always passed `false` (header trust is decided by `clientIdentity`, which extracts via `forwardedIdentity`); the `true` branch existed only for tests and duplicated `forwardedIdentity`. `clientIP` now always returns the normalized TCP peer.

### Changed

- `main.go` `clientIdentity` parses `r.RemoteAddr` exactly once per request instead of up to three times (`peerIPString`, then a re-parse inside `peerTrusted`, then a third parse in `isLoopbackIP`); `peerTrusted` now takes the parsed `net.IP`. Trust and strict-identity decisions are unchanged. As a side effect, a bracketed peer without a port (e.g. a bare `[::1]`) is now recognized as loopback, matching `clientIP`'s existing normalization.
- `main_test.go`: the five `clientIP` tests were re-pointed — header-extraction expectations now exercise `forwardedIdentity` (`TestForwardedIdentity*`), peer-only expectations exercise the header-free `clientIP` (`TestClientIPIgnoresForwardedHeaders`, `TestClientIPNormalizesIPv6`).
- `README.md`: the local-test summary now names forwarded-identity header extraction as covered by the suite.

### Notes

- No Go toolchain was available during this change, so run `gofmt -l . && go vet ./... && go test -race ./...` before release (the Docker build also runs `go vet` and `go test`).
