# Changelog

Entries use the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) style. Only the latest modification set is kept here.

## [Unreleased] - 2026-10-30

Maintenance and correctness pass after reading every project file and the full test suite. No project version was changed.

### Fixed

- `/healthz` now returns `503` when its client-state table is full, reserving `429` and `Retry-After` for identities that actually exceed their health request budget.
- `/healthz` now applies the configured IPv6 prefix grouping, preventing address rotation within one routed prefix from multiplying the health request budget or filling the table with duplicate identities.
- Upstream response-body read timeouts now return `504` like request and response-header timeouts; ordinary invalid or failed upstream responses remain `502`.
- An unexpected upstream transport cancellation now returns `502` while the client request is still active; only cancellation of the client request itself exits without attempting a response.
- `TRUSTED_PROXIES` now uses the broad loopback/link-local/private-network defaults only when the variable is unset. An explicitly empty or all-invalid value trusts no non-loopback proxy networks instead of silently widening trust; loopback remains an explicit local-trust exception.

### Changed

- Peer address parsing and normalization are shared so `clientIdentity` parses the TCP peer once and reuses the normalized address for trust checks and fallback identity; scoped IPv6 zone identifiers are stripped before trust matching and rate grouping.
- Signed proxy identities now verify the exact supplied header value and enforce the documented lowercase hexadecimal signature format before parsing the IP.
- The health limiter now runs the same periodic idle-state cleanup as the DoH limiter, avoiding retention of stale health identities until the table reaches capacity.
- The DNS resource-record ceiling is now derived from the 8,192-byte wire-message bound (743 minimum-size records), removing an unreachable 4,096-record allowance.
- Added regression coverage for upstream response-body timeouts and unexpected transport cancellation alongside health limiter capacity and IPv6 prefix grouping, explicit trusted-proxy configuration, trusted versus untrusted peers, strict identity handling, loopback trust and fallback, and signed identity verification.
- Updated the README for health status and IPv6 identity behavior, trusted-proxy fail-closed behavior and loopback exception, limiter-state retention, separate DoH/health client-state table limits, and consistent upstream timeout status handling.
