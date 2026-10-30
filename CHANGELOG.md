# Changelog

Entries use the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) style. Only the latest modification set is kept here.

## [Unreleased] - 2026-10-30

Second full-project audit: all 13 files read end to end (gateway, tests,
Dockerfile, entrypoint, configs, CI workflow, docs), followed by static
cross-checks — a symbol-reference scan (every function, variable, constant
and type in `main.go` is referenced from code or tests; all 76 tests and 6
test helpers are exercised), arithmetic verification of the documented bounds
(GET raw-query bound 10,927 characters, resource-record cap 743), and a
claim-by-claim pass of the README against the code, tests, `config.yml` and
`entrypoint.sh`. No functional behavior changed; the public API, status-code
matrix and configuration surface are unchanged.

### Fixed

- Precision fix in `README.md` and the `maxDNSResourceRecords` comment in
  `main.go`: 743 is the maximum *uncompressed* record count that fits an
  8,192-byte message (compression pointers would allow more); the cap itself
  is unchanged and remains correct as a conservative bound.

### Verified (no changes required)

- Bugs: none found. Re-traced the sliding-window limiter (lock ordering,
  eviction safety), strict base64url GET decoding, DNS wire validation,
  upstream response verification, timeout classification and graceful shutdown.
- Dead/redundant code: none — `clientIP()` is retained because tests exercise
  it directly; the previously removed unreachable `/healthz` default branch
  stays removed.
- Conflicts: none — `go.mod` matches the Dockerfile builder, the
  `.dockerignore` allow-list matches the Dockerfile `COPY` set, and README
  claims re-verified against code, tests, `config.yml` and `entrypoint.sh`.
- Optimizations assessed and rejected: passing `netip.Addr` through
  `clientIdentity`/`limiterKey` to avoid one IP re-parse per request remains
  negligible and disruptive to test-facing signatures; the existing
  optimizations (lazy limiter maps, pooled canonical-name buffers,
  binary-search expiry, cached health probe, throttled logging) are confirmed
  in place.
