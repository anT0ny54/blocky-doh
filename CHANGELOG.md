# Changelog

Entries use the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) style. Only the latest modification set is kept here.

## [Unreleased] - 2026-11-15

Sixth-pass audit of the merged main project and the supplied modified-file archives. No project version bump.

### Fixed

- Tightened `entrypoint.sh` validation of `ports.freeBind`: only a direct, unquoted lowercase `true` or `false` is accepted. Quoted values are YAML strings, not booleans, and could otherwise pass the guard before failing Blocky's typed configuration decoder.

### Changed

- Added positive regression cases for both plain boolean values and rejection cases for single- and double-quoted string values.
- Updated `README.md` to describe the exact accepted `freeBind` syntax.

### Verified

- `gofmt`, `go vet ./...`, `go test ./...`, `go test -race ./...`, and `go test -cover ./...` pass on a disposable Go 1.23.2 compatibility copy; statement coverage is recorded by the test command.
- `sh -n`, `dash -n`, and BusyBox `sh -n` pass. The startup-guard regression suite is exercised with host shell/awk and with BusyBox `sh`/`awk` on `PATH`.
- `config.yml` parses and retains the required loopback listeners and `pprof.enable: false`.

### Not verified

- The repository declares Go 1.27; local Go 1.23.2 checks used a disposable copy with only its `go` directive lowered to 1.23. The submitted `go.mod` remains unchanged.
- The actual Go 1.27.4/Docker build and Blocky v0.36.0 `validate` command were not run because Docker and outbound build-network access are unavailable. ShellCheck is not installed.
