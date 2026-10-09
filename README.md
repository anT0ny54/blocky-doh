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
     |       +-- per-client-IP limiter: 99 / 60s strict sliding window
     |       +-- max concurrent DNS requests: 8 (default and hard cap)
     |       +-- DNS wire-format validation
     |
     v
 Blocky 0.35.0
 127.0.0.1:8053
     |
     +-- HTTPS to HaGeZi DoH upstreams
```

The exact **99 requests / 60 seconds / client IP** policy is deliberately kept in the gateway rather than delegated to Blocky, so the policy remains independent of Blocky release changes.

The limiter is a **strict sliding window**: at most 99 requests per client IP fall inside any rolling 60-second span. There is no token refill — after a full 99-request burst, further requests are admitted only as earlier timestamps age out of the window. Two rejection modes are distinguished: an over-budget client receives `429` with a `Retry-After` header holding the whole seconds until its oldest request leaves the window (at most 60), while a new client arriving when the fixed client table is full receives `503`, so capacity exhaustion is not mistaken for per-client throttling. Client states idle for more than 2 minutes are evicted by a 60-second background sweep and, when the table is full, by an on-demand scan that runs at most once per second; live states are never evicted to admit a new client. Rejected requests — including rate-limited ones — still refresh a client's liveness timestamp, so a persistently throttled client is not evicted as idle. The rate budget is charged only after a request passes body validation and the concurrency gate, so malformed or over-capacity requests never consume a client's window. Client identities are normalized with `net.IP.String()`, so an IPv6 client cannot occupy multiple limiter states via alternate textual encodings of the same address.

The `/dns-query` path is implemented by `doh-gateway`, which answers `OPTIONS` preflights (`204`) and sets `Access-Control-Allow-Origin: *` on preflights and successful DNS responses. Methods other than `GET`, `POST` and `OPTIONS` receive `405`. Blocky v0.35.0 supports DoH itself, but this project intentionally keeps every Blocky listener on loopback and serves `/dns-query` through the resource-bounded gateway instead. Blocky's loopback HTTP listener also exposes its own REST API and pprof endpoints; the gateway never proxies them, because it only ever forwards DNS messages to `/dns-query`.

## SnapDeploy deployment

1. Upload/connect this repository with the Dockerfile.
2. Use the Small container size (512 MB / 0.25 vCPU).
3. Do **not** create a `PORT` environment variable manually; SnapDeploy manages it. The image advertises port `8080`, so SnapDeploy can detect the service port.
4. No database, Redis, RabbitMQ, or other SnapDeploy add-on is required. Blocky's DNS response cache is in-memory and Blocky is otherwise stateless.
5. Add your custom domain if the service is intended for public DoH clients. SnapDeploy terminates HTTPS and routes to the container.
6. Use `/dns-query` as the DoH path.

The Docker build uses a single Go builder stage (the Small tier's two-stage limit leaves exactly one builder plus one runtime stage): it first clones and builds Blocky, then copies in the gateway sources, runs `go vet` and `go test`, and builds the gateway binary — so a failing test fails the build. Because the Blocky clone/build precedes the gateway `COPY` instructions, editing gateway sources leaves the cached Blocky layers untouched; only the later layers rebuild. That stage clones the Blocky repository at the pinned `BLOCKY_VERSION` tag (`v0.35.0`, overridable with `--build-arg BLOCKY_VERSION=<tag>`) and builds it with `make build BIN_OUT_DIR=/bin`. The build requires network access to github.com for the clone step, so restricted build environments without outbound HTTPS to GitHub fail there with a `Failed to connect to github.com port 443`-style error.

The final Alpine image contains only the gateway binary, the Blocky binary, `config.yml`, `entrypoint.sh`, `ca-certificates`, and the BusyBox `wget` used by the health check, and runs as the unprivileged `app` user — **without `CAP_NET_BIND_SERVICE`, so no listener may use a port below 1024**. `config.yml` pins Blocky to loopback ports `5353`/`8053` exactly so Blocky never binds its default `:53` (which would fail with `listen udp :53: bind: permission denied`); `entrypoint.sh` additionally refuses to start with a clear, naming error if `PORT` or any Blocky listener port in `config.yml` is privileged, instead of crash-looping at bind time. The image health check (every 30 s, 3 s timeout, 5 s start period, 3 retries) requests `/healthz` on `$PORT` (default `8080`; a leading `:` in `PORT` is tolerated).

Example endpoint after a custom domain is attached:

`https://dns.example.com/dns-query`

The `/healthz` endpoint (`GET`/`HEAD`) returns `200` only when a TCP connection to Blocky's loopback listener (`127.0.0.1:8053`) succeeds within 250 ms, otherwise `503`. This lets the container health check catch a failed DNS backend rather than reporting the gateway as ready by itself. It is a connect check only, not a DNS query. Methods other than `GET`/`HEAD` receive `405`. Other paths are intentionally rejected with `404`; the one exception is a non-canonical path such as `//dns-query` or `/a/../dns-query`, which Go's `http.ServeMux` answers with a `301` redirect to the cleaned path before routing.

