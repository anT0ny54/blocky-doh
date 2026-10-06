ARG BLOCKY_VERSION=v0.35.0
FROM golang:1.27.1-alpine AS builder
RUN apk add --no-cache \
        coreutils \
        git \
        libcap \
        make
ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GO_SKIP_GENERATE=1 \
    GO_BUILD_FLAGS="-tags static -v"
WORKDIR /src/doh
COPY go.mod ./
COPY main.go main_test.go ./
RUN go vet ./... && \
    go test ./... && \
    go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/doh-gateway .
ARG BLOCKY_VERSION
WORKDIR /src/blocky
RUN git clone \
        --depth 1 \
        --branch "${BLOCKY_VERSION}" \
        https://github.com/0xERR0R/blocky.git .
RUN go mod download && \
    make build BIN_OUT_DIR=/bin && \
    test -x /bin/blocky
FROM alpine:3.24
RUN apk add --no-cache ca-certificates && \
    addgroup -S app && \
    adduser -S -G app -H app
COPY --from=builder /out/doh-gateway /doh-gateway
COPY --from=builder /bin/blocky /blocky
COPY config.yml /etc/blocky/config.yml
COPY entrypoint.sh /entrypoint.sh
RUN chmod 0555 \
        /doh-gateway \
        /blocky \
        /entrypoint.sh && \
    chmod 0444 /etc/blocky/config.yml
EXPOSE 8080
ENV GOMAXPROCS=1 \
    RATE_LIMIT=99 \
    MAX_CLIENTS=256 \
    MAX_CONCURRENT=16 \
    TRUST_PROXY=true \
    BLOCKY_CONFIG_FILE=/etc/blocky/config.yml
HEALTHCHECK --interval=30s \
    --timeout=3s \
    --start-period=5s \
    --retries=3 \
    CMD wget -q -O /dev/null \
        "http://127.0.0.1:${PORT:-8080}/healthz" || exit 1
USER app:app
ENTRYPOINT ["/entrypoint.sh"]
