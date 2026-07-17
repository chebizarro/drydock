# Deployment Guide

## Prerequisites

- **Go 1.26+** with CGO support (for native builds — tree-sitter grammars require CGO)
- **Git** in `$PATH` (used at runtime for clone, fetch, grep, log)
- **ripgrep** (`rg`) recommended for faster symbol callsite search (falls back to git grep)
- **Ollama** or any OpenAI-compatible inference server
- A **Nostr signing identity** — NIP-46 bunker, NIP-5F socket signer (Signet), or nsec key

## Native (Development)

```bash
# 1. Clone and build
git clone https://github.com/user/drydock.git
cd drydock
go build ./...

# 2. Configure
cp .env.example .env
# Edit .env — at minimum set DRYDOCK_SIGNER_NSEC or DRYDOCK_SIGNER_BUNKER_URL

# 3. Start Ollama with required models
ollama pull qwen2.5-coder:14b-instruct-q4_K_M
ollama pull qwen2.5-coder:32b-instruct-q4_K_M
ollama pull llama3.3:70b-instruct-q4_K_M

# 4. Run
source .env  # or use direnv / dotenv
go run ./cmd/drydock
```

The health endpoint is available at `http://localhost:8081/readyz`.

### Management listener security

The management listener defaults to `127.0.0.1:8081`. The dashboard and its APIs (`/dashboard/` and `/api/*`) are unauthenticated only when `DRYDOCK_DASHBOARD_BEARER_TOKEN` is empty, so keep the listener on loopback in that mode. If you deliberately expose `DRYDOCK_HEALTH_ADDR` on a non-loopback interface, configure a strong token:

```bash
DRYDOCK_HEALTH_ADDR=0.0.0.0:8081
DRYDOCK_DASHBOARD_BEARER_TOKEN='replace-with-a-long-random-secret'
curl -H 'Authorization: Bearer replace-with-a-long-random-secret' \
  http://localhost:8081/api/stats
```

The bearer token protects only `/dashboard/` and `/api/*`; `/healthz`, `/readyz`, and `/metrics` remain available for probes and monitoring.

## Docker (Microservices)

Drydock runs as a multi-service Docker Compose stack: drydock-core (always), Qdrant (always), and optionally the LSP bridge. Compose requires `DRYDOCK_SIGNER_BUNKER_URL` and fails configuration with a clear error when it is unset.

```bash
# 1. Configure
cp .env.example .env
# Edit .env — set DRYDOCK_SIGNER_BUNKER_URL and your LLM endpoints

# 2. Default stack (drydock-core + Qdrant)
docker compose up --build -d

# 2b. With LSP bridge for enhanced symbol analysis
docker compose --profile lsp up --build -d

# 3. Check health
curl http://localhost:8081/readyz   # drydock-core
curl http://localhost:6333/healthz  # qdrant
curl http://localhost:8082/healthz  # lsp-bridge (if enabled)

# 4. Follow logs
docker compose logs -f
```

### Volume Layout

| Volume | Mount | Contents |
|--------|-------|----------|
| `drydock_data` | `/data` (drydock-core), `/data:ro` (lsp-bridge) | SQLite database, cloned git repos |
| `qdrant_storage` | `/qdrant/storage` | Vector index data, collections |

**Backup strategy**: Back up both volumes to preserve review history, listener state, and vector indexes.

```bash
# Stop services before backup for consistency
docker compose stop

# SQLite: copy both .db and .db-wal files
tar czf drydock-backup.tar.gz \
  $(docker volume inspect drydock_data --format '{{ .Mountpoint }}')

# Qdrant: copy storage directory
tar czf qdrant-backup.tar.gz \
  $(docker volume inspect qdrant_storage --format '{{ .Mountpoint }}')

docker compose start
```

### Accessing Host Services

The `docker-compose.yml` includes `extra_hosts: host.docker.internal:host-gateway`, which maps `host.docker.internal` to the host machine. The default LLM endpoint URLs use this hostname:

```
DRYDOCK_PLANNER_BASE_URL=http://host.docker.internal:11434/v1
```

If your LLM endpoints are on a different host, override the `*_BASE_URL` variables in `.env`.

### Makefile Shortcuts

```bash
make up      # docker compose up --build -d
make down    # docker compose down --remove-orphans
make logs    # docker compose logs -f drydock-core
make eval    # run the evaluation harness in a container
make build   # go build ./...
make test    # go test ./...
make ps      # docker compose ps
make config  # docker compose config (validate compose file)
```

## Signing Configuration

In production (including the default Compose stack), Drydock requires a NIP-46 bunker signer. Development mode (`--dev`, `DEV_MODE=true`, or `DRYDOCK_DEV_MODE=true`) also permits the local signer fallbacks below.

### 1. NIP-46 Bunker (Recommended for Production)

The bunker keeps your private key on a separate device or service. Drydock never sees the raw key.

```bash
DRYDOCK_SIGNER_BUNKER_URL=bunker://relay.example.com/npub1abc...?secret=xyz
```

On first connection, the bunker may require interactive authorization. Drydock logs the auth URL:

```
{"level":"INFO","msg":"bunker auth required","url":"https://..."}
```

### 2. NIP-5F Unix Socket (Signet)

