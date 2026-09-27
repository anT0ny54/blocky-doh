# Builder: exact current supported Go toolchain for the DNS gateway and Blocky 0.25 build.
FROM golang:1.23.2-alpine AS builder

ARG BLOCKY_VERSION=v0.25
ENV CGO_ENABLED=0 \
    GO111MODULE=on \
    GOTOOLCHAIN=local

RUN apk add --no-cache ca-certificates git
WORKDIR /src

# Build the small DNS-only gateway.
COPY go.mod ./
COPY main.go main_test.go ./
RUN go test ./...
RUN go build -trimpath -ldflags='-s -w' -o /out/doh-gateway .

# Build the requested Blocky release from its immutable tag.
RUN go install -trimpath -ldflags='-s -w' github.com/0xERR0R/blocky@${BLOCKY_VERSION}

# Final runtime: Alpine 3.24, no Go toolchain kept in the image.
FROM alpine:3.24

RUN apk add --no-cache ca-certificates && \
    addgroup -S app && adduser -S -G app -H app && \
    mkdir -p /etc/blocky && chown -R app:app /etc/blocky

COPY --from=builder /out/doh-gateway /doh-gateway
COPY --from=builder /go/bin/blocky /blocky
COPY config.yml /etc/blocky/config.yml
COPY entrypoint.sh /entrypoint.sh

RUN chmod 0555 /doh-gateway /blocky /entrypoint.sh && chmod 0444 /etc/blocky/config.yml

# SnapDeploy detects this public container port and manages PORT at runtime.
EXPOSE 8080

# Small, memory-conscious process settings. The app reads PORT from SnapDeploy.
ENV GOGC=100 \
    RATE_LIMIT=99 \
    MAX_CLIENTS=131072 \
    MAX_CONCURRENT=256 \
    TRUST_PROXY=true

# Give the platform a cheap readiness signal tied to the local Blocky listener.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD-SHELL wget -q -O /dev/null "http://127.0.0.1:${PORT:-8080}/healthz" || exit 1

USER app:app
ENTRYPOINT ["/entrypoint.sh"]
