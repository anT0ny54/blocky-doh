# Changelog

Entries use the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) style. Only the latest modification set is kept here.

## [Unreleased] - 2026-11-08

Re-review of the four-file patch against all 13 project files. No project version bump.

### Updated

- `Dockerfile`: updated the builder image from `golang:1.27.1-alpine` to `golang:1.27.3-alpine3.24`. Go 1.27.3 includes a security fix in `crypto/x509`; Go 1.27.2 also includes security fixes in packages including `net/http`.
- `.github/workflows/Keep-Alive.yml`: upgraded `actions/checkout` from the v5 to the v7 major release.
- `README.md`: updated the documented build toolchain to Go 1.27.3.

### Review

- Re-read all 13 project files, including the complete gateway and test sources. No dead or redundant Go declarations, confirmed gateway logic defects, or configuration conflicts requiring further source changes were found.
- The Docker health check already trims whitespace from `PORT` before applying its `8080` default, and the `SC2086` ShellCheck suppression is already directly above the intentional word-splitting loop in `entrypoint.sh`. Neither was changed by this patch.
- Checked the options used by `config.yml` against the schema shipped with Blocky v0.35.0; the relevant upstream, listener, cache, bootstrap DNS, and query-log settings are supported.

### Verified

- `gofmt -d main.go main_test.go` — clean.
- `go vet ./...`, `go test -count=1 ./...`, and `go test -race -count=1 ./...` — passed with the locally installed Go 1.23.2 compiler using a temporary copy whose module directive was lowered to `go 1.23` for compatibility. The project's original `go.mod` remains unchanged at `go 1.27`.
- `sh -n entrypoint.sh` and `busybox sh -n entrypoint.sh` — passed.
- The Blocky, Compose, and GitHub Actions YAML files parsed successfully.
- The exact `golang:1.27.3-alpine3.24` image tag and the `actions/checkout` v7 release line were verified against their published registries.

### Not verified

- The actual Go 1.27.3 Docker build and container runtime were not executed because Docker and the Go 1.27.3 toolchain were unavailable in the environment.
- `shellcheck` was not installed; shell syntax was checked with POSIX `sh` and BusyBox `ash` instead.
