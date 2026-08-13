# Architecture

Drydock is a Go-based NIP-34 automated code review agent. The core review pipeline runs as `drydock-core`; `drydock-mcp` exposes the same frozen-snapshot tools to authenticated external clients. Optional services provide vector search (Qdrant), language-server analysis (LSP bridge), and text embedding.

## Component Map

```
                        ┌─────────────────────────────────────────┐
                        │             Nostr Relays                │
                        │  wss://relay.damus.io  wss://nos.lol   │
                        └──────┬──────────────────────▲───────────┘
                               │ subscribe            │ publish
                               │ NIP-34/51, 30078,    │ 1111, 25910,
                               │ 25910, 31990         │ 30619/4903
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
│  │ Manager │  │  (9 core providers)      │  │   Engine     │   │
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

Nostr-native IDE and marketplace paths share the same relay fabric:

```
IDE / Editor
    │
    ├─ kind 30078  NIP-78 session state
    └─ kind 25910  ContextVM review/request + review/apply-fix
           │
           ▼
      IDE Gateway ──▶ Review Pipeline ──▶ Publisher ──kind 25910 result/error──▶ IDE / Editor

Generic Review Orderer (including Loom)
    └─ kind 25910  ContextVM review/order (plain or gift-wrapped)
           │
           ▼
      Review Order Service ──▶ durable claim + payment ──▶ Review Pipeline

Reviewer Marketplace
    │
    ├─ kind 31990  NIP-89 reviewer profiles for discovery
    └─ kind 25910  ContextVM assign/accept/reject/complete requests
                    + marketplace/feedback notifications
           │
           ▼
      Marketplace Router ──▶ Reviewers ──▶ Review Pipeline / Publisher
