# Blocky + DoH gateway for SnapDeploy

Minimal public DNS-over-HTTPS service using:

- Alpine Linux 3.24 runtime
- Go 1.27.1 build toolchain
- Blocky v0.35.0
- HaGeZi DoH upstreams
- DNS-only Go HTTP gateway for the exact **99 requests / 60 seconds / client IP** rule
- Public listener on `$PORT` (local default: `8080`)
- Private Blocky HTTP listener on `127.0.0.1:8053`; no public Blocky DNS/TLS/HTTPS listener

## Architecture

```text
Internet / SnapDeploy HTTPS
          |
          v
  doh-gateway :$PORT
     |       |
     |       +-- per-client-IP limiter: 99 / 60s strict sliding window
     |       +-- max concurrent DNS requests: 16 (default and hard cap)
     |       +-- DNS wire-format validation
     |
     v
 Blocky 0.35.0
 127.0.0.1:8053
     |
     +-- HTTPS to HaGeZi DoH upstreams
```

The exact **99 requests / 60 seconds / client IP** policy is deliberately kept in the gateway rather than delegated to Blocky, so the policy remains independent of Blocky release changes.

The limiter is a **strict sliding window**: at most 99 requests per client IP fall inside any rolling 60-second span. There is no token refill — after a full 99-request burst, further requests are admitted only as earlier timestamps age out of the window. Two rejection modes are distinguished: an over-budget client receives `429` with a `Retry-After` header holding the whole seconds until its oldest request leaves the window (at most 60), while a new client arriving when the fixed client table is full receives `503`, so capacity exhaustion is not mistaken for per-client throttling. Client states idle for more than 2 minutes are evicted by a 60-second background sweep and, when the table is full, by an on-demand scan that runs at most once per second; live states are never evicted to admit a new client. Client identities are normalized with `net.IP.String()`, so an IPv6 client cannot occupy multiple limiter states via alternate textual encodings of the same address.

The `/dns-query` path is implemented by `doh-gateway`, which answers `OPTIONS` preflights (`204`) and sets `Access-Control-Allow-Origin: *` on preflights and successful DNS responses. Methods other than `GET`, `POST` and `OPTIONS` receive `405`. Blocky v0.35.0 supports DoH itself, but this project intentionally keeps Blocky's public-facing listeners disabled and serves `/dns-query` through the resource-bounded gateway instead.

## SnapDeploy deployment

1. Upload/connect this repository with the Dockerfile.
2. Use the Small container size (512 MB / 0.25 vCPU).
3. Do **not** create a `PORT` environment variable manually; SnapDeploy manages it. The image advertises port `8080`, so SnapDeploy can detect the service port.
4. No database, Redis, RabbitMQ, or other SnapDeploy add-on is required. Blocky's DNS response cache is in-memory and Blocky is otherwise stateless.
5. Add your custom domain if the service is intended for public DoH clients. SnapDeploy terminates HTTPS and routes to the container.
6. Use `/dns-query` as the DoH path.

The Docker build compiles the gateway in a Go builder stage and runs `go vet` and `go test` there, so a failing test fails the build. The final Alpine image contains only the gateway binary, the Blocky binary, `config.yml`, `entrypoint.sh`, `ca-certificates`, and the BusyBox `wget` used by the health check.

The Dockerfile intentionally does **not** run `git clone` or download Blocky source from GitHub during the build. It copies the Blocky v0.35.0 binary from the official `ghcr.io/0xerr0r/blocky:v0.35.0` image instead. This avoids the `git clone ... Failed to connect to github.com port 443` failure seen in restricted build environments.

Example endpoint after a custom domain is attached:

`https://dns.example.com/dns-query`

The `/healthz` endpoint (`GET`/`HEAD`) returns `200` only when a TCP connection to Blocky's loopback listener (`127.0.0.1:8053`) succeeds within 250 ms, otherwise `503`. This lets the container health check catch a failed DNS backend rather than reporting the gateway as ready by itself. It is a connect check only, not a DNS query. Other paths are intentionally rejected with `404`.

## DoH handling and validation

The gateway accepts RFC-style DoH GET and POST requests on `/dns-query`:

- GET `dns=` values are decoded as unpadded base64url (values longer than the encoded form of 65,535 bytes are rejected with `413` before decoding) and forwarded internally as bounded POST requests.
- POST requests require `application/dns-message` and are capped at the 65,535-byte DNS wire-format maximum.
- Incoming DNS messages are structurally validated before Blocky is called; exactly one DNS question is required and resource records are capped at 4,096.
- Upstream responses must be HTTP `200`, use `application/dns-message`, fit the same size bound, have valid DNS wire structure, preserve the request transaction ID, and echo the request's question section (owner name and QTYPE/QCLASS, compression-expanded).
- Backend response headers are capped at 16 KiB, response bodies at 65,535 bytes, and backend redirects are never followed.
- The gateway never forwards arbitrary upstream response headers to clients.

This validation is primarily a resource-safety and protocol-correctness guard; it does not replace DNSSEC validation or Blocky's resolver protections.

## Important client-IP setting

`TRUST_PROXY=true` is enabled because SnapDeploy routes traffic through its managed load balancer. The gateway uses the rightmost valid address in `X-Forwarded-For`, then `X-Real-IP`, and otherwise falls back to the TCP peer address. This matches the usual append-style proxy chain and avoids trusting a client-prepended spoofed address.

