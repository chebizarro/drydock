# LSP Bridge �� multi-language server manager
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

# Base tools
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates wget curl git \
    && rm -rf /var/lib/apt/lists/*

# Go + gopls
RUN wget -qO- https://go.dev/dl/go1.23.6.linux-amd64.tar.gz | tar -xz -C /usr/local
ENV PATH="/usr/local/go/bin:/root/go/bin:${PATH}"
RUN go install golang.org/x/tools/gopls@latest

# Python + pylsp
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-pip python3-venv \
    && rm -rf /var/lib/apt/lists/* \
    && pip3 install --no-cache-dir python-lsp-server

# Node.js + typescript-language-server
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g typescript typescript-language-server \
    && rm -rf /var/lib/apt/lists/*

# clangd (C/C++)
RUN apt-get update && apt-get install -y --no-install-recommends \
    clangd-14 \
    && ln -sf /usr/bin/clangd-14 /usr/bin/clangd \
    && rm -rf /var/lib/apt/lists/*

# rust-analyzer (Rust)
RUN curl -L https://github.com/rust-lang/rust-analyzer/releases/latest/download/rust-analyzer-x86_64-unknown-linux-gnu.gz \
    | gunzip > /usr/local/bin/rust-analyzer && chmod +x /usr/local/bin/rust-analyzer

WORKDIR /app

COPY --from=builder /out/lsp-bridge /usr/local/bin/lsp-bridge

ENV LSP_BRIDGE_ADDR=:8082

HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -qO- http://localhost:8082/healthz || exit 1

EXPOSE 8082

ENTRYPOINT ["lsp-bridge", "-addr", ":8082"]