```

## Dual Invocation Model

Repository eligibility has two axes:

- `monitoring.Registry` is a persisted atomic projection of the operator's NIP-51 kind-30001 `drydock:monitored-repositories:v1` list. It controls only automatic NIP-34 review and changes live.
- The static repository/owner matcher is a hard security ceiling for reactive, IDE, and generic stored-patch review admission. Empty static allowlists allow all, but they do not enable reactive work without a valid monitored-list state.

`revieworder.Service` owns durable review claiming and the bounded wake-up queue for all stored-patch paths. Reactive intake must pass monitoring plus the ceiling. IDE stored-patch requests and generic ContextVM `review/order` bypass monitoring but still pass the ceiling, canonical repository payment policy, and force authorization. Loom interoperability comes from this generic ContextVM contract; Drydock does not subscribe to Loom-local kinds.

A queue send is only a wake-up hint. Claims, ContextVM receipts, invocation metadata, and skipped states live in SQLite. Queue-full orders report `retry_pending`, and startup/failed-review recovery re-enqueues the durable task. Reactive membership is rechecked before work and before publication, so a live removal cannot publish already queued reactive work.

## Service Topology

| Service | Binary | Required | Purpose |
|---------|--------|----------|---------|
| `drydock-core` | `cmd/drydock` | Yes | Full review pipeline: listen → ingest → freeze snapshot → discover context → review → publish |
| `drydock-mcp` | `cmd/drydock-mcp` | No | Stdio or authenticated Streamable HTTP adapter over the canonical agent tool registry |
| `qdrant` | Docker image `qdrant/qdrant:v1.12.6` | No | Vector similarity search for NIP specs, project docs, few-shot examples |
| Embedding server | Ollama or dedicated endpoint | No | Text → vector embeddings (required if Qdrant is enabled) |
| `lsp-bridge` | `cmd/lsp-bridge` | No | Multi-language LSP server manager for type-aware symbol analysis |

**Graceful degradation**: All external services are optional. When unconfigured or unreachable, drydock-core logs a warning and falls back:
- No Qdrant → RAG context layers disabled
- No embedding server → Qdrant cannot be used
- No LSP bridge → symbols extracted via tree-sitter + callsites via ripgrep/git grep
- No signer → listen-only mode (events ingested but no reviews published)

## Event Layers

Drydock models Nostr events in four protocol layers:

| Layer | Purpose | Drydock Examples |
|-------|---------|------------------|
| Observable | Public facts that can be indexed and reacted to | NIP-34 patches/PRs (`1617`–`1619`), comments (`1111`), security reports (`30619`, `4903`) |
| Intent | Addressed requests and one-way notifications | ContextVM `review/order`, IDE and marketplace requests, `marketplace/feedback`, and `security/audit/progress` on `25910` |
| Collection | Discoverable addressable records | Reviewer profiles (`31990`), repository announcements (`30617`), and the operator monitored list (`30001`) |
| State | Replaceable or durable current state | NIP-78 IDE sessions (`30078`), repository snapshots (`30618`), review/order receipts in SQLite |

Private intent payloads should be wrapped with NIP-59 gift-wrap (`1059`) while keeping only non-sensitive routing tags visible.

## Package Map

| Package | Responsibility |
|---------|---------------|
| `listener` | Subscribes to Nostr relays, receives events, manages high-water-mark for restart resilience |
| `ingest` | Verifies signatures, deduplicates events, applies monitoring control-plane events, and routes protocol messages |
| `monitoring` | Persists and atomically projects the winning operator-authored NIP-51 list |
| `revieworder` | Applies reactive/on-demand policy, atomically claims work, stores receipts, and owns the bounded queue |
| `pipeline` | Worker pool that orchestrates review lifecycle and rechecks reactive membership |
| `agenticreview` | Shared snapshot → discovery → exact finalization → iterative review → session service used by pipeline, IDE, and audit |
| `agenttools` | Canonical tool schemas, role/capability policy, snapshot-bound handlers, selection state, and replay isolation |
| `workspacesnapshot` | Immutable pinned-git or mutable-copy snapshots, manifests, leases, expiry, and garbage collection |
| `reviewsession` | Versioned review conversations, idempotent turns, persisted artifacts/messages, and deterministic compaction |
| `mcpserver` | MCP transport adapter with per-connection server-resolved role and snapshot scope |
| `contextbuilder` | Deterministic fallback builder with 9 core providers plus optional retrieval/security providers and a 64K budget |
| `symbols` | Tree-sitter AST parsing for 9 languages (Go, Python, JS, TS, Rust, C, C++, Java, Ruby) — extracts declarations from changed files |
| `reviewengine` | Engine-owned planner, checklist, consensus, severity mapping, finding filtering, and walkthrough around a pluggable reviewer executor |
| `publisher` | Constructs and publishes kind 1111 (NIP-22 comment) Nostr events with relay fanout |
| `metareview` | Self-improvement loop: evaluates review quality and routes feedback for prompt tuning |
| `promptrefine` | Automated prompt versioning: batches prompt gaps, refines via LLM, activates with eval-gated rollback |
| `repo` | Clones/fetches git repositories with URL validation and LRU cache eviction |
| `signing` | Shared `cascadia-go/signet` NIP-46 client → local nsec development fallback |
| `vectorstore` | Qdrant REST API client — CRUD, search, scroll, collection management |
| `embedding` | HTTP client for OpenAI-compatible embedding endpoints |
| `nipingest` | Markdown NIP spec ingestion: chunk by heading, embed, upsert to Qdrant with content-hash dedup |
| `idegateway` | IDE session and ContextVM review/fix protocol handler for kinds `30078` and `25910` |
| `marketplace` | Reviewer discovery plus ContextVM assignment, completion, and authenticated feedback routing |
| `lspbridge` | Shared types + HTTP client for the LSP bridge sidecar |
| `lspbridge/server` | LSP bridge HTTP server: process lifecycle manager, JSON-RPC 2.0 over stdio |
| `db` | SQLite storage with WAL mode, migrations, and all state management queries |
| `health` | HTTP server with `/healthz` (liveness) and `/readyz` (readiness) endpoints |
| `config` | Environment variable parsing with typed defaults |
| `eval` | Held-out evaluation harness for measuring review quality metrics |

## End-to-End Data Flow

Trace of a single patch event from relay to published review:

1. **Receive** — `listener.Service.Run` calls `pool.SubscribeManyNotifyClosed` with a filter for [NIP-34 event kinds](nostr-protocol.md). The pool delivers events on a channel.

2. **Ingest** — `ingest.Processor.ProcessEvent` verifies signatures (or a verified gift-wrap rumor), deduplicates events, persists repository/patch state, and routes ContextVM requests versus notifications. Kind-30001 list replacements and exact kind-5 deletions update `monitoring.Registry` atomically.

3. **Admit** — `revieworder.Service` applies invocation-aware policy:
   - reactive patch: require current monitoring membership and the static ceiling;
   - IDE/generic order: require a stored unique patch target and the static ceiling, but not monitoring membership;
   - load canonical repository payment policy, authorize force, atomically store the claim/order receipt, then emit a bounded queue wake-up;
   - persist `not_monitored` or `monitoring_removed` terminal skips; static-ceiling rejection occurs before a claim.

4. **Pipeline** — `pipeline.Runner.work` picks up the durable task:
   - `repo.Service.PreparePatchSeries` — clones or fetches the repository; for kind 1617 applies the patch series to a throwaway branch, for kind 1618/1619 checks out the PR tip and computes the diff against its merge-base with the default branch (in the canonical clone, so a fork cannot pick the diff base)
   - **Status gate** — re-checks the root's current NIP-34 status against the repo's `review.statuses` config (open by default, drafts opt-in, merged/closed never); status skips are permanent
   - `agenticreview.Service.Prepare` — freezes a pinned snapshot, runs capability-filtered discovery, and accepts context only after `selection.finalize` passes the exact-token gate; the explicit rollout fallback uses `contextbuilder.Builder.Build` and then passes its serialized package through the same authoritative exact-token gate
   - `agenticreview.Service.ReviewPrepared` — the engine plans once, then an iterative reviewer uses read-only snapshot tools and must finish with evidence-backed `review.submit`; consensus, filtering, and one post-consensus walkthrough remain engine-owned
   - **Pipeline-owned post-review stages** — deterministic security scans, security verification, deduplication, repository-policy filtering, final target filtering, and optional auto-fix remain outside the shared service
   - `publisher.Service.PublishReview` — builds kind 1111 events (labeled with the model the endpoint actually served), signs them, fans out to relays
   - `metareview.Service.RunAsync` — asynchronous quality evaluation (non-blocking)

5. **Publish** — The publisher resolves target relays (patch-seen relays + repo announcement relays + defaults), constructs summary and detail comment events, signs each, and publishes.

## Agentic Review Data Flow

```
authoritative patch + repository state
                │
                ▼
       immutable workspace snapshot
       (pinned git or copy + hashes)
                │
                ▼
     discovery loop (read + selection tools)
                │
                ▼
 selection.finalize ──exact tokens over limit──▶ prune and retry
                │ success
                ▼
        frozen ContextBundle
                │
                ▼
 planner ──▶ iterative reviewer ──▶ review.submit
                │                         │
                └──── engine-owned consensus, scope validation,
                      severity mapping, walkthrough, publication