For a direct local/container test without a trusted proxy, set `TRUST_PROXY=false`. Invalid `TRUST_PROXY` values fall back to the safe default (`false`). The Dockerfile's production `ENV` defaults to `true` because SnapDeploy's load balancer is the trusted proxy in that path.

## Shutdown behavior

On `SIGINT`/`SIGTERM`, the gateway stops accepting new connections and drains in-flight DoH requests via a graceful HTTP shutdown (up to 5 seconds) before exiting, rather than dropping active requests immediately. `entrypoint.sh` shuts down in order: it first sends `SIGTERM` to the gateway and waits for it to exit, and only then stops Blocky, so Blocky keeps answering while requests drain. If either process exits on its own, the entrypoint stops the other one and exits with the first process's status; a signal-initiated shutdown exits `0`. Neither process is given a startup delay: until Blocky is listening, `/healthz` returns `503` and DNS queries return `502`.

## Configuration

Environment variables read by the gateway. Unset, empty, non-numeric, below-1 or above-cap values fall back to the default.

| Variable | Default | Allowed range | Meaning |
|---|---|---|---|
| `PORT` | `8080` | any | Public listener port (a leading `:` is accepted). |
| `RATE_LIMIT` | `99` | 1–256 | Requests per client IP per 60-second sliding window. The window length is fixed. |
| `MAX_CLIENTS` | `256` | 1–1024 | Maximum client rate-limit states held in memory. |
| `MAX_CONCURRENT` | `16` | 1–16 | Maximum in-flight DNS requests and backend connections. 16 is both the default and the hard cap, so the value can only be lowered. |
| `TRUST_PROXY` | `false` in the binary, `true` in the Docker image | `true`/`false` (case-insensitive) | Use `X-Forwarded-For`/`X-Real-IP` for client identity. |

The Blocky backend address (`127.0.0.1:8053`) is fixed in the gateway and must match `ports.http` in `config.yml`.

## Resource tuning

The default values are chosen for the Small tier:

- `GOMAXPROCS=1` — matches the Go runtime's scheduler/GC thread count to the tier's 0.25 vCPU quota instead of the host's full core count. The Dockerfile sets it for the whole container, so it applies to the Blocky process as well as the gateway.
- `MAX_CONCURRENT=16` — bounds in-flight DNS work and prevents request floods from consuming all memory/CPU. Requests beyond the limit receive `503` immediately.
- `MAX_CLIENTS=256` — hard cap on in-memory rate-limit client states; the limiter map is allocated only when the first request arrives. When the table is full, new clients receive `503` and the event is logged, while over-budget clients receive `429`. Because idle states are only reclaimed after 2 minutes, the service can track at most `MAX_CLIENTS` distinct client IPs per 2-minute period; raise it (up to 1024) for busier deployments.
- `caching.maxItemsCount=32768` — bounded Blocky cache; Blocky documents this option specifically as useful on systems with limited RAM. `caching.cacheTimeNegative=5m` caches negative answers for five minutes.
- `upstreams.strategy=random` — one upstream request per cache miss in the normal path; this avoids the extra upstream fan-out of `parallel_best` and is a better fit for 0.25 vCPU.
- `upstreams.timeout=1200ms` — keeps each failed upstream attempt below the gateway's 3-second request deadline and leaves room for fallback.
- Backend HTTP response headers are capped at 16 KiB and redirects are disabled to keep the loopback-only backend path bounded and non-redirecting. The gateway's backend request deadline is 3 seconds, with a 2.5-second response-header timeout; both timeouts surface as `504`, while other backend failures surface as `502`.
- `connectIPVersion=v4` — avoids unnecessary IPv6 connection attempts for the supplied upstream configuration.
- Query logging, statistics, Prometheus, API, prefetching, and blocklists are disabled.
- Gateway observability is limited to startup, shutdown-drain, backend transport failures (connection errors and timeouts, `502`/`504`), and rate-limiter capacity messages on stdout. The last two are throttled to at most one line per 10 seconds each, with a count of suppressed messages appended, so request floods cannot turn logging into a resource drain. Invalid-but-delivered upstream responses (`502`) and client disconnects are not logged.

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

`docker-compose.yml` pins `TRUST_PROXY=false` for the same reason: a plain local compose setup has no trusted load balancer in front, so forwarded headers must be ignored.

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

## 🌐 Free DNS Services

Public endpoints run by the project author. Filtering (HaGeZi blocklists) is performed by the upstream resolvers or by the respective hosting setup, **not** by this gateway or by this repository's Blocky configuration, which has blocklists disabled. Availability of these hosted endpoints is outside the scope of this repository.

| Blocklist | DNS-over-HTTPS (DoH) |
| :--- | :--- |
| Multi Pro + TIF | `https://freedns.koyeb.app/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns-pi.vercel.app/api/doh/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dnssix.netlify.app/api/doh/dns-query` |
| Multi Pro + TIF | `https://dns-93aca.containers.snapdeploy.app/dns-query` (Recommended, but will sleep if not used in 15 minutes) |
| Multi Pro + TIF | `https://doh-93aca.containers.snapdeploy.app/dns-query` (Recommended, but will sleep if not used in 15 minutes) |

## Related project

[Bandwidth Hero Server](https://bhserv.netlify.app/) is a separate image-compression proxy by the same author. It is not part of this repository and shares no code with it.

## Supporting the Project

If you find this project useful, donations are appreciated:

- **Bitcoin**: `1HntwKxyqGCfnSGvGLMUTRAqLnTvLarAQP`

## License

See [`LICENSE`](LICENSE).
