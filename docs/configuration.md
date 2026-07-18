# Configuration Reference

All configuration is via environment variables with the `DRYDOCK_` prefix. Copy `.env.example` as a starting point:

```bash
cp .env.example .env
```

## Runtime

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_MODE` | `listener` \\| `eval` | `listener` | Docker entrypoint routing: `listener` runs `cmd/drydock`, `eval` runs `cmd/drydock-eval`. Not parsed by the Go binaries themselves — only used by `scripts/entrypoint.sh`. |
| `DRYDOCK_LOG_LEVEL` | `debug` \| `info` \| `warn` \| `error` | `info` | Structured JSON log level. `debug` is verbose and includes raw LLM responses. |

## Database & Storage

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_DATABASE_URL` | string | `file:drydock.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)` | SQLite connection string. Includes pragma directives for foreign keys, write lock timeout, and WAL journaling. |
| `DRYDOCK_REPO_CACHE_DIR` | path | `repos` | Directory for cloned git repositories. |
| `DRYDOCK_REPO_CACHE_MAX_COUNT` | integer | `50` | Maximum number of cached repositories before LRU eviction. Set to `0` to disable count-based eviction. |
| `DRYDOCK_REPO_CACHE_MAX_SIZE_MB` | integer | `10240` | Maximum total cache size in MB (default 10 GB). Set to `0` to disable size-based eviction. |

## Nostr Relays

Drydock supports separate relay lists for reading (subscribing to events) and writing (publishing reviews). If read/write-specific lists are not set, `DRYDOCK_RELAYS` is used for both.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_RELAYS` | comma-separated URLs | `wss://relay.damus.io,wss://nos.lol,wss://relay.primal.net` | Fallback relay list used for both reading and writing when specific lists are not set. |
| `DRYDOCK_READ_RELAYS` | comma-separated URLs | *(empty — falls back to `DRYDOCK_RELAYS`)* | Relays to subscribe to for incoming events. |
| `DRYDOCK_WRITE_RELAYS` | comma-separated URLs | *(empty — falls back to `DRYDOCK_RELAYS`)* | Relays to publish review comments to. |

## Listener

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_LISTENER_LOOKBACK_MIN` | integer (minutes) | `5` | How far back to look when starting a fresh subscription with no persisted high-water-mark. |

## Signing

Drydock needs a Nostr identity to sign review comments. Two signing methods are supported, checked in priority order:

1. **NIP-46 Bunker** (recommended for production) — key never touches disk
2. **Local nsec** — for development and testing only

If none is configured, the listener and ingest pipeline still run, but the review pipeline is disabled (no reviews are published).

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_SIGNER_BUNKER_URL` | string | *(empty)* | NIP-46 bunker URL (`bunker://...`) or NIP-05 bunker identifier. Checked first. |
| `DRYDOCK_SIGNER_NSEC` | string | *(empty)* | Raw nsec bech32 key or 64-character hex private key. **Warning**: this is a plaintext secret. Use Docker secrets or a secrets manager in production. Checked last. |

## LLM Endpoints

Drydock uses OpenAI-compatible `/chat/completions` endpoints. Five model slots are configured independently — they can all point to the same Ollama instance or be spread across multiple servers.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_LLM_API_KEY` | string | *(empty)* | Shared API key for all LLM endpoints. Sent as `Authorization: Bearer <key>`. |
| `DRYDOCK_PLANNER_BASE_URL` | URL | `http://127.0.0.1:11434/v1` | Base URL for the planner model (lightweight, selects review route). |
| `DRYDOCK_PLANNER_MODEL` | string | `qwen2.5-coder-14b-instruct-q4_k_m` | Model name for planner requests. |
| `DRYDOCK_CODER32B_BASE_URL` | URL | `http://127.0.0.1:11434/v1` | Base URL for the 32B coder model (complex code, multi-file). |
| `DRYDOCK_CODER32B_MODEL` | string | `qwen2.5-coder-32b-instruct-q4_k_m` | Model name for coder32b requests. |
| `DRYDOCK_LLM70B_BASE_URL` | URL | `http://127.0.0.1:11435/v1` | Base URL for the 70B model (architecture, security). |
| `DRYDOCK_LLM70B_MODEL` | string | `llama-3.3-70b-instruct-q4_k_m` | Model name for llm70b requests. |
| `DRYDOCK_CODER14B_BASE_URL` | URL | `http://127.0.0.1:11434/v1` | Base URL for the 14B coder model (simple patches, style). |
| `DRYDOCK_CODER14B_MODEL` | string | `qwen2.5-coder-14b-instruct-q4_k_m` | Model name for coder14b requests. |
| `DRYDOCK_META_BASE_URL` | URL | `http://127.0.0.1:11436/v1` | Base URL for the meta-review model. |
| `DRYDOCK_META_MODEL` | string | `llama-3.3-70b-instruct-q4_k_m` | Model name for meta-review requests. |