```

All three orchestration families call `internal/agenticreview.Service`. Pipeline and audit snapshots pin a Git commit; IDE snapshots copy and hash the mutable workspace before any tool call. An IDE pubkey may freeze only an exact, canonical workspace root assigned to it by `DRYDOCK_IDE_WORKSPACE_BINDINGS`; unbound, relative, nonexistent, non-directory, and workspace-rebinding requests cannot start an inline filesystem review. Audit scanning, localization, verification, and progress reporting remain audit-owned around the shared service. Sessions persist the finalized artifact manifest and re-render code context from that same snapshot on every continuation; broken or expired sessions are rejected before snapshot restore or model execution.

MCP HTTP currently exposes only the `external_readonly` role. Authentication resolves a server-created session and snapshot before a tool list is returned; clients cannot choose or replace roles, capabilities, sessions, or roots. Listing and dispatch both enforce capability policy, and path resolution rejects absolute paths, traversal, symlinks, and live-workspace fallback.

## Deterministic Context Provider Flow

The deterministic builder remains the rollout and discovery-exhaustion fallback. It has **9 core providers**; optional retrieval and security providers are appended by configuration.

```
Patch diff
    │
    ├─→ priority 1: patch (raw diff, 40 KB cap)
    ├─→ priority 2: modified-files
    ├─→ priority 2: change-impact
    ├─→ priority 2: taint
    ├─→ priority 3: symbols
    ├─→ priority 4: tests
    ├─→ priority 5: imports-exports
    ├─→ priority 6: commit-history
    ├─→ priority 7: project-docs
    └─→ priority 8: optional qdrant/chartroom retrieval