## DoH handling and validation

The gateway accepts RFC-style DoH GET and POST requests on `/dns-query`:

- GET `dns=` values are decoded as unpadded base64url and forwarded internally as bounded POST requests. Padded values are rejected with `400`.
- GET query strings longer than `dns=` plus the base64url-encoded form of 8,192 bytes (10,924 characters) — 10,928 characters total — are rejected with `413` before the query string is parsed at all. This single bound also caps the `dns=` value, so no decoded message can exceed 8,192 bytes. `HEAD` and any method other than `GET`/`POST`/`OPTIONS` receives `405`.
- POST requests require `application/dns-message` and are capped at 8,192 bytes (the gateway's wire-format cap, well under the 65,535-byte DNS maximum).
- Incoming DNS messages are structurally validated before Blocky is called; exactly one DNS question is required and resource records are capped at 4,096.
- Upstream responses must be HTTP `200`, use `application/dns-message`, fit the same size bound, have valid DNS wire structure, preserve the request transaction ID, and echo the request's question section (owner name and QTYPE/QCLASS, compression-expanded).
- Backend response headers are capped at 16 KiB, response bodies at 8,192 bytes, and backend redirects are never followed.
- The gateway never forwards arbitrary upstream response headers to clients.

This validation is primarily a resource-safety and protocol-correctness guard; it does not replace DNSSEC validation or Blocky's resolver protections.

### Request order and status codes

`/dns-query` requests are checked in this order, and the first failing check decides the response: HTTP method, then the concurrency gate, then body/query decoding and size, then DNS wire-format validation, then the per-client rate limiter, and finally the backend call. The concurrency gate is intentionally before parsing so malformed or oversized request floods cannot consume unbounded CPU or memory outside the configured in-flight budget. The rate budget is charged only after DNS validation, so malformed requests do not consume a client window.

| Status | When |
|---|---|
| `200` | Valid, fully verified DNS response from Blocky (`application/dns-message`, `Cache-Control: no-store`, `Access-Control-Allow-Origin: *`). |
| `204` | `OPTIONS` preflight on `/dns-query`. |
| `400` | GET without `dns=`, padded or otherwise invalid base64url, unreadable POST body, or a message that fails DNS structural validation. |
| `405` | Method not allowed (`Allow` header is set). |
| `413` | GET query string over the 10,928-character bound, or POST body over 8,192 bytes. |
| `415` | POST whose `Content-Type` is not `application/dns-message` (parameters such as `charset` are accepted). |
| `429` | Client exceeded its sliding window; `Retry-After` is 1–60 seconds. |
| `500` | The gateway could not build the backend request. |
| `502` | Backend unreachable, or its response failed any verification check (status, content type, size, wire format, transaction ID, question echo). |
| `503` | Concurrency limit reached, or the client table is full and the client is new. Also `/healthz` when Blocky's listener does not accept a connection. |
| `504` | Backend request exceeded its 3-second deadline or 2.5-second response-header timeout. |

All gateway-generated error responses are plain text with `Cache-Control: no-store`; they carry no CORS headers.

### Known limitations

- Rate limiting is per exact client IP. IPv6 addresses are intentionally not grouped by prefix in this gateway, so a subscriber that can rotate source addresses inside a routed `/64` can obtain additional windows. The bounded client table is designed to cap memory rather than provide a perfect identity system.
- `/healthz` is public and is not covered by the rate limiter or the DoH concurrency gate. Its loopback readiness probe is cached for 2 seconds, so repeated health checks do not create a TCP connection storm.
- With `TRUST_PROXY=true`, a request that carries no valid `X-Forwarded-For` or `X-Real-IP` value is attributed to its TCP peer, which behind a load balancer is the load balancer itself.

## Important client-IP setting

`TRUST_PROXY=true` is enabled in the production image for the intended SnapDeploy edge deployment. The gateway first uses `CF-Connecting-IP`, then the first valid address in `X-Forwarded-For`, then `X-Real-IP`, and otherwise falls back to the TCP peer address. The trusted edge must overwrite/sanitize these headers before forwarding; do not enable proxy-header trust on a directly reachable port. This keeps one stable client-IP key when the platform supplies the original address.

For a direct local/container test without a trusted proxy, set `TRUST_PROXY=false`. Invalid `TRUST_PROXY` values fall back to the safe default (`false`). The Dockerfile's production `ENV` defaults to `true` because SnapDeploy's load balancer is the trusted proxy in that path.

## Shutdown behavior

On `SIGINT`/`SIGTERM`, the gateway stops accepting new connections and drains in-flight DoH requests via a graceful HTTP shutdown (up to 5 seconds) before exiting, rather than dropping active requests immediately. `entrypoint.sh` shuts down in order: it first sends `SIGTERM` to the gateway and waits for it to exit, and only then stops Blocky, so Blocky keeps answering while requests drain. If either process exits on its own, the entrypoint stops the other one and exits with the first process's status; a signal-initiated shutdown exits `0`. The entrypoint polls the two processes' exit status once per second, but does so by waiting on a background `sleep`, so `SIGINT`/`SIGTERM` are acted on immediately rather than after the current poll interval. Neither process is given a startup delay: until Blocky is listening, `/healthz` returns `503` and DNS queries return `502`.

## Configuration

Environment variables read by the gateway. Unset, empty, non-numeric, below-1 or above-cap values fall back to the default.

| Variable | Default | Allowed range | Meaning |
|---|---|---|---|
| `PORT` | `8080` | TCP port number | Public listener port (a leading `:` is accepted). The gateway itself does not range-check it; `entrypoint.sh` refuses to start if it names a privileged port (<1024), and any other unusable value makes the gateway exit at startup. |
| `RATE_LIMIT` | `99` | 1–256 | Requests per client IP per 60-second sliding window. The window length is fixed. |
| `MAX_CLIENTS` | `8192` | 1–32768 | Maximum client rate-limit states held in memory. |
| `MAX_CONCURRENT` | `8` | 1–8 | Maximum in-flight DNS requests, including body parsing and backend connections. 8 is the hard cap for this image. |
| `TRUST_PROXY` | `false` in the binary, `true` in the Docker image | `true`/`false` (case-insensitive) | Use `X-Forwarded-For`/`X-Real-IP` for client identity. |

The Dockerfile's `ENV` sets `GOMAXPROCS=1`, `GOGC=100`, `GOMEMLIMIT=160MiB`, `RATE_LIMIT=99`, `MAX_CLIENTS=8192`, `MAX_CONCURRENT=8` and `TRUST_PROXY=true`.

The Blocky backend address (`127.0.0.1:8053`) is fixed in the gateway and must match `ports.http` in `config.yml`. The Blocky config path (`/etc/blocky/config.yml`) is fixed in `entrypoint.sh`.

## Resource tuning

The default values are chosen for the Small tier:

- `GOMAXPROCS=1` — matches the Go runtime's scheduler/GC thread count to the tier's 0.25 vCPU quota instead of the host's full core count. The Dockerfile sets it for the whole container, so it applies to the Blocky process as well as the gateway.
- `MAX_CONCURRENT=8` — bounds both request parsing and backend DNS work, which is a better fit for 0.25 vCPU. Requests beyond the limit receive `503` immediately.
- `MAX_CLIENTS=8192` — hard cap on in-memory rate-limit client states; the limiter map is allocated only when the first request arrives. When the table is full, new clients receive `503` and the event is logged, while over-budget clients receive `429`. Idle states are reclaimed after 2 minutes, so this gives headroom for thousands of active client IPs without allowing unbounded map growth.
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

To run the unit tests without Docker, use `go vet ./... && go test ./...` (Go 1.27 or newer).

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
| `main.go` | DoH gateway: limiter, validation, proxying, health check. |
| `main_test.go` | Unit tests, run during `docker build`. |
| `go.mod` | Go module definition (`go 1.27`, no external dependencies). |
| `Dockerfile` | Single-builder-stage build and Alpine runtime image. |
| `entrypoint.sh` | Starts and supervises Blocky and the gateway; refuses privileged ports (<1024) up front. |
| `config.yml` | Blocky configuration. |
| `docker-compose.yml` | Local run with `TRUST_PROXY=false`. |
| `CHANGELOG.md` | Change history and code-audit notes. |
| `LICENSE` | License text. |

## Changelog

See [`CHANGELOG.md`](CHANGELOG.md) for the full change history and code-audit notes.

## 🌐 Free DNS Services

High-performance DNS utilizing HaGeZi Blocklists (Multi Pro + TIF).

| Blocklist | DNS-over-HTTPS (DoH) |
| :--- | :--- |
| Multi Pro + TIF | `https://freedns.koyeb.app/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns.mydoh.workers.dev/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns-pi.vercel.app/api/doh/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dnssix.netlify.app/api/doh/dns-query` |
| Multi Pro + TIF | `https://dns-93aca.containers.snapdeploy.app/dns-query` |
| Multi Pro + TIF | `https://doh-93aca.containers.snapdeploy.app/dns-query` |

## ⚡ Bandwidth Hero Server

A lightweight image optimization proxy designed to slash bandwidth usage and accelerate web browsing.

Bandwidth Hero Server fetches remote images, compresses them on the fly, and delivers optimized versions to the client. This significantly reduces data consumption while improving page load performance.

🖥️ **Live Demo:** [Bandwidth Hero](https://bhserv.netlify.app/).

## Supporting the Project

If you find this project useful, donations are appreciated:

- **Bitcoin**: `1HntwKxyqGCfnSGvGLMUTRAqLnTvLarAQP`

## License

See [`LICENSE`](LICENSE).