### Single-Endpoint Pattern

For development or single-GPU deployments, point all endpoints to the same Ollama instance:

```bash
DRYDOCK_PLANNER_BASE_URL=http://127.0.0.1:11434/v1
DRYDOCK_CODER32B_BASE_URL=http://127.0.0.1:11434/v1
DRYDOCK_LLM70B_BASE_URL=http://127.0.0.1:11434/v1
DRYDOCK_CODER14B_BASE_URL=http://127.0.0.1:11434/v1
DRYDOCK_META_BASE_URL=http://127.0.0.1:11434/v1
```

This works but serializes all LLM calls through one instance — see [Scaling](scaling.md) for multi-endpoint setups.

## Qdrant Vector Store (Optional)

Qdrant provides vector similarity search for NIP spec retrieval, project documentation, and few-shot review examples. All Qdrant features are disabled when `DRYDOCK_QDRANT_URL` is empty.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_QDRANT_URL` | URL | *(empty)* | Qdrant REST API endpoint (e.g., `http://qdrant:6333`). Empty disables all RAG features. |
| `DRYDOCK_QDRANT_API_KEY` | string | *(empty)* | API key for Qdrant authentication. Not needed for local/Docker deployments. |

Drydock auto-creates three collections on startup:
- `nip_specs` — Nostr Improvement Proposal documentation chunks
- `project_docs` — Per-project documentation embeddings
- `few_shot_reviews` — Positive/negative review examples for prompt enrichment

## Embedding Model (Optional)

Required when Qdrant is enabled. Any OpenAI-compatible `/embeddings` endpoint works.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_EMBED_BASE_URL` | URL | *(empty)* | Embedding server base URL (e.g., `http://host.docker.internal:11434/v1` for Ollama). |
| `DRYDOCK_EMBED_MODEL` | string | `nomic-embed-text` | Model name for embedding requests. |
| `DRYDOCK_EMBED_API_KEY` | string | *(empty)* | API key for the embedding endpoint. |

## LSP Bridge (Optional)

The LSP bridge is a separate HTTP service that manages language servers for type-aware symbol analysis. Activate via the `lsp` Docker profile.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_LSP_BRIDGE_URL` | URL | *(empty)* | LSP bridge endpoint (e.g., `http://lsp-bridge:8082`). Empty disables LSP-enhanced analysis; drydock falls back to tree-sitter + ripgrep. |

## Pipeline

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_PIPELINE_WORKERS` | integer | `2` | Number of concurrent review pipeline workers. Increase only if your LLM endpoints support concurrent requests. |

## Health & Monitoring

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_HEALTH_ADDR` | `host:port` | `:8081` | Listen address for the health check HTTP server. |

Endpoints:
- `GET /healthz` — Always returns `200 OK`. Use as a liveness probe.
- `GET /readyz` — Returns `200 OK` when the service is started and the database is reachable. Returns `503` during startup or if the DB is unreachable. Use as a readiness probe.

## Evaluation

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_EVAL_DATASET_PATH` | path | `eval/heldout-sample.json` | Path to the JSON evaluation dataset. |
