ARG BLOCKY_VERSION=v0.35.0

# Single builder stage to stay within the Small container 2-stage limit.
FROM golang:1.27.1-alpine AS builder

RUN apk add --no-cache \
        git \
        make

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GO_SKIP_GENERATE=1 \
    GO_BUILD_FLAGS="-tags static -v"

ARG BLOCKY_VERSION

# ------------------------------------------------------------
# Build Blocky
# ------------------------------------------------------------
WORKDIR /src/blocky

RUN git clone \
        --depth 1 \
        --branch "${BLOCKY_VERSION}" \
        https://github.com/0xERR0R/blocky.git .

RUN go mod download && \
    make build BIN_OUT_DIR=/bin && \
    test -x /bin/blocky

# ------------------------------------------------------------
# Build DoH gateway
# ------------------------------------------------------------
WORKDIR /src/doh

COPY go.mod ./
COPY main.go main_test.go ./

RUN go vet ./... && \
    go test ./... && \
    mkdir -p /out && \
    go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/doh-gateway .

# Runtime files and their permissions are prepared here: COPY --from preserves
# file modes, so the runtime stage needs no chmod layer that would store a
# second copy of the large Blocky binary.
COPY config.yml entrypoint.sh /out/

RUN sh -n /out/entrypoint.sh && \
    chmod 0555 /out/doh-gateway /out/entrypoint.sh /bin/blocky && \
    chmod 0444 /out/config.yml

# ------------------------------------------------------------
# Runtime
# ------------------------------------------------------------
FROM alpine:3.24

RUN apk add --no-cache ca-certificates && \
    addgroup -S app && \
    adduser -S -G app -H app

COPY --from=builder /out/doh-gateway /doh-gateway
COPY --from=builder /bin/blocky /blocky
COPY --from=builder /out/config.yml /etc/blocky/config.yml
COPY --from=builder /out/entrypoint.sh /entrypoint.sh

EXPOSE 8080

ENV GOMAXPROCS=1 \
    GOMEMLIMIT=160MiB \
    RATE_LIMIT=99 \
    MAX_CLIENTS=8192 \
    MAX_CONCURRENT=8 \
    TRUST_PROXY=true

HEALTHCHECK --interval=30s \
    --timeout=3s \
    --start-period=5s \
    --retries=3 \
    CMD p="${PORT:-8080}"; \
        wget -q -O /dev/null "http://127.0.0.1:${p#:}/healthz" || exit 1

USER app:app

ENTRYPOINT ["/entrypoint.sh"]
