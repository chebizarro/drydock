FROM golang:1.26-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git ca-certificates build-base

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETARCH=amd64
RUN CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/drydock ./cmd/drydock && \
    CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/drydock-eval ./cmd/drydock-eval

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata git bash ripgrep
WORKDIR /app

COPY --from=builder /out/drydock /usr/local/bin/drydock
COPY --from=builder /out/drydock-eval /usr/local/bin/drydock-eval
COPY eval /app/eval
COPY scripts/entrypoint.sh /entrypoint.sh

RUN chmod +x /entrypoint.sh && \
    mkdir -p /data/repos && \
    adduser -D -u 1000 drydock && \
    chown -R drydock:drydock /data /app

USER drydock

ENV DRYDOCK_DATABASE_URL=file:/data/drydock.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)
ENV DRYDOCK_REPO_CACHE_DIR=/data/repos
ENV DRYDOCK_EVAL_DATASET_PATH=/app/eval/heldout-sample.json
ENV DRYDOCK_MODE=listener
ENV DRYDOCK_HEALTH_ADDR=:8081

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://localhost:8081/readyz || exit 1

EXPOSE 8081

ENTRYPOINT ["/entrypoint.sh"]