If a Signet-compatible signer is running, Drydock auto-detects the socket at `~/.local/share/nostr/signer.sock`, or you can set a custom path:

```bash
DRYDOCK_SIGNER_SOCKET_PATH=/path/to/signer.sock
```

The socket uses NIP-5F JSON-RPC framing (4-byte big-endian length prefix).

### 3. NIP-55L DBus (Linux Only)

On Linux, Drydock can use the `org.nostr.Signer` DBus session bus interface:

```bash
DRYDOCK_SIGNER_DBUS=true
```

### 4. Local nsec (Development Only)

```bash
DRYDOCK_SIGNER_NSEC=nsec1your_key_here
```

> **⚠️ Security Warning**: The nsec is stored in plaintext. Never commit `.env` files with real keys. Use Docker secrets or a secrets manager in production environments.

### No Signer

Outside development mode, Drydock exits when no bunker signer is configured. The Compose file catches this earlier during configuration and reports that `DRYDOCK_SIGNER_BUNKER_URL` must be set. In development mode only, Drydock can run without a signer in listen-only mode: events are ingested and stored, but the review pipeline is disabled and no comments are published.

## LLM Endpoint Configuration

### Single-GPU Setup

Point all endpoints to one Ollama instance. The planner and reviewer calls are serialized.

```bash
# All on one Ollama instance (port 11434)
DRYDOCK_PLANNER_BASE_URL=http://127.0.0.1:11434/v1
DRYDOCK_CODER32B_BASE_URL=http://127.0.0.1:11434/v1
DRYDOCK_LLM70B_BASE_URL=http://127.0.0.1:11434/v1
DRYDOCK_CODER14B_BASE_URL=http://127.0.0.1:11434/v1
DRYDOCK_META_BASE_URL=http://127.0.0.1:11434/v1
```

### Multi-GPU / Multi-Server Setup

Spread models across multiple Ollama instances for parallel inference:

```bash
# GPU 1: Small models (14B planner + 14B coder)
DRYDOCK_PLANNER_BASE_URL=http://gpu1:11434/v1
DRYDOCK_CODER14B_BASE_URL=http://gpu1:11434/v1

# GPU 2: Medium model (32B coder)
DRYDOCK_CODER32B_BASE_URL=http://gpu2:11434/v1

# GPU 3: Large models (70B reviewer + 70B meta)
DRYDOCK_LLM70B_BASE_URL=http://gpu3:11434/v1
DRYDOCK_META_BASE_URL=http://gpu3:11434/v1
```

With this setup, increase `DRYDOCK_PIPELINE_WORKERS` to match your inference parallelism.

## Running the Eval Harness

The evaluation harness measures review quality against a labeled dataset:

```bash
# Native (separate binary, no DRYDOCK_MODE needed)
go run ./cmd/drydock-eval

# Docker (DRYDOCK_MODE selects binary via entrypoint.sh)
make eval
```

Results are printed to stdout as JSON and persisted in the `eval_runs` SQLite table. See [Evaluation](eval.md) for dataset format and metrics.

## Embedding Model Setup

Qdrant-based RAG features require an embedding model. The simplest option is Ollama:

```bash
# Pull the embedding model
ollama pull nomic-embed-text

# Configure drydock to use it
DRYDOCK_EMBED_BASE_URL=http://host.docker.internal:11434/v1
DRYDOCK_EMBED_MODEL=nomic-embed-text
DRYDOCK_QDRANT_URL=http://qdrant:6333
```

Any OpenAI-compatible `/embeddings` endpoint works (e.g., vLLM, TEI, OpenAI API).

## Lemmy Deployment

For the Lemmy deployment target (192.168.40.110), use the `.env.lemmy` file:

```bash
cp .env.lemmy .env
# Review and adjust settings for your environment
docker compose up --build -d
```

## Production Hardening Checklist

- [ ] **Signing**: Use NIP-46 bunker or NIP-5F socket signer, not local nsec
- [ ] **Secrets**: Never commit `.env` files. Use Docker secrets, Vault, or environment injection
- [ ] **Database backups**: Back up both `drydock_data` and `qdrant_storage` volumes regularly
- [ ] **Repo cache sizing**: Set `DRYDOCK_REPO_CACHE_MAX_SIZE_MB` based on available disk. Repos range from 10 MB to 2 GB each
- [ ] **LLM timeouts**: The HTTP client has a 120-second timeout per request. Ensure your GPU can complete inference within this window for the models you're using
- [ ] **Log level**: Use `info` in production. `debug` logs raw LLM responses and is very verbose
- [ ] **Health monitoring**: Point your orchestrator's health checks at:
  - `GET /readyz` on port 8081 (drydock-core)
  - `GET /healthz` on port 6333 (Qdrant)
  - `GET /healthz` on port 8082 (LSP bridge, if enabled)
- [ ] **Single instance**: SQLite is single-writer. Do not run multiple Drydock instances against the same database file
- [ ] **Restart policy**: The Docker Compose file uses `restart: unless-stopped`. Ensure your deployment platform has equivalent behavior
- [ ] **Qdrant sizing**: Allocate ~1 GB RAM per 1M vectors. See [Scaling](scaling.md) for guidance
