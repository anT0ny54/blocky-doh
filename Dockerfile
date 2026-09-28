# Use Blocky's published v0.25 image instead of cloning GitHub during the build.
# This removes the build-time dependency on outbound access to github.com.
FROM spx01/blocky:v0.25 AS blocky

# Builder: exact Go toolchain for the small DNS gateway.
FROM golang:1.23.2-alpine AS builder

ENV CGO_ENABLED=0 \
    GO111MODULE=on \
    GOTOOLCHAIN=local
WORKDIR /src

# The gateway only uses the Go standard library, so no source dependency
# downloads are required during the build.
COPY go.mod ./
COPY main.go main_test.go ./
RUN go test ./...
RUN go build -trimpath -ldflags='-s -w' -o /out/doh-gateway .

# Blocky is copied from the published multi-arch image above; no source checkout is needed.
COPY --from=blocky /app/blocky /out/blocky

# Final runtime: Alpine 3.24, no Go toolchain kept in the image.
FROM alpine:3.24

# wget is used by the health check; ca-certificates are needed for HTTPS upstreams.
RUN apk add --no-cache ca-certificates wget && \
    addgroup -S app && adduser -S -G app -H app && \
    mkdir -p /etc/blocky && chown -R app:app /etc/blocky

COPY --from=builder /out/doh-gateway /doh-gateway
COPY --from=builder /out/blocky /blocky
COPY config.yml /etc/blocky/config.yml
COPY entrypoint.sh /entrypoint.sh

RUN chmod 0555 /doh-gateway /blocky /entrypoint.sh && chmod 0444 /etc/blocky/config.yml

# SnapDeploy detects this public container port and manages PORT at runtime.
EXPOSE 8080

# Small, memory-conscious process settings. The app reads PORT from SnapDeploy.
# GOMAXPROCS is pinned to the tier's 0.25 vCPU quota.
ENV GOGC=100 \
    GOMAXPROCS=1 \
    RATE_LIMIT=99 \
    MAX_CLIENTS=131072 \
    MAX_CONCURRENT=256 \
    TRUST_PROXY=true

# Readiness is tied to the local Blocky-backed gateway.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PORT:-8080}/healthz" || exit 1

USER app:app
ENTRYPOINT ["/entrypoint.sh"]
