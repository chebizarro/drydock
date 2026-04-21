# Architecture

Drydock is a single-binary Go service with a pipeline architecture. Events flow in one direction: from Nostr relays, through ingestion and review, back out as published comments.

## Component Map

```
┌─────────────────────────────────────────────────────────────────┐
│                        Nostr Relays                             │
│   (wss://relay.damus.io, wss://nos.lol, wss://relay.primal.net)│
└──────────┬─────────────────────────────────────────▲────────────┘
           │ subscribe (NIP-34 kinds)                │ publish (kind 1111)
           ▼                                         │
┌──────────────────┐                      ┌──────────────────┐
│     Listener     │                      │    Publisher      │
│  (nostr pool)    │                      │ (relay fanout)    │
└────────┬─────────┘                      └──────▲───────────┘
         │ events                                │ signed events
         ▼                                       │
┌──────────────────┐    ReviewTask     ┌─────────┴──────────┐
│     Ingest       │───────────────────▶   Pipeline Runner  │
│  (processor)     │   channel (256)   │   (N workers)      │
└──────────────────┘                   └──┬──┬──┬──┬──┬─────┘
                                          │  │  │  │  │
              ┌───────────────────────────┘  │  │  │  └──────────┐
              ▼                              ▼  │  ▼              ▼
       ┌────────────┐  ┌─────────────────┐  │ ┌──────────┐ ┌──────────┐
       │    Repo     │  │ Context Builder │  │ │  Review  │ │   Meta   │
       │   Manager   │  │  (7 layers)     │  │ │  Engine  │ │  Review  │
       └────────────┘  └─────────────────┘  │ └──────────┘ └──────────┘
                                            │       │
                                            │       ▼
                                            │  ┌──────────┐
                                            │  │   LLM    │
                                            │  │  Client   │
                                            │  └──────────┘
                                            ▼
                                      ┌──────────┐
                                      │  SQLite  │
                                      │  (WAL)   │
                                      └──────────┘
```

| Package | Responsibility |
|---------|---------------|
| `listener` | Subscribes to Nostr relays, receives events, manages high-water-mark for restart resilience |
| `ingest` | Verifies signatures, deduplicates events, filters stale/closed patches, enqueues review tasks |
| `pipeline` | Worker pool that orchestrates the full review lifecycle for each patch |
| `contextbuilder` | Builds deterministic context bundles with a 64K token budget across 7 priority layers |
| `reviewengine` | Two-stage LLM pipeline: planner selects a model route, reviewer produces findings |
| `publisher` | Constructs and publishes kind 1111 Nostr comment events with relay fanout |
| `metareview` | Self-improvement loop: evaluates review quality and routes feedback for prompt tuning |
| `repo` | Clones/fetches git repositories with URL validation and LRU cache eviction |
| `signing` | NIP-46 bunker signer or local nsec signer for event signing |
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
   - `contextbuilder.Builder.Build` — assembles context layers within token budget
   - `reviewengine.Engine.Run` — planner call → model route selection → reviewer call with checklist injection
   - `publisher.Service.PublishReview` — builds kind 1111 events, signs them, fans out to relays
   - `metareview.Service.RunAsync` — asynchronous quality evaluation (non-blocking)

4. **Publish** — The publisher resolves target relays (patch-seen relays + repo announcement relays + defaults), constructs summary and detail comment events, signs each, and publishes.

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
