# Changelog

Entries use the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) style. Only the latest modification set is kept here.

## [Unreleased] - 2026-11-15

Tenth full review of the original project with the review-9 patch applied. No project version bump.

### Updated

- `README.md`: clarify that Blocky v0.36.0's new `pprof.enable` setting defaults to `true`; an explicit opt-out must be adopted together with a tested Blocky upgrade, not added to the current v0.35.0 configuration.

### Review

- Read all 13 files in the merged project in full before judgement (4,237 lines, including the complete Go implementation and test suite).
- No dead or redundant production code, reproducible runtime defect, or configuration conflict was identified. Removing code or tuning runtime limits would not be justified by the available evidence; no production-code changes were made in this review.
- Confirmed that the pinned `golang:1.27.4-alpine3.24` image tag exists and that Go 1.27.4 is a released toolchain. Retained the project's existing Go and Blocky pins.
- Blocky v0.36.0 adds `pprof.enable` for disabling `/debug` profiler endpoints. The current v0.35.0 pin does not include that option; an upgrade and config change should be tested together in the actual image before adoption.
- No project version was changed; `go.mod` remains unchanged.

### Verified

- `gofmt`, `go test ./...`, `go test -race ./...`, `go vet ./...`, `go build ./...`, and `go test -cover ./...` (83.8% statement coverage) pass using Go 1.23.2 in a temporary compatibility copy with only its Go directive lowered to 1.23.
- Builder-mode checks with `CGO_ENABLED=0`: `go test`, `go vet`, and a production-style trimmed build pass in the same temporary compatibility copy.
- POSIX `sh -n` and BusyBox `sh -n` pass; the full startup-guard test suite passes with both host `mawk` and BusyBox `sh`/`awk`.
- `config.yml`, `docker-compose.yml`, and `.github/workflows/Keep-Alive.yml` parse as YAML.
- ZIP integrity and application of the cumulative patch over the original archive were verified.

### Not verified

- Native Go 1.27.4 compilation/tests, the Docker image build (including Blocky's config validator), and end-to-end DNS resolver behavior: Docker is unavailable and the Go 1.27.4 toolchain could not be downloaded in this environment.
- ShellCheck, Actionlint, Hadolint, and yamllint: not installed.
