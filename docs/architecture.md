# Architecture

Drydock is a Go-based NIP-34 automated code review agent. The core review pipeline runs as a single binary (`drydock-core`), with optional microservices for vector search (Qdrant), language server analysis (LSP bridge), and text embedding.

## Component Map

```
                        ┌─────────────────────────────────────────┐
                        │             Nostr Relays                │
                        │  wss://relay.damus.io  wss://nos.lol   │
                        └──────┬──────────────────────▲───────────┘
                               │ subscribe            │ publish
                               │ (NIP-34 kinds)       │ (kind 1111)
                               ▼                      │
┌──────────────────────────────────────────────────────┴──────────┐
│                        drydock-core                             │
│                                                                 │
│  ┌──────────┐  ┌──────────┐  ┌───────────┐  ┌───────────────┐  │
│  │ Listener │→ │  Ingest  │→ │  Pipeline │→ │   Publisher   │  │
│  │(nostr    │  │(dedup +  │  │ (N workers│  │ (kind 1111    │  │
│  │ pool)    │  │ enqueue) │  │  per task)│  │  relay fanout)│  │
│  └──────────┘  └──────────┘  └──┬──┬──┬──┘  └───────────────┘  │
│                                 │  │  │                         │
│       ┌─────────────────────────┘  │  └───────────────┐        │
│       ▼                            ▼                  ▼        │
│  ┌─────────┐  ┌──────────────────────────┐  ┌─────────────┐   │
│  │  Repo   │  │    Context Builder       │  │   Review     │   │
│  │ Manager │  │  (8 priority layers)     │  │   Engine     │   │
│  └─────────┘  │  ┌──────┐ ┌───────────┐ │  └──────┬──────┘   │
│               │  │tree- │ │  ripgrep/  │ │         │          │
│               │  │sitter│ │  git grep  │ │         ▼          │
│               │  └──────┘ └───────────┘ │  ┌─────────────┐   │
│               └──────────────────────────┘  │  Meta-Review│   │
│                                             └─────────────┘   │
│  ┌──────────┐  ┌──────────┐  ┌──────────────────────────────┐ │
│  │ SQLite   │  │  Signing │  │          Config              │ │
│  │  (WAL)   │  │(bunker → │  │ (env vars, graceful degrade) │ │
│  │          │  │socket →  │  │                              │ │
│  │          │  │nsec)     │  │                              │ │
│  └──────────┘  └──────────┘  └──────────────────────────────┘ │
└───────────┬───────────────────────┬───────────────┬───────────┘
            │ REST API              │ REST API      │ REST API
            ▼                       ▼               ▼
  ┌──────────────────┐  ┌───────────────┐  ┌──────────────────┐
  │  Qdrant v1.12    │  │ Embedding     │  │  LSP Bridge      │
  │  (vector search) │  │ Server        │  │  (optional,      │
  │                  │  │ (Ollama or    │  │   profile: lsp)  │
  │  Collections:    │  │  dedicated)   │  │                  │
  │  • nip_specs     │  │               │  │  gopls, pyright, │
  │  • project_docs  │  │  nomic-embed- │  │  tsserver,       │
  │  • few_shot      │  │  text         │  │  clangd, rust-   │
  │                  │  │               │  │  analyzer         │
  └──────────────────┘  └───────────────┘  └──────────────────┘
       (optional)           (optional)          (optional)
```

## Service Topology

| Service | Binary | Required | Purpose |
|---------|--------|----------|---------|
| `drydock-core` | `cmd/drydock` | Yes | Full review pipeline: listen → ingest → build context → review → publish |
| `qdrant` | Docker image `qdrant/qdrant:v1.12.6` | No | Vector similarity search for NIP specs, project docs, few-shot examples |
| Embedding server | Ollama or dedicated endpoint | No | Text → vector embeddings (required if Qdrant is enabled) |
| `lsp-bridge` | `cmd/lsp-bridge` | No | Multi-language LSP server manager for type-aware symbol analysis |

**Graceful degradation**: All external services are optional. When unconfigured or unreachable, drydock-core logs a warning and falls back:
- No Qdrant → RAG context layers disabled
- No embedding server → Qdrant cannot be used
- No LSP bridge → symbols extracted via tree-sitter + callsites via ripgrep/git grep
- No signer → listen-only mode (events ingested but no reviews published)

## Package Map

