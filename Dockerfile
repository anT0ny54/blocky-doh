# Builder: exact Go toolchain + Blocky binary source
FROM golang:1.23.2-alpine AS builder

ENV CGO_ENABLED=0 \
    GO111MODULE=on \
    GOTOOLCHAIN=local

WORKDIR /src

# Go gateway source
COPY go.mod ./
COPY main.go main_test.go ./

# Standard-library-only gateway: no dependency downloads required.
RUN go vet ./... && \
    go test ./... && \
    go build -trimpath -ldflags='-s -w' -o /out/doh-gateway .

# Pull the published Blocky binary directly from its image.
COPY --from=spx01/blocky:v0.25 /app/blocky /out/blocky


# Final runtime: no Go toolchain kept in the image.
FROM alpine:3.24

# wget is used by the health check; ca-certificates are needed for HTTPS upstreams.
RUN apk add --no-cache ca-certificates wget && \
    addgroup -S app && \
    adduser -S -G app -H app && \
    mkdir -p /etc/blocky && \
    chown -R app:app /etc/blocky

COPY --from=builder /out/doh-gateway /doh-gateway
COPY --from=builder /out/blocky /blocky
COPY config.yml /etc/blocky/config.yml
COPY entrypoint.sh /entrypoint.sh

RUN chmod 0555 /doh-gateway /blocky /entrypoint.sh && \
    chmod 0444 /etc/blocky/config.yml

EXPOSE 8080

# Small, memory-conscious process settings. GOGC intentionally left at the Go
# default; MAX_CLIENTS/MAX_CONCURRENT/RATE_LIMIT mirror the gateway defaults.
ENV GOMAXPROCS=1 \
    RATE_LIMIT=99 \
    MAX_CLIENTS=64 \
    MAX_CONCURRENT=128 \
    TRUST_PROXY=true

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PORT:-8080}/healthz" || exit 1

USER app:app
ENTRYPOINT ["/entrypoint.sh"]
