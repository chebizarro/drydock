# LSP Bridge — multi-language server manager
#
# This image is large (~2-4 GB) because it bundles language servers for
# Go, Python, TypeScript/JavaScript, Rust, and C/C++. It runs separately
# from the lean drydock-core image (~20 MB) under the "lsp" Docker profile.
#
# Build: docker build -t drydock-lsp-bridge -f docker/lsp-bridge/Dockerfile .
# Run:   docker compose --profile lsp up -d

# --- Build stage: compile lsp-bridge binary ---
FROM golang:1.26-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/lsp-bridge ./cmd/lsp-bridge

# --- Runtime stage: language servers + bridge binary ---
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# Pinned language-server versions so two builds of the same commit produce the
# same image. Bump these deliberately, the way the core Dockerfile pins
# BETTERLEAKS_VERSION.
ARG GOPLS_VERSION=v0.23.0
ARG NODE_VERSION=20.19.4
ARG TYPESCRIPT_VERSION=5.7.3
ARG TYPESCRIPT_LANGUAGE_SERVER_VERSION=6.0.0
ARG PYTHON_LSP_SERVER_VERSION=1.15.0
ARG RUST_ANALYZER_VERSION=2026-08-24

# Base tools + clangd + Python runtime in a single layer.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates wget curl git xz-utils \
    python3 python3-pip python3-venv \
    clangd-14 \
    && ln -sf /usr/bin/clangd-14 /usr/bin/clangd \
    && rm -rf /var/lib/apt/lists/*

# Non-root user (mirrors the core Dockerfile's `adduser -D -u 1000 drydock`).
RUN useradd --uid 1000 --create-home --shell /bin/bash drydock

# Go toolchain from the builder — a single toolchain matching go.mod (go 1.26)
# so gopls is built by the same compiler that parses the modules it serves.
# gopls also shells out to `go` at runtime, so it must stay on PATH.
COPY --from=builder /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
RUN GOTOOLCHAIN=local GOBIN=/usr/local/bin GOPATH=/tmp/go \
    go install golang.org/x/tools/gopls@${GOPLS_VERSION} \
    && rm -rf /tmp/go

# Node.js + typescript-language-server from a pinned tarball (no piping a
# remote script into a root shell).
RUN wget -qO- "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
    | tar -xJ -C /usr/local --strip-components=1 \
    && npm install -g "typescript@${TYPESCRIPT_VERSION}" "typescript-language-server@${TYPESCRIPT_LANGUAGE_SERVER_VERSION}" \
    && npm cache clean --force

# Python language server (pinned).
RUN pip3 install --no-cache-dir "python-lsp-server==${PYTHON_LSP_SERVER_VERSION}"

# rust-analyzer (pinned release instead of releases/latest).
RUN curl -fsSL "https://github.com/rust-lang/rust-analyzer/releases/download/${RUST_ANALYZER_VERSION}/rust-analyzer-x86_64-unknown-linux-gnu.gz" \
    | gunzip > /usr/local/bin/rust-analyzer && chmod +x /usr/local/bin/rust-analyzer

WORKDIR /app

COPY --from=builder /out/lsp-bridge /usr/local/bin/lsp-bridge

ENV LSP_BRIDGE_ADDR=0.0.0.0:8082

# Run the bridge and every language server it spawns as an unprivileged user;
# they execute against attacker-supplied repository contents.
USER drydock

HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -qO- http://localhost:8082/healthz || exit 1

EXPOSE 8082

ENTRYPOINT ["lsp-bridge", "-addr", "0.0.0.0:8082"]