| Package | Responsibility |
|---------|---------------|
| `listener` | Subscribes to Nostr relays, receives events, manages high-water-mark for restart resilience |
| `ingest` | Verifies signatures, deduplicates events, filters stale/closed patches, enqueues review tasks |
| `pipeline` | Worker pool that orchestrates the full review lifecycle for each patch |
| `contextbuilder` | Builds deterministic context bundles with a 64K token budget across 8 priority layers; workspace boundary detection for monorepos |
| `symbols` | Tree-sitter AST parsing for 9 languages (Go, Python, JS, TS, Rust, C, C++, Java, Ruby) — extracts declarations from changed files |
| `reviewengine` | Two-stage LLM pipeline: planner selects a model route, reviewer produces findings |
| `publisher` | Constructs and publishes kind 1111 (NIP-22 comment) Nostr events with relay fanout |
| `metareview` | Self-improvement loop: evaluates review quality and routes feedback for prompt tuning |
| `promptrefine` | Automated prompt versioning: batches prompt gaps, refines via LLM, activates with eval-gated rollback |
| `repo` | Clones/fetches git repositories with URL validation and LRU cache eviction |
| `signing` | Signer chain: NIP-46 bunker → NIP-5F Unix socket (Signet) → NIP-55L DBus (Linux) → local nsec |
| `vectorstore` | Qdrant REST API client — CRUD, search, scroll, collection management |
| `embedding` | HTTP client for OpenAI-compatible embedding endpoints |
| `nipingest` | Markdown NIP spec ingestion: chunk by heading, embed, upsert to Qdrant with content-hash dedup |
| `lspbridge` | Shared types + HTTP client for the LSP bridge sidecar |
| `lspbridge/server` | LSP bridge HTTP server: process lifecycle manager, JSON-RPC 2.0 over stdio |
| `db` | SQLite storage with WAL mode, migrations, and all state management queries |
| `health` | HTTP server with `/healthz` (liveness) and `/readyz` (readiness) endpoints |
| `config` | Environment variable parsing with typed defaults |
| `eval` | Held-out evaluation harness for measuring review quality metrics |

## End-to-End Data Flow

Trace of a single patch event from relay to published review:

1. **Receive** — `listener.Service.Run` calls `pool.SubscribeManyNotifyClosed` with a filter for [NIP-34 event kinds](nostr-protocol.md). The pool delivers events on a channel.

2. **Ingest** — `ingest.Processor.ProcessEvent` is called for each event:
   - Rejects events with invalid signatures (`event.VerifySignature()`)
   - Inserts into `ingested_events` (idempotent; duplicates are skipped)
   - For patch kinds (1617/1618/1619): checks staleness against `repository_snapshots`, checks if the root is closed via `root_statuses`, then calls `store.BeginReview` to acquire a review slot
   - Sends a `ReviewTask` to the pipeline channel (buffer: 256)

3. **Pipeline** — `pipeline.Runner.work` picks up the task:
   - `repo.Service.PreparePatchSeries` — clones or fetches the repository, applies the patch series to a throwaway branch
   - `contextbuilder.Builder.Build` — detects workspace boundaries, assembles context layers within token budget
   - `reviewengine.Engine.Run` — planner call → model route selection → reviewer call with checklist injection
   - `publisher.Service.PublishReview` — builds kind 1111 events, signs them, fans out to relays
   - `metareview.Service.RunAsync` — asynchronous quality evaluation (non-blocking)

4. **Publish** — The publisher resolves target relays (patch-seen relays + repo announcement relays + defaults), constructs summary and detail comment events, signs each, and publishes.

## Context Builder Data Flow

```
Patch diff
    │
    ├─→ Layer 1: patch (raw diff, 40 KB cap)
    ├─→ Layer 2: modified-files (full file contents, workspace-scoped)
    ├─→ Layer 3: symbols (tree-sitter AST → changed declarations → rg/git grep callsites)
    ├─→ Layer 4: tests (rg/git grep in test files, workspace-scoped)
    ├─→ Layer 5: imports-exports (import/export line extraction)
    ├─→ Layer 6: commit-history (git log for changed files)
    ├─→ Layer 7: project-docs (workspace-local + repo-level docs)
    └─→ Layer 8: qdrant-docs (vector search: NIP specs + project docs)
                                    │
                                    ├─ embed(patch diff) → query nip_specs (if Nostr-related)
                                    └─ embed(patch diff) → query project_docs
```

When the 64K token budget is reached, the current layer and all lower-priority layers are dropped (hard stop policy).

## Signer Chain

Drydock tries signers in priority order. The first successful signer wins:

