# Blocky + DoH gateway for SnapDeploy

Minimal public DNS-over-HTTPS service using:

- Alpine Linux 3.24 runtime
- Go 1.27.1 build toolchain
- Blocky v0.35.0
- HaGeZi DoH upstreams
- DNS-only Go HTTP gateway for the exact **99 requests / 60 seconds / client IP** rule
- Public listener on `$PORT` (local default: `8080`)
- Blocky listens on loopback only: HTTP on `127.0.0.1:8053` (the gateway's backend) and DNS on `127.0.0.1:5353` (unused by the gateway, pinned to loopback so Blocky does not bind its default `:53`); no TLS/HTTPS listener

## Architecture

```text
Internet / SnapDeploy HTTPS
          |
          v
  doh-gateway :$PORT
     |       |
     |       +-- per-client-IP limiter (IPv4 exact, IPv6 grouped by prefix): 99 / 60s strict sliding window
     |       +-- per-client-IP health budget on /healthz (HEALTH_RATE, default 60 / 60s)
     |       +-- max concurrent DNS requests: 8 (default and hard cap; configurable only downward)
     |       +-- DNS wire-format validation
     |
     v
 Blocky 0.35.0
 127.0.0.1:8053
     |
     +-- HTTPS to HaGeZi DoH upstreams
```

The exact **99 requests / 60 seconds / client IP** policy is deliberately kept in the gateway rather than delegated to Blocky, so the policy remains independent of Blocky release changes.

The limiter is a **strict sliding window**: at most 99 requests per client identity fall inside any rolling 60-second span. Client identity is the exact IPv4 address, or the IPv6 address masked to `IPV6_PREFIX` (default `/64`, see Configuration) so address rotation inside one routed prefix cannot mint additional windows. There is no token refill — after a full 99-request burst, further requests are admitted only as earlier timestamps age out of the window. Two rejection modes are distinguished: an over-budget client receives `429` with a `Retry-After` header holding the whole seconds until its oldest request leaves the window (at most 60), while a new client arriving when the fixed client table is full receives `503`, so capacity exhaustion is not mistaken for per-client throttling.

Client states whose newest accepted request is more than 2 minutes old — or older than the window, if the window is longer than that — are evicted by a 60-second background sweep and, when the table is full, by an on-demand scan that runs at most once per second. Because the idle threshold is never shorter than the window, a state is never evicted while it still holds live timestamps, so eviction cannot reset a client's budget; live states are also never evicted to admit a new client. Liveness is derived from the newest stored timestamp, so a throttled client keeps its state for exactly as long as its budget is in use and no separate liveness field is tracked. The rate budget is charged only after a request passes body validation and the concurrency gate, so malformed or over-capacity requests never consume a client's window. Client identities are normalized with `net.IP.String()` after parsing, so alternate textual encodings of the same address cannot occupy multiple limiter states; the prefix mask is applied on top of that normalization. The mask is computed with `net/netip` (no per-request allocations); the derived identity-key strings are unchanged. Proxy identity headers are honored only from `TRUSTED_PROXIES` peers (see below), so `TRUST_PROXY=true` on a directly reachable port cannot be used to forge identities.

The limiter samples each request timestamp after acquiring the client-state lock, so concurrent requests for one IP are ordered with their window updates. The `Retry-After` delay is calculated from that same timestamp and the oldest in-window timestamp snapshot before the lock is released; later requests cannot change a rejection's delay, and the value is capped at the configured window as a defensive bound. Timestamps remain ordered, so expired entries are skipped with a binary search and the oldest request is available in O(1).

The `/dns-query` path is implemented by `doh-gateway`, which answers `OPTIONS` preflights (`204`) and sets `Access-Control-Allow-Origin: *` on preflights and successful DNS responses. Methods other than `GET`, `POST` and `OPTIONS` receive `405`. Blocky v0.35.0 supports DoH itself, but this project intentionally keeps every Blocky listener on loopback and serves `/dns-query` through the resource-bounded gateway instead. Blocky's loopback HTTP listener also exposes its own REST API and pprof endpoints; the gateway never proxies them, because it only ever forwards DNS messages to `/dns-query`.

## SnapDeploy deployment

1. Upload/connect this repository with the Dockerfile.
2. Use the Small container size (512 MB / 0.25 vCPU).
3. Do **not** create a `PORT` environment variable manually; SnapDeploy manages it. The image advertises port `8080`, so SnapDeploy can detect the service port.
4. No database, Redis, RabbitMQ, or other SnapDeploy add-on is required. Blocky's DNS response cache is in-memory and Blocky is otherwise stateless.
5. Add your custom domain if the service is intended for public DoH clients. SnapDeploy terminates HTTPS and routes to the container.
6. Use `/dns-query` as the DoH path.

The Docker build uses a single Go builder stage (the Small tier's two-stage limit leaves exactly one builder plus one runtime stage): it first clones and builds Blocky, then copies in the gateway sources, runs `go vet` and `go test`, and builds the gateway binary — so a failing test fails the build. Because the Blocky clone/build precedes the gateway `COPY` instructions, editing gateway sources leaves the cached Blocky layers untouched; only the later layers rebuild. That stage clones the Blocky repository at the pinned `BLOCKY_VERSION` tag (`v0.35.0`, overridable with `--build-arg BLOCKY_VERSION=<tag>`) and builds it with `make build BIN_OUT_DIR=/bin`. The build requires network access to github.com for the clone step, so restricted build environments without outbound HTTPS to GitHub fail there with a `Failed to connect to github.com port 443`-style error.

Runtime file permissions (`0555` for binaries and `entrypoint.sh`, `0444` for `config.yml`) are set in the builder stage and carried over by `COPY --from`, so the runtime stage has no `chmod` layer that would store a second copy of the Blocky binary.

The final Alpine image contains only the gateway binary, the Blocky binary, `config.yml`, `entrypoint.sh`, `ca-certificates`, and the BusyBox `wget` used by the health check, and runs as the unprivileged `app` user — **without `CAP_NET_BIND_SERVICE`, so no listener may use a port below 1024**. `config.yml` pins Blocky to loopback ports `5353`/`8053` exactly so Blocky never binds its default `:53` (which would fail with `listen udp :53: bind: permission denied`); `entrypoint.sh` additionally refuses to start with a clear, naming error if `PORT` or any Blocky listener port in `config.yml` (trailing YAML comments are ignored) is privileged, instead of crash-looping at bind time. The image health check (every 30 s, 3 s timeout, 5 s start period, 3 retries) requests `/healthz` on `$PORT` (default `8080`; a leading `:` in `PORT` is tolerated).

Example endpoint after a custom domain is attached:

`https://dns.example.com/dns-query`

The `/healthz` endpoint (`GET`/`HEAD`) returns `200` only when a TCP connection to Blocky's loopback listener (`127.0.0.1:8053`) succeeds within 250 ms, otherwise `503`. This lets the container health check catch a failed DNS backend rather than reporting the gateway as ready by itself. It is a connect check only, not a DNS query. `/healthz` is public, so it carries its own coarse per-client-IP budget (`HEALTH_RATE`, default 60 requests per 60-second window, independent of `RATE_LIMIT`); an over-budget client receives `429` with `Retry-After`. Methods other than `GET`/`HEAD` receive `405`. Other paths are intentionally rejected with `404`; the one exception is a non-canonical path such as `//dns-query` or `/a/../dns-query`, which Go's `http.ServeMux` answers with a `301` redirect to the cleaned path before routing.

## DoH handling and validation

The gateway accepts RFC-style DoH GET and POST requests on `/dns-query`:

- GET `dns=` values are decoded as canonical, unpadded base64url and forwarded internally as bounded POST requests. Padding, characters outside the base64url alphabet, embedded CR/LF, non-canonical trailing bits, malformed query escapes, and duplicate `dns` parameters are rejected with `400`.
- GET query strings longer than `dns=` plus the base64url-encoded form of 8,192 bytes (10,923 encoded characters; 10,927 characters including `dns=`) are rejected with `413` before the query string is parsed at all. This single bound also caps the `dns=` value, so no decoded message can exceed 8,192 bytes. `HEAD` and any method other than `GET`/`POST`/`OPTIONS` receives `405`.
- POST requests require `application/dns-message` and are capped at 8,192 bytes (the gateway's wire-format cap, well under the 65,535-byte DNS maximum). A declared `Content-Length` over the cap is rejected before reading the body; streaming bodies are bounded while being read.
- Incoming DNS messages are structurally validated before Blocky is called; exactly one DNS question is required and resource records are capped at 4,096.
- Upstream responses must be HTTP `200`, use `application/dns-message` and not declare a body over the size bound (all checked before the body is read, so error bodies are never buffered), fit the same size bound while reading, have valid DNS wire structure, preserve the request transaction ID, opcode and recursion-desired bit, and echo the request's question section (owner name and QTYPE/QCLASS, compression-expanded with label boundaries preserved).
- Backend response headers are capped at 16 KiB, response bodies at 8,192 bytes, and backend redirects are never followed.
- The gateway never forwards arbitrary upstream response headers to clients.

This validation is primarily a resource-safety and protocol-correctness guard; it does not replace DNSSEC validation or Blocky's resolver protections.

### Request order and status codes

`/dns-query` requests are checked in this order, and the first failing check decides the response: HTTP method, then the concurrency gate, then body/query decoding and size, then DNS wire-format validation, then client-identity resolution (a non-loopback trusted proxy without a valid identity is rejected with `503` in strict mode), then the per-client rate limiter, and finally the backend call. The concurrency gate is intentionally before parsing so malformed or oversized request floods cannot consume unbounded CPU or memory outside the configured in-flight budget. The gate slot is released as soon as the upstream answer has been verified, before the response is written to the client, so a slow-reading client cannot hold one of the few slots while its reply drains. The rate budget is charged only after DNS validation, so malformed requests do not consume a client window.

| Status | When |
|---|---|
| `200` | Valid, fully verified DNS response from Blocky (`application/dns-message`, explicit `Content-Length`, `Cache-Control: no-store`, `Access-Control-Allow-Origin: *`). |
| `204` | `OPTIONS` preflight on `/dns-query`. |
| `400` | GET without `dns=`, with duplicate `dns` parameters or malformed query escapes, padded or otherwise invalid base64url, unreadable POST body, or a message that fails DNS structural validation. |
| `405` | Method not allowed (`Allow` header is set). |
| `413` | GET raw query string over the 10,927-character bound, or POST body over 8,192 bytes. |
| `415` | POST whose `Content-Type` is not `application/dns-message` (parameters such as `charset` are accepted). |
| `429` | Client exceeded its sliding window (or its `/healthz` budget); `Retry-After` is 1–60 seconds. |
| `500` | The gateway could not build the backend request. |
| `502` | Backend unreachable, or its response failed any verification check (status, content type, size, wire format, transaction ID, opcode/RD echo, or question echo). |
| `503` | Concurrency limit reached, the client table is full and the client is new, or a non-loopback trusted proxy presented no valid client identity (strict mode). Also `/healthz` when Blocky's listener does not accept a connection. |
| `504` | Backend request exceeded its 3-second deadline or 2.5-second response-header timeout. |

All gateway-generated error responses are plain text with `Cache-Control: no-store`; they carry no CORS headers.

## Important client-IP setting

`TRUST_PROXY=true` is enabled in the production image for the intended SnapDeploy edge deployment. The gateway consults proxy identity headers **only when the TCP peer itself belongs to a `TRUSTED_PROXIES` network**; headers from any other peer are ignored and the TCP peer address is used, so `TRUST_PROXY` cannot be abused on a directly reachable port. When `TRUST_PROXY=true` and `TRUSTED_PROXIES` is unset, the gateway trusts loopback, link-local, IPv6 ULA and the RFC 1918 private ranges — the networks a platform load balancer typically connects from. Set `TRUSTED_PROXIES` to an explicit comma-separated list of IPs/CIDRs to restrict trust further (bare IPs match exactly; invalid entries are ignored). When `TRUST_PROXY=false`, no headers are ever trusted.

On top of peer trust, two mechanisms close the remaining identity gaps:

- **Signed identity (spoofing-proof):** set `TRUST_SECRET` to a shared secret and have the edge send the client IP in the `TRUST_HEADER` header (default `X-Real-Client-IP`) together with a lowercase hex HMAC-SHA256 of the exact header value in `<TRUST_HEADER>-Sig` (default `X-Real-Client-IP-Sig`). The gateway accepts the identity only when the signature verifies, so a client cannot forge an identity even if a header value reaches the gateway un-sanitized. Without `TRUST_SECRET`, the gateway falls back to the conventional headers (`CF-Connecting-IP`, first valid `X-Forwarded-For`, `X-Real-IP`), which the edge must still sanitize.
- **Strict identity (no silent proxy bucket):** with `TRUST_REQUIRE_IDENTITY=true` (the default), a non-loopback trusted peer that presents no valid identity receives `503` instead of being attributed to the proxy itself. Loopback peers are exempt, so the container health check works without identity headers. Set it to `false` only if your platform sometimes forwards requests without any client-identity header.

Rate identity is prefix-based: an actor controlling multiple routed IPv6 prefixes could still obtain multiple windows. That is a threat-model boundary for an anonymous public resolver — the per-prefix sliding window plus the bounded client table are the enforcement mechanism, and a stricter prefix can be selected with `IPV6_PREFIX`.

For a direct local/container test without a trusted proxy, set `TRUST_PROXY=false`. Invalid `TRUST_PROXY` values fall back to the safe default (`false`). The Dockerfile's production `ENV` defaults to `true` because SnapDeploy's load balancer is the trusted proxy in that path.

## Shutdown behavior

On `SIGINT`/`SIGTERM`, the gateway stops accepting new connections and drains in-flight DoH requests via a graceful HTTP shutdown (up to 5 seconds) before exiting, rather than dropping active requests immediately. `entrypoint.sh` records child exit statuses in a uniquely created, private temporary directory, avoiding PID collisions when multiple instances share a temporary volume. It shuts down in order: it first sends `SIGTERM` to the gateway and waits for it to exit, and only then stops Blocky, so Blocky keeps answering while requests drain. If either process exits on its own, the entrypoint stops the other one and exits with the first process's status; a signal-initiated shutdown exits `0`. The entrypoint polls the two processes' exit status once per second, but does so by waiting on a background `sleep`, so `SIGINT`/`SIGTERM` are acted on immediately rather than after the current poll interval. Neither process is given a startup delay: until Blocky is listening, `/healthz` returns `503` and DNS queries return `502`.

## Configuration

For `RATE_LIMIT`, `MAX_CLIENTS`, `HEALTH_RATE`, and `MAX_CONCURRENT`, unset, empty, non-numeric, below-1, or above-cap values fall back to the listed default. `IPV6_PREFIX` accepts `0`–`128` (out-of-range values fall back to the default; `0` disables grouping). `TRUST_PROXY` also falls back to its safe default (`false` in the binary) when unset, empty, or invalid. `TRUSTED_PROXIES` entries that are neither an IP nor a CIDR are ignored. `TRUST_SECRET` accepts any non-empty value (use a long random string); an empty value disables signed-identity mode. `PORT` is different: an empty value selects `8080`, a privileged port is refused by `entrypoint.sh`, and other unusable values cause the gateway to fail at startup.

| Variable | Default | Allowed range | Meaning |
|---|---|---|---|
| `PORT` | `8080` | TCP port number | Public listener port (a leading `:` is accepted). The gateway itself does not range-check it; `entrypoint.sh` refuses to start if it names a privileged port (<1024), and any other unusable value makes the gateway exit at startup. |
| `RATE_LIMIT` | `99` | 1–256 | Requests per client identity per 60-second sliding window. The window length is fixed. Identity is the exact IPv4 address, or the IPv6 address masked to `IPV6_PREFIX`. |
| `IPV6_PREFIX` | `64` | 0–128 | Prefix length used to group IPv6 clients into one rate-limit identity. `0` restores exact per-address identity (the pre-grouping behavior). IPv4 clients are always individual. |
| `MAX_CLIENTS` | `8192` | 1–32768 | Maximum client rate-limit states held in memory. |
| `MAX_CONCURRENT` | `8` | 1–8 | Maximum in-flight DNS requests, covering body parsing and backend work (the slot is released before the response is written to the client). The default and hard cap are both 8, so the variable can only lower the limit. |
| `HEALTH_RATE` | `60` | 1–256 | Per-client-IP request budget for `/healthz` per 60-second window, independent of `RATE_LIMIT`. Excess health requests receive `429`. |
| `TRUST_PROXY` | `false` in the binary, `true` in the Docker image | `true`/`false` (case-insensitive) | Use `CF-Connecting-IP`, then `X-Forwarded-For`/`X-Real-IP` for client identity, but only when the TCP peer belongs to `TRUSTED_PROXIES`; otherwise the TCP peer address is used. |
| `TRUSTED_PROXIES` | loopback, link-local, ULA and RFC 1918 ranges when `TRUST_PROXY=true`; none when `TRUST_PROXY=false` | comma-separated IPs/CIDRs | Networks whose proxy identity headers are honored when `TRUST_PROXY=true`. Bare IPs match exactly; invalid entries are ignored. |
| `TRUST_SECRET` | unset (disabled) | any non-empty string | Shared secret enabling HMAC-SHA256 verification of the `TRUST_HEADER` client-identity header; without a valid signature the header is ignored. |
| `TRUST_HEADER` | `X-Real-Client-IP` | header name | Header carrying the claimed client IP from the trusted edge; the signature arrives in `<name>-Sig`. Used only when `TRUST_SECRET` is set. |
| `TRUST_REQUIRE_IDENTITY` | `true` | `true`/`false` (case-insensitive) | Reject (`503`) requests from a non-loopback trusted proxy that carry no valid client identity, instead of attributing them to the proxy itself. Loopback peers are always exempt. |

The Dockerfile's `ENV` sets `GOMAXPROCS=1`, `GOMEMLIMIT=160MiB`, `RATE_LIMIT=99`, `MAX_CLIENTS=8192`, `MAX_CONCURRENT=8` and `TRUST_PROXY=true`. `IPV6_PREFIX`, `HEALTH_RATE`, `TRUSTED_PROXIES`, `TRUST_SECRET`, `TRUST_HEADER` and `TRUST_REQUIRE_IDENTITY` keep their binary defaults unless set explicitly. `GOGC` is left at the Go default of `100`.

The Blocky backend address (`127.0.0.1:8053`) is fixed in the gateway and must match `ports.http` in `config.yml`. The Blocky config path (`/etc/blocky/config.yml`) is fixed in `entrypoint.sh`.

## Resource tuning

The default values are chosen for the Small tier:

- `GOMAXPROCS=1` — matches the Go runtime's scheduler/GC thread count to the tier's 0.25 vCPU quota instead of the host's full core count. The Dockerfile sets it for the whole container, so it applies to the Blocky process as well as the gateway.
- `MAX_CONCURRENT=8` — bounds both request parsing and backend DNS work, which is a better fit for 0.25 vCPU. Requests beyond the limit receive `503` immediately.
- `MAX_CLIENTS=8192` — hard cap on in-memory rate-limit client states; the limiter map is allocated only when the first request arrives. When the table is full, new clients receive `503` and the event is logged, while over-budget clients receive `429`. Idle states are reclaimed after 2 minutes, so this gives headroom for thousands of active client identities (IPv4 addresses plus IPv6 prefixes) without allowing unbounded map growth.
- `IPV6_PREFIX=64` — one of the most common routed subscriber prefix sizes; grouping at `/64` stops in-prefix address rotation from multiplying rate windows. Raise it toward `128` (or set `0`) only if your clients legitimately span a single large prefix with many distinct users.
- `HEALTH_RATE=60` — the public `/healthz` endpoint gets its own budget (60 per 60 s), so it cannot be used as an unthrottled request driver; the container health check polls every 30 s and stays well under the budget.
- `TRUST_REQUIRE_IDENTITY=true` — a trusted edge that loses its client-identity headers fails closed (`503`) instead of silently giving every client one shared proxy bucket; loopback is exempt so the in-container health check is unaffected. Pair it with `TRUST_SECRET` at the edge to make identities unforgeable.
- `caching.maxItemsCount=16384` — bounded Blocky cache; this halves cache metadata versus 32,768 entries and leaves more headroom for the gateway, TLS, and Go runtime under a 512 MB container. `caching.cacheTimeNegative=5m` caches negative answers for five minutes.
- `upstreams.strategy=random` — one upstream request per cache miss in the normal path; this avoids the extra upstream fan-out of `parallel_best` and is a better fit for 0.25 vCPU.
- `upstreams.timeout=1200ms` — keeps each failed upstream attempt below the gateway's 3-second request deadline and leaves room for fallback.
- Backend HTTP response headers are capped at 16 KiB and redirects are disabled to keep the loopback-only backend path bounded and non-redirecting. The gateway's backend request deadline is 3 seconds, with a 2.5-second response-header timeout; both timeouts surface as `504`, while other backend failures surface as `502`. `/healthz` uses a separate 250 ms loopback probe cached for 2 seconds.
- `connectIPVersion=v4` — avoids unnecessary IPv6 connection attempts for the supplied upstream configuration.
- `queryLog.type=none` — query logging is explicitly disabled. Blocky's own default when `queryLog.type` is unset is `console`, which builds a log entry for every query even if `log.level=warn` then discards the line; `none` makes Blocky skip that per-query work. Prometheus metrics (off by default), prefetching (`prefetching: false`) and blocklists (no `blocking` section) are likewise disabled; blocking is performed by the HaGeZi upstreams, not by Blocky. Blocky's REST API and pprof endpoints exist on its loopback HTTP listener but are not reachable through the gateway.
- Blocky's default `upstreams.init.strategy` (`blocking`) is left unchanged.
- Gateway observability is limited to startup, listen failure, shutdown-drain, backend transport failures (connection errors and timeouts, `502`/`504`), and rate-limiter capacity messages on the standard logger (stderr). The last two are throttled to at most one line per 10 seconds each, with a count of suppressed messages appended, so request floods cannot turn logging into a resource drain. Invalid-but-delivered upstream responses (`502`) and client disconnects are not logged.

These are capacity-oriented defaults, not a guarantee of a fixed users-per-second number. Real capacity depends heavily on cache hit rate, DNS response sizes, upstream latency, and the traffic pattern.

## Upstream bootstrap

Blocky uses the configured `bootstrapDns` IPs to reach the named HTTPS upstreams without relying on the container's ordinary resolver for those bootstrap lookups. The upstream URLs remain hostname-based so normal TLS certificate and SNI handling is preserved.

| Name | Endpoint | Bootstrap IPv4 |
|---|---|---|
| HaGeZiDNS1 | `https://root.hagezi.org/dns-query` | `188.34.161.210` |
| HaGeZiDNS2 | `https://wurzn.hagezi.org/dns-query` | `159.69.155.94` |
| HaGeZiDNS3 | `https://juuri.hagezi.org/dns-query` | `95.217.163.17` |

## Local test

Build and run the same single-container image locally:

```sh
docker build -t blocky-doh .
docker run --rm -p 8080:8080 -e TRUST_PROXY=false blocky-doh
```

To run the unit tests without Docker, use `go vet ./... && go test ./...` (Go 1.27 or newer; add `-race` where cgo is available). The suite covers decode and validation rejection paths (including CR/LF smuggled into the `dns=` value), rejection of oversized GET query strings before parsing, both declared and streaming oversized POST bodies, mid-read POST failures (`failed to read dns message`, surfaced as `400`), method rejection with the `Allow` header, out-of-order limiter timestamps, bounded `Retry-After` responses, idle-state eviction relative to the window, eviction-scan throttling, lazy limiter-map allocation, concurrency-slot release before the response is written, the explicit response `Content-Length`, health-probe caching, and forwarded-identity header extraction (`CF-Connecting-IP`, first valid `X-Forwarded-For` entry, `X-Real-IP`). `entrypoint.sh` is kept clean under `shellcheck -s sh entrypoint.sh`.

`docker-compose.yml` (service `hagezi-doh`, port `8080:8080`, `restart: unless-stopped`) pins `TRUST_PROXY=false` for the same reason: a plain local compose setup has no trusted load balancer in front, so forwarded headers must be ignored.

The public container listener will be at `http://127.0.0.1:8080` locally. DoH clients should use `/dns-query` over HTTPS only when a valid TLS endpoint is in front of the container.

## DoH test

POST a DNS wire-format body:

```sh
curl -sS \
  -H 'content-type: application/dns-message' \
  --data-binary @query.bin \
  http://127.0.0.1:8080/dns-query \
  -o response.bin
```

For a GET request, the `dns` query parameter is unpadded base64url containing the DNS wire-format message.

## SnapDeploy policy note

This project is a DNS resolver/DoH service, not a general-purpose HTTP/SOCKS proxy, tunnel, VPN panel, or remote shell. SnapDeploy currently documents restrictions on proxies/tunnels and separately documents a shared-domain per-IP request limit; a custom domain is not subject to that shared-domain limit.

## Project files

| File | Purpose |
|---|---|
| `main.go` | DoH gateway: limiter (IPv6 prefix grouping), trusted-proxy gating, validation, proxying, rate-limited health check. |
| `main_test.go` | Unit tests, run during `docker build`. |
| `go.mod` | Go module definition (`go 1.27`, no external dependencies). |
| `Dockerfile` | Single-builder-stage build and Alpine runtime image. |
| `entrypoint.sh` | Starts and supervises Blocky and the gateway; refuses privileged ports (<1024) up front. |
| `config.yml` | Blocky configuration. |
| `docker-compose.yml` | Local run with `TRUST_PROXY=false`. |
| `.dockerignore` | Deny-by-default allow-list: only the files the Dockerfile `COPY`s (`go.mod`, `main.go`, `main_test.go`, `config.yml`, `entrypoint.sh`) enter the build context, so unrelated edits never invalidate the build cache. |
| `.github/workflows/Keep-Alive.yml` | Scheduled workflow that refreshes `keep-alive.txt` so an inactive fork remains eligible for scheduled GitHub Actions. |
| `keep-alive.txt` | Timestamp refreshed by the keep-alive workflow. |
| `CHANGELOG.md` | Notes for the latest modification set. |
| `LICENSE` | License text. |

## Changelog

See [`CHANGELOG.md`](CHANGELOG.md) for the notes on the latest modification set.

## 🔗 Related projects (not part of this repository)

The services below are maintained separately and are **not** part of this repository, its build pipeline, or its release cycle. They are listed here for convenience only.

### 🌐 Free DNS Services

High-performance DNS utilizing HaGeZi Blocklists (Multi Pro + TIF).

| Blocklist | DNS-over-HTTPS (DoH) |
| :--- | :--- |
| Multi Pro + TIF | `https://freedns.koyeb.app/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns.mydoh.workers.dev/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns-pi.vercel.app/api/doh/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dnssix.netlify.app/api/doh/dns-query` |
| Multi Pro + TIF | `https://dns-93aca.containers.snapdeploy.app/dns-query` |
| Multi Pro + TIF | `https://doh-93aca.containers.snapdeploy.app/dns-query` |

### ⚡ Bandwidth Hero Server

A lightweight image optimization proxy designed to slash bandwidth usage and accelerate web browsing.

Bandwidth Hero Server fetches remote images, compresses them on the fly, and delivers optimized versions to the client. This significantly reduces data consumption while improving page load performance.

🖥️ **Live Demo:** [Bandwidth Hero](https://bhserv.netlify.app/).

## Supporting the Project

If you find this project useful, donations are appreciated:

- **Bitcoin**: `1HntwKxyqGCfnSGvGLMUTRAqLnTvLarAQP`

## License

See [`LICENSE`](LICENSE).
