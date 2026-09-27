# HaGeZi DoH — SnapDeploy Small (512 MB / 0.25 vCPU)

Minimal public DNS-over-HTTPS service using:

- Alpine Linux 3.24 runtime
- Go 1.23.2 build toolchain
- Blocky v0.25
- HaGeZi DoH upstreams
- DNS-only Go HTTP gateway for the exact **99 requests / 60 seconds / client IP** rule
- Public listener on `$PORT` (local default: `8080`); no listener is configured on the commonly used Blocky API port

## Architecture

```text
Internet / SnapDeploy HTTPS
          |
          v
  doh-gateway :$PORT
     |       |
     |       +-- per-client-IP limiter: 99 / 60s
     |       +-- max concurrent DNS requests: 256
     |
     v
 Blocky 0.25
 127.0.0.1:8053
     |
     +-- https://root.hagezi.org/dns-query
     +-- https://wurzn.hagezi.org/dns-query
     +-- https://juuri.hagezi.org/dns-query
```

Blocky 0.25 does not provide the newer `rateLimit` configuration, so the rate limiter is deliberately kept outside Blocky while the DNS engine remains exactly v0.25.

## SnapDeploy deployment

1. Upload/connect this repository with the Dockerfile.
2. Use the Small container size (512 MB / 0.25 vCPU).
3. Do **not** create a `PORT` environment variable manually; SnapDeploy manages it. The image advertises port `8080`, so SnapDeploy can detect the service port.
4. Add your custom domain if the service is intended for public DoH clients. SnapDeploy terminates HTTPS and routes to the container.
5. Use `/dns-query` as the DoH path.

Example endpoint after a custom domain is attached:

`https://dns.example.com/dns-query`

The local `/healthz` endpoint returns `ok` for the platform health check. Other paths are intentionally rejected.

## Important client-IP setting

`TRUST_PROXY=true` is enabled because SnapDeploy routes traffic through its managed load balancer. The gateway uses the rightmost valid address in `X-Forwarded-For`, then `X-Real-IP`, and otherwise falls back to the TCP peer address. This matches the usual append-style proxy chain and avoids trusting a client-prepended spoofed address.

For a direct local/container test without a trusted proxy, set `TRUST_PROXY=false`.

## Resource tuning

The default values are chosen for the Small tier:

- `MAX_CONCURRENT=256` — bounds in-flight DNS work and prevents request floods from consuming all memory/CPU.
- `MAX_CLIENTS=131072` — hard cap on in-memory rate-limit client states.
- `caching.maxItemsCount=65536` — bounded Blocky cache; Blocky documents this option specifically as useful on systems with limited RAM.
- `upstreams.strategy=random` — one upstream request per cache miss in the normal path; this avoids the extra upstream fan-out of `parallel_best` and is a better fit for 0.25 vCPU.
- `connectIPVersion=v4` — avoids unnecessary IPv6 connection attempts because the supplied upstream information is IPv4-based.
- Query logging, statistics, Prometheus, API, prefetching, and blocklists are disabled.

These are capacity-oriented defaults, not a guarantee of a fixed users-per-second number. Real capacity depends heavily on cache hit rate, DNS response sizes, upstream latency, and the traffic pattern.

## Local test

```sh
docker compose up --build
```

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

## Upstream inputs

| Name | Endpoint | IPv4 supplied |
|---|---|---|
| HaGeZiDNS1 | `https://root.hagezi.org/dns-query` | `188.34.161.210` |
| HaGeZiDNS2 | `https://wurzn.hagezi.org/dns-query` | `159.69.155.94` |
| HaGeZiDNS3 | `https://juuri.hagezi.org/dns-query` | `95.217.163.17` |

The service keeps the upstream URLs by hostname so TLS certificate/SNI handling remains normal; it does not hard-code the supplied IP addresses into the HTTPS URLs.

## SnapDeploy policy note

This project is a DNS resolver/DoH service, not a general-purpose HTTP/SOCKS proxy, tunnel, VPN panel, or remote shell. SnapDeploy currently documents restrictions on proxies/tunnels and separately documents a shared-domain per-IP request limit; a custom domain is not subject to that shared-domain limit.