```
1. NIP-46 Bunker     (DRYDOCK_SIGNER_BUNKER_URL set)
        │ fail/skip
        ▼
2. NIP-5F Socket     (DRYDOCK_SIGNER_SOCKET_PATH set, or ~/.local/share/nostr/signer.sock exists)
        │ fail/skip
        ▼
3. NIP-55L DBus      (Linux only, DRYDOCK_SIGNER_DBUS=true)
        │ fail/skip
        ▼
4. Local nsec        (DRYDOCK_SIGNER_NSEC set)
        │ fail/skip
        ▼
5. No signer → listen-only mode (warning logged)
```

## Docker Compose Topology

```
┌─────────────────────────────────────────────────────────┐
│                    drydock_net (bridge)                  │
│                                                         │
│  ┌──────────────┐    ┌─────────┐    ┌──────────────┐   │
│  │ drydock-core │───▶│ qdrant  │    │  lsp-bridge  │   │
│  │              │    │ :6333   │    │  :8082       │   │
│  │  depends_on: │    │         │    │              │   │
│  │  qdrant      │    │ healthy │    │ profile: lsp │   │
│  └──────┬───────┘    └─────────┘    └──────────────┘   │
│         │                                               │
│         ├─▶ host.docker.internal (Ollama / LLM)         │
│         │                                               │
│  ┌──────┴───────┐    ┌──────────────┐                   │
│  │ drydock_data │    │qdrant_storage│                   │
│  │   (volume)   │    │   (volume)   │                   │
│  └──────────────┘    └──────────────┘                   │
└─────────────────────────────────────────────────────────┘
```

**Default**: `docker compose up -d` starts drydock-core + qdrant.
**With LSP**: `docker compose --profile lsp up -d` adds the LSP bridge.

## Review Log State Machine

The `review_log` table tracks each review through a state machine:

```
                 ┌──────────┐
                 │ pending  │ ← BeginReview (unique patch_event_id + repo_id)
                 └────┬─────┘
                      │ pipeline picks up task
                      ▼
                 ┌──────────┐
                 │reviewing │
                 └──┬───┬───┘
        success ────┘   └──── failure
                ▼              ▼
          ┌──────────┐  ┌──────────┐
          │published │  │  failed  │
          └──────────┘  └──────────┘
```

**Recovery**: On startup, `store.ResetStuckReviews` moves any rows stuck in `reviewing` back to `pending`. This handles crashes mid-review.

## Concurrency Model

- **Listener**: Single goroutine reading from the Nostr pool channel. Events are processed synchronously in `ProcessEvent`, then dispatched to the review queue channel.
- **Pipeline workers**: `N` goroutines (configurable via `DRYDOCK_PIPELINE_WORKERS`, default 2). Each reads from the shared review queue. Workers drain on context cancellation via `sync.WaitGroup`.
- **Meta-review**: Spawned as individual goroutines by `RunAsync`, gated by a `semaphore.Weighted` with `MaxConcurrent` (default 10).
- **Repo locks**: Per-repository `sync.Mutex` stored in a `sync.Map` prevents concurrent git operations on the same repo path.

## Database Schema

All state is stored in SQLite with WAL mode, foreign keys, and `busy_timeout=5000ms`. `MaxOpenConns` is set to 1 for write safety.

| Table | Purpose |
|-------|---------|
| `ingested_events` | Deduplication store for all received Nostr events |
| `repositories` | Repository announcements (kind 30617) with clone URLs |
| `repository_snapshots` | Latest repository state (kind 30618) for staleness checks |
| `patch_events` | Patch/PR events (kinds 1617/1618/1619) |
| `patch_event_relays` | Which relays each patch event was seen on |
| `review_events` | Published review comment events |
| `thread_cache` | Thread membership tracking by root event ID |
| `root_statuses` | Status events (kinds 1630–1633) for closed/applied filtering |
| `review_log` | Review state machine (`pending → reviewing → published \| failed`) |
| `meta_review_log` | Meta-review results with context hashes |
| `meta_review_routes` | Feedback routing decisions from meta-reviews |
| `few_shot_reviews` | Positive/negative few-shot examples for prompt improvement |
| `listener_state` | High-water-mark timestamp for restart resilience |
| `eval_runs` | Evaluation harness run results and metrics |

Full DDL is in [`internal/db/schema.go`](../internal/db/schema.go).

## High-Water-Mark Resilience

The listener persists the `created_at` timestamp of the most recent event it processed into the `listener_state` table. On restart:

1. Read the persisted high-water-mark
2. Subtract 30 seconds to handle clock skew between relays
3. Use whichever is earlier: the adjusted high-water-mark or the configured lookback window

This ensures no events are missed across restarts, at the cost of re-processing a small overlap window (deduplicated by `ingested_events`).
