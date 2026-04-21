# Scaling Considerations

Drydock is designed as a single-instance service backed by SQLite. This document describes its current throughput characteristics, bottlenecks, and options for scaling.

## Current Bottlenecks

### 1. LLM Inference (Primary)

Each patch requires two LLM calls (planner + reviewer) and optionally a third (meta-review). With a 120-second HTTP timeout per call, a single review can take up to 4–5 minutes under load.

**Impact**: This is the primary throughput limiter. With 2 pipeline workers and a single Ollama instance, Drydock processes roughly 10–30 patches per hour depending on model size and GPU.

### 2. SQLite Single-Writer

SQLite operates in WAL mode with `MaxOpenConns(1)` and `busy_timeout=5000ms`. All writes are serialized. Under normal operation this is not a bottleneck — write operations are small and fast compared to LLM inference.

**When it matters**: If the review queue is persistently full and multiple workers are trying to write results simultaneously, the 5-second busy timeout provides contention relief. If you see `database is locked` errors, the system is write-saturated.

### 3. Git Operations

Git clone, fetch, and grep operations are I/O-bound. A per-repo mutex prevents concurrent operations on the same repository. Cross-repo operations run in parallel.

**Impact**: First-time clones of large repos (>1 GB) can take minutes. Subsequent fetches are fast. The LRU cache keeps hot repos available.

## Pipeline Workers

The `DRYDOCK_PIPELINE_WORKERS` environment variable controls the number of concurrent review workers (default: 2).

**When to increase**: Only if your LLM infrastructure supports concurrent inference requests. With a single Ollama instance, requests are queued at the inference server — more workers just move the queue from Drydock to Ollama without improving throughput.

**Recommended settings**:

| Setup | Workers |
|-------|---------|
| Single Ollama instance | 2 |
| 2 Ollama instances (14B + 70B on separate GPUs) | 3–4 |
| Dedicated inference cluster | Match to cluster concurrency |

## Review Queue

The review queue is a buffered Go channel with capacity 256. Behavior at capacity:

- New tasks are marked as `failed` with reason `"review queue full"`
- On next restart, `ResetStuckReviews` moves stuck `reviewing` tasks back to `pending`
- Failed tasks are not automatically retried during the same session

**For high-volume deployments**: The 256-element buffer is adequate for bursty traffic. Sustained overflow indicates that LLM throughput cannot keep up with event volume — add more inference capacity rather than increasing the buffer.

## Repo Cache Sizing

The LRU cache evicts repositories by least-recent access time. Two limits are independently enforced:

| Limit | Default | Config Var |
|-------|---------|-----------|
| Max repo count | 50 | `DRYDOCK_REPO_CACHE_MAX_COUNT` |
| Max total size | 10 GB | `DRYDOCK_REPO_CACHE_MAX_SIZE_MB` |

**Sizing guidance**:

| Scenario | Count | Size |
|----------|-------|------|
| Personal use (5–10 repos) | 20 | 2048 MB |
| Community relay (50–100 repos) | 100 | 20480 MB |
| High-volume (500+ repos) | 200 | 51200 MB |

Each cloned repo typically ranges from 10 MB (small projects) to 2 GB (large monorepos with history).

## Relay Load

Drydock maintains one long-lived subscription per configured relay. The Nostr pool handles reconnection automatically. With 10+ relays, the listener goroutine can become busy processing events — but the ingest pipeline is fast (mostly SQLite writes and channel sends), so this is rarely a bottleneck.

**Optimization**: Use `DRYDOCK_READ_RELAYS` to subscribe to a small, curated list of relays where NIP-34 events are posted, and `DRYDOCK_WRITE_RELAYS` to publish to a broader set.

## Qdrant Sizing

Qdrant memory usage scales with the number of stored vectors:

| Vectors | RAM (approx) | Use Case |
|---------|-------------|----------|
| 10K | ~100 MB | Small deployment, handful of repos |
| 100K | ~500 MB | Community relay, 50-100 repos |
| 1M | ~2-4 GB | Large-scale, many repos + full NIP archive |

