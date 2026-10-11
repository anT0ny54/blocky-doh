# Changelog

All notable changes to this project are documented here. Entries use the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) style.

## [Unreleased] - 2026-10-11

### Changed

- Made the `MAX_CONCURRENT` hard cap explicit with `maxConcurrentLimit`, instead of passing `defaultMaxConcurrent` as both the default and upper bound at the `parseEnvInt` call site. Runtime behavior is unchanged: accepted values remain 1–8, with invalid values falling back to 8.
- Documented the rate limiter's lock-ordering contract: `limiter.mu` must be acquired before `clientState.mu`. Existing `check`, `evictStaleLocked`, and `retryAfter` paths already preserve that order; the comment prevents a future reverse-order path from introducing a deadlock.

### Documentation

- Clarified that `MAX_CONCURRENT` defaults to 8 and cannot be raised above 8; it can only lower the concurrency limit.
- Added this change history file, which is already linked from the README.

### Notes

- No request-handling, rate-limiting, or response-status behavior changes in this update.
