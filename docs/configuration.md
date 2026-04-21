# Configuration Reference

All configuration is via environment variables with the `DRYDOCK_` prefix. Copy `.env.example` as a starting point:

```bash
cp .env.example .env
```

## Runtime

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_MODE` | `listener` \| `eval` | `listener` | Operating mode. `listener` runs the full review pipeline; `eval` runs the evaluation harness and exits. |
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

Drydock needs a Nostr identity to sign review comments. Two signing methods are supported, checked in order:

1. **NIP-46 Bunker** (recommended for production) — key never touches disk
2. **Local nsec** — for development and testing only

If neither is configured, the listener and ingest pipeline still run, but the review pipeline is disabled (no reviews are published).

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_SIGNER_BUNKER_URL` | string | *(empty)* | NIP-46 bunker URL (`bunker://...`) or NIP-05 bunker identifier. Checked first. |
| `DRYDOCK_SIGNER_NSEC` | string | *(empty)* | Raw nsec bech32 key or 64-character hex private key. **Warning**: this is a plaintext secret. Use Docker secrets or a secrets manager in production. |

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
