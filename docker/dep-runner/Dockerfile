# Dependency-upgrade runner — package-manager toolchain sandbox
#
# This image is large because it bundles four ecosystem toolchains (Go, Node/npm,
# Rust/cargo, Python/pip). It runs separately from the lean drydock-core image
# under the "depupgrade" Docker profile, so drydock itself ships no toolchains
# and gains no host privilege. It executes package-manager commands against
# UNTRUSTED repository manifests, so it runs as an unprivileged user and blocks
# lifecycle scripts unless an operator explicitly opts in.
#
# Build: docker build -t drydock-dep-runner -f docker/dep-runner/Dockerfile .
# Run:   docker compose --profile depupgrade up -d

# --- Build stage: compile dep-runner binary + carry the Go toolchain ---
FROM golang:1.26-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/dep-runner ./cmd/dep-runner

# --- Runtime stage: ecosystem toolchains + runner binary ---
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# Pinned toolchain versions so two builds of the same commit produce the same
# image. Bump these deliberately, the way the core Dockerfile pins
# BETTERLEAKS_VERSION and lsp-bridge pins its language servers.
ARG NODE_VERSION=20.19.4
ARG RUST_VERSION=1.83.0
ARG PIP_VERSION=24.3.1
ARG SETUPTOOLS_VERSION=75.6.0
ARG WHEEL_VERSION=0.45.1

# Base tools + Python runtime in a single layer.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates wget curl git xz-utils \
    python3 python3-pip python3-venv \
    && rm -rf /var/lib/apt/lists/*

# Non-root user (mirrors the core Dockerfile's uid-1000 drydock user).
RUN useradd --uid 1000 --create-home --shell /bin/bash drydock

# Go toolchain from the builder — a single toolchain matching go.mod (go 1.26).
COPY --from=builder /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:/usr/local/cargo/bin:${PATH}"

# Node.js + npm from a pinned tarball (no piping a remote script into root).
RUN wget -qO- "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
    | tar -xJ -C /usr/local --strip-components=1

# Rust/cargo via rustup, pinned toolchain, minimal profile. RUSTUP/CARGO home
# live in a system location; the sandbox points CARGO_HOME at a per-run temp dir
# so the shared toolchain dir is never written by an upgrade job.
ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo
RUN curl -fsSL https://sh.rustup.rs | sh -s -- \
        -y --no-modify-path --profile minimal --default-toolchain "${RUST_VERSION}" \
    && chmod -R a+rX "${RUSTUP_HOME}" "${CARGO_HOME}"

# Pin pip and its build backend so metadata resolution is reproducible.
RUN pip3 install --no-cache-dir --upgrade \
    "pip==${PIP_VERSION}" "setuptools==${SETUPTOOLS_VERSION}" "wheel==${WHEEL_VERSION}"

WORKDIR /app
COPY --from=builder /out/dep-runner /usr/local/bin/dep-runner

# Per-request temp roots live here. Mount a size-limited tmpfs over /work in
# compose so a runaway build cannot fill the host disk.
RUN mkdir -p /work && chown drydock:drydock /work
ENV DEP_RUNNER_ADDR=0.0.0.0:8083 \
    DEP_RUNNER_WORK_ROOT=/work

# Everything — the runner and every toolchain it spawns — runs unprivileged,
# because it executes against attacker-supplied manifests.
USER drydock

# Fail fast if a toolchain is missing from the image.
RUN go version && node --version && npm --version && cargo --version && pip --version

HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -qO- http://localhost:8083/healthz || exit 1

EXPOSE 8083

ENTRYPOINT ["dep-runner", "-addr", "0.0.0.0:8083"]
