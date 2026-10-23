# Changelog

All notable changes to this project are documented here. Entries use the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) style.

## [Unreleased] - 2026-10-23

### Fixed

- Compare upstream DNS question names with canonical label-length-prefixed wire names, preserving label boundaries and ASCII case-insensitivity; verify response opcode and recursion-desired-bit echo; and report excessive compression-pointer chains accurately.
- Reject malformed GET query escapes, duplicate `dns` parameters, characters ignored by Go's base64 decoder, and non-canonical trailing base64 bits, while retaining the pre-parse raw-query size limit.
- Compute `Retry-After` from the oldest timestamp even when concurrent requests append timestamps out of chronological order, and prevent client liveness timestamps from regressing.
- Use an atomically-created private entrypoint status directory instead of a PID-derived path, avoiding collisions between instances that share a temporary volume.
- Create `/out` in the Docker builder before writing the gateway binary, preventing a clean-image build failure when the directory does not exist.
- Correct the documented maximum GET query length to 10,923 base64url characters plus the four-character `dns=` prefix (10,927 total), and clarify `PORT` startup behavior.

- Check upstream HTTP status, `Content-Type` and declared length before reading the response body, so error responses are never buffered.
- Remove a redundant presence check in GET `dns=` decoding and a duplicated comment.
- Ignore trailing YAML comments when `entrypoint.sh` scans Blocky listener ports, so a comment such as `# 53` cannot trigger a false privileged-port refusal; reuse `$BLOCKY_CONFIG` instead of a second hard-coded config path.
- Set runtime file permissions in the Docker builder stage instead of a `chmod` layer in the runtime stage, which duplicated the Blocky binary in the final image.
- Tighten `.dockerignore` so documentation, workflow and compose files stay out of the build context; add a trailing newline to `docker-compose.yml` and remove trailing whitespace from the keep-alive workflow.

### Tests

- Added regressions for ambiguous DNS label boundaries, upstream opcode/RD mismatches, malformed or duplicate GET parameters, and out-of-order limiter timestamps.

- Fixed a `t.Fatal` call made from a non-test goroutine in the concurrency-gate test (now `t.Error`), closed a leaked listener in the health-cache test, and replaced a duplicate malformed-escape case with padded-base64url and missing-`dns` regressions.

No project or dependency version was bumped.
