# Blocky + DoH gateway for SnapDeploy

Minimal public DNS-over-HTTPS service using:

- Alpine Linux 3.24 runtime
- Go 1.23.2 build toolchain
- Blocky v0.25
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
     |       +-- max concurrent DNS requests: 128
     |       +-- DNS wire-format validation
     |
     v
 Blocky 0.25
 127.0.0.1:8053
     |
     +-- HTTPS to HaGeZi DoH upstreams
```

Blocky 0.25 does not provide the newer `rateLimit` configuration, so the rate limiter is deliberately kept outside Blocky while the DNS engine remains exactly v0.25.

The limiter is a **strict sliding window**: at most 99 requests per client IP fall inside any rolling 60-second span. There is no token refill — after a full 99-request burst, further requests are admitted only as earlier timestamps age out of the window. Two rejection modes are distinguished: an over-budget client receives `429`, while a new client arriving when the fixed client table is full receives `503`, so capacity exhaustion is not mistaken for per-client throttling. Client identities are normalized with `net.IP.String()`, so an IPv6 client cannot occupy multiple limiter states via alternate textual encodings of the same address.

The `/dns-query` path is implemented by `doh-gateway`. Do not add `ports.dohPath` to this Blocky 0.25 configuration; that field is not part of the v0.25 `ports` schema.

## SnapDeploy deployment

1. Upload/connect this repository with the Dockerfile.
2. Use the Small container size (512 MB / 0.25 vCPU).
3. Do **not** create a `PORT` environment variable manually; SnapDeploy manages it. The image advertises port `8080`, so SnapDeploy can detect the service port.
4. No database, Redis, RabbitMQ, or other SnapDeploy add-on is required. Blocky's DNS response cache is in-memory and Blocky is otherwise stateless.
5. Add your custom domain if the service is intended for public DoH clients. SnapDeploy terminates HTTPS and routes to the container.
6. Use `/dns-query` as the DoH path.

The Dockerfile intentionally does **not** run `git clone` or download Blocky source from GitHub during the build. It copies the Blocky v0.25 binary from the published `spx01/blocky:v0.25` image instead. This avoids the `git clone ... Failed to connect to github.com port 443` failure seen in restricted build environments.

Example endpoint after a custom domain is attached:

`https://dns.example.com/dns-query`

The `/healthz` endpoint returns `200` only when Blocky's local HTTP listener is reachable. This lets the container health check catch a failed DNS backend rather than reporting the gateway as ready by itself. Other paths are intentionally rejected.

## DoH handling and validation

The gateway accepts RFC-style DoH GET and POST requests on `/dns-query`:

- GET `dns=` values are decoded as unpadded base64url and forwarded internally as bounded POST requests.
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

On `SIGINT`/`SIGTERM`, the gateway stops accepting new connections and drains in-flight DoH requests via a graceful HTTP shutdown (up to 5 seconds) before exiting, rather than dropping active requests immediately. `entrypoint.sh` sends this signal to both processes and waits for them to exit.

## Resource tuning

The default values are chosen for the Small tier:

- `GOMAXPROCS=1` — matches the Go runtime's scheduler/GC thread count to the tier's 0.25 vCPU quota instead of the host's full core count.
- `MAX_CONCURRENT=128` — bounds in-flight DNS work and prevents request floods from consuming all memory/CPU.
- `MAX_CLIENTS=64` — hard cap on in-memory rate-limit client states; the limiter map is allocated only when the first request arrives. Values below 1 fall back to the default. When the table is full, new clients receive `503` and the event is logged, while over-budget clients receive `429`.
- `caching.maxItemsCount=65536` — bounded Blocky cache; Blocky documents this option specifically as useful on systems with limited RAM.
- `upstreams.strategy=random` — one upstream request per cache miss in the normal path; this avoids the extra upstream fan-out of `parallel_best` and is a better fit for 0.25 vCPU.
- `upstreams.timeout=1200ms` — keeps each failed upstream attempt below the gateway's 3-second request deadline and leaves room for fallback.
- Backend HTTP response headers are capped at 16 KiB and redirects are disabled to keep the loopback-only backend path bounded and non-redirecting.
- `connectIPVersion=v4` — avoids unnecessary IPv6 connection attempts for the supplied upstream configuration.
- Query logging, statistics, Prometheus, API, prefetching, and blocklists are disabled.
- Gateway observability is limited to startup, upstream failure (`502`/`504`), and rate-limiter capacity messages on stdout, so request floods cannot turn logging into a resource drain.

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

High-performance DNS utilizing HaGeZi Blocklists (Multi Pro + TIF).

| Blocklist | DNS-over-HTTPS (DoH) |
| :--- | :--- |
| Multi Pro + TIF | `https://freedns.koyeb.app/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns-pi.vercel.app/api/doh/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dnssix.netlify.app/api/doh/dns-query` |
| Multi Pro + TIF | `https://dns-93aca.containers.snapdeploy.app/dns-query` (Recommended, but will sleep if not used in 15 minutes) |
| Multi Pro + TIF | `https://doh-93aca.containers.snapdeploy.app/dns-query` (Recommended, but will sleep if not used in 15 minutes) |

## ⚡ Bandwidth Hero Server

A lightweight image optimization proxy designed to slash bandwidth usage and accelerate web browsing.

Bandwidth Hero Server fetches remote images, compresses them on the fly, and delivers optimized versions to the client. This significantly reduces data consumption while improving page load performance.

🖥️ **Live Demo:** [Bandwidth Hero](https://bhserv.netlify.app/).

## Supporting the Project

If you find this project useful, donations are appreciated:

- **Bitcoin**: `1HntwKxyqGCfnSGvGLMUTRAqLnTvLarAQP`

## License

See [`LICENSE`](LICENSE).