```

The deterministic builder truncates priority-1/2 content when a useful prefix fits, otherwise drops an oversized layer and continues considering later layers. Its repository file reads normalize relative paths and reject absolute paths, traversal, NULs, and every symlink component. Whether invoked by the rollout flag or discovery exhaustion, its serialized output must pass the same authoritative exact-token gate before review. Agentic discovery instead mutates a selection and passes that gate through `selection.finalize`.

## Signer Chain

Drydock tries signers in priority order. The first successful signer wins:

```
1. Shared NIP-46 client (DRYDOCK_SIGNER_BUNKER_URL set)
        │ fail/skip
        ▼
2. Local nsec        (DRYDOCK_SIGNER_NSEC set; development only)
        │ fail/skip
        ▼
3. No signer → listen-only mode (warning logged)
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
                 │ pending  │ ← atomic review/order claim
                 └──┬────┬──┘
        admission/  │    │ pipeline picks up task
        live removal│    ▼
             ▼      │ ┌──────────┐
        ┌─────────┐ │ │reviewing │
        │ skipped │ │ └──┬───┬───┘
        └─────────┘ │    │   │
                    │ success failure
                    ▼       ▼
              ┌──────────┐ ┌──────────┐
              │published │ │  failed  │
              └──────────┘ └──────────┘
```

**Recovery**: On startup, `store.ResetStuckReviews` moves any rows stuck in `reviewing` back to `pending`. This handles crashes mid-review.

## Concurrency Model

- **Listener**: Reads relay events, opens verified gift wraps, and synchronously hands events to ingest before advancing the cursor.
- **Monitoring**: Registry updates are serialized for persistence and publish immutable atomic snapshots for lock-free membership reads.
- **Review ordering**: One bounded channel owned by `revieworder.Service`; durable claims remain authoritative when a wake-up cannot be queued.
- **Pipeline workers**: `N` goroutines (configurable via `DRYDOCK_PIPELINE_WORKERS`, default 2). Each reads from the shared review-order queue. Workers drain on context cancellation via `sync.WaitGroup`.
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
| `review_log` | Invocation-aware review state (`pending → reviewing → published \| failed \| skipped`) with requester/order metadata |
| `review_orders` | Immutable ContextVM order receipts keyed by requester and JSON-RPC ID |
| `review_skips` | Durable terminal skip reasons |
| `monitored_repository_list_state` | Winning list/deletion revision and raw event |
| `monitored_repository_members` | Active canonical repository addresses |
| `meta_review_log` | Meta-review results with context hashes |
| `meta_review_routes` | Feedback routing decisions from meta-reviews |
| `few_shot_reviews` | Positive/negative few-shot examples for prompt improvement |
| `listener_state` | High-water-mark timestamp for restart resilience |
| `eval_runs` | Evaluation harness run results and metrics |
| `review_snapshots` | Snapshot metadata, manifest/diff hashes, expiry, and reference count |
| `review_sessions` | Opaque chat IDs, owners, modes, snapshot binding, state, lease, and optimistic version |
| `review_session_artifacts` | Ordered immutable selection artifacts and content hashes |
| `review_session_turns` | Request-ID idempotency, expected versions, results, and failures |
| `review_session_messages` | Normalized ordered assistant/tool transcript messages |

Full DDL is in [`internal/db/schema.go`](../internal/db/schema.go).

## High-Water-Mark Resilience

The listener persists the `created_at` timestamp of the most recent event it processed into the `listener_state` table. On restart:

1. Read the persisted high-water-mark
2. Subtract 30 seconds to handle clock skew between relays
3. Use whichever is earlier: the adjusted high-water-mark or the configured lookback window

This ensures no events are missed across restarts, at the cost of re-processing a small overlap window (deduplicated by `ingested_events`).
