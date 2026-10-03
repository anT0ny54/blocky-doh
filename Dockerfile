# Builder: pinned Go toolchain; compiles and tests the gateway
FROM golang:1.27.1-alpine AS builder

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local

WORKDIR /src

# Go gateway source
COPY go.mod ./
COPY main.go main_test.go ./

# Standard-library-only gateway: no dependency downloads required.
RUN go vet ./... && \
    go test ./... && \
    go build -trimpath -ldflags='-s -w' -o /out/doh-gateway .

# Final runtime: no Go toolchain kept in the image.
FROM alpine:3.24

# ca-certificates are needed for HTTPS upstreams. The health check uses BusyBox
# wget, which is already part of the Alpine base image.
RUN apk add --no-cache ca-certificates && \
    addgroup -S app && \
    adduser -S -G app -H app

COPY --from=builder /out/doh-gateway /doh-gateway
# Take the official latest stable Blocky release (v0.35.0 as of 2026-09-29)
# directly from its published image instead of cloning/building from source.
COPY --from=ghcr.io/0xerr0r/blocky:v0.35.0 /app/blocky /blocky
COPY config.yml /etc/blocky/config.yml
COPY entrypoint.sh /entrypoint.sh

RUN chmod 0555 /doh-gateway /blocky /entrypoint.sh && \
    chmod 0444 /etc/blocky/config.yml

EXPOSE 8080

# Small, memory-conscious process settings. GOGC intentionally left at the Go
# default; MAX_CLIENTS/MAX_CONCURRENT/RATE_LIMIT mirror the gateway defaults.
ENV GOMAXPROCS=1 \
    RATE_LIMIT=99 \
    MAX_CLIENTS=256 \
    MAX_CONCURRENT=16 \
    TRUST_PROXY=true

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PORT:-8080}/healthz" || exit 1

USER app:app
ENTRYPOINT ["/entrypoint.sh"]