**Storage**: Qdrant stores data on disk with memory-mapped indexes. The `qdrant_storage` Docker volume should be on SSD for query performance.

**Collections**: Drydock uses three collections:
- `nip_specs` — typically small (hundreds of chunks for all NIPs)
- `project_docs` — scales with number of repos, ~100-1000 chunks per repo
- `few_shot_reviews` — grows slowly over time as meta-review identifies good/bad examples

**Recommendation**: For most deployments, Qdrant's default settings are fine. The Docker Compose stack runs Qdrant with default memory limits. For large deployments, consult the [Qdrant configuration docs](https://qdrant.tech/documentation/guides/configuration/).

## Embedding Model Resources

The embedding model converts text to vectors for Qdrant queries.

| Model | VRAM | Throughput | Notes |
|-------|------|-----------|-------|
| `nomic-embed-text` (Ollama) | ~1 GB | ~100 chunks/sec | Default, good quality/speed tradeoff |
| `text-embedding-3-small` (OpenAI) | N/A (API) | Rate-limited | Hosted, no local GPU needed |
| Custom TEI server | ~2-4 GB | ~500 chunks/sec | Best for high-volume ingestion |

Embedding calls are infrequent during normal operation (only during NIP ingestion and per-review Qdrant queries). A shared Ollama instance serving both LLM and embedding models works well for most setups.

## LSP Bridge Resources

The LSP bridge runs language servers as child processes. Each language server has different memory requirements:

| Language Server | Idle RAM | Active RAM (large repo) |
|----------------|---------|------------------------|
| gopls | ~50 MB | ~200-500 MB |
| pyright | ~100 MB | ~300-800 MB |
| typescript-language-server | ~80 MB | ~200-600 MB |
| clangd | ~50 MB | ~100-400 MB |
| rust-analyzer | ~200 MB | ~500 MB-2 GB |

Language servers have a 5-minute idle TTL and are automatically shut down. In practice, only servers for actively reviewed languages consume memory.

**Recommendation**: The LSP bridge Docker image is 2-4 GB. Allocate at least 2 GB RAM for the container. If rust-analyzer is needed, allocate 4+ GB.

## Multi-Instance Deployment

Running multiple Drydock instances against the same SQLite database is **not supported**. SQLite's single-writer model means the second instance would frequently encounter lock contention.

To scale beyond a single instance, you would need to:

1. **Replace SQLite with PostgreSQL** — supports concurrent writers
2. **Add a message broker** (NATS, Redis Streams) — for distributing review tasks across workers
3. **Implement distributed dedup** — the `ingested_events` primary key gate needs to work across instances
4. **Coordinate repo locks** — the per-repo mutex is process-local; multi-instance needs distributed locks

Note: Qdrant already supports multi-instance access. The SQLite limitation is the primary blocker.

This is a significant architectural change and is not currently on the roadmap. For most deployments, a single well-resourced instance (fast GPU, SSD, 50 GB cache) handles the event volume of multiple active Nostr code repositories.

## Performance Tuning Checklist

- [ ] **GPU sizing**: Ensure the GPU has enough VRAM to run your largest model (70B q4 ≈ 40 GB VRAM)
- [ ] **SSD storage**: SQLite, Qdrant, and git operations benefit significantly from SSD over HDD
- [ ] **Workers vs. GPUs**: Match `DRYDOCK_PIPELINE_WORKERS` to your inference parallelism
- [ ] **Repo cache**: Set `DRYDOCK_REPO_CACHE_MAX_SIZE_MB` to at least 2× your expected working set
- [ ] **Relay selection**: Subscribe only to relays where NIP-34 events are posted
- [ ] **Log level**: `debug` logging adds measurable overhead — use `info` in production
- [ ] **Qdrant**: Place `qdrant_storage` volume on SSD. Monitor RAM usage if indexing many repos
- [ ] **Embedding**: Share an Ollama instance for embedding + LLM, or dedicate a small GPU for embeddings
- [ ] **LSP bridge**: Only enable (`--profile lsp`) if language-server analysis is needed; it adds 2-4 GB image size
