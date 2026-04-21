# Scaling Considerations

Drydock is designed as a single-instance service backed by SQLite. This document describes its current throughput characteristics, bottlenecks, and options for scaling.

## Current Bottlenecks

### 1. LLM Inference

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

## Multi-Instance Deployment

Running multiple Drydock instances against the same SQLite database is **not supported**. SQLite's single-writer model means the second instance would frequently encounter lock contention.

To scale beyond a single instance, you would need to:

1. **Replace SQLite with PostgreSQL** — supports concurrent writers
2. **Add a message broker** (NATS, Redis Streams) — for distributing review tasks across workers
3. **Implement distributed dedup** — the `ingested_events` primary key gate needs to work across instances
4. **Coordinate repo locks** — the per-repo mutex is process-local; multi-instance needs distributed locks

This is a significant architectural change and is not currently on the roadmap. For most deployments, a single well-resourced instance (fast GPU, SSD, 50 GB cache) handles the event volume of multiple active Nostr code repositories.

## Performance Tuning Checklist

- [ ] **GPU sizing**: Ensure the GPU has enough VRAM to run your largest model (70B q4 ≈ 40 GB VRAM)
- [ ] **SSD storage**: SQLite and git operations benefit significantly from SSD over HDD
- [ ] **Workers vs. GPUs**: Match `DRYDOCK_PIPELINE_WORKERS` to your inference parallelism
- [ ] **Repo cache**: Set `DRYDOCK_REPO_CACHE_MAX_SIZE_MB` to at least 2× your expected working set
- [ ] **Relay selection**: Subscribe only to relays where NIP-34 events are posted
- [ ] **Log level**: `debug` logging adds measurable overhead — use `info` in production
