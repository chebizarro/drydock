# Drydock

Drydock is an automated NIP-34 code review agent that ingests patch and PR events from Nostr, builds repository context, runs local model reviews, and publishes structured kind `1111` review comments.

## Current bootstrap scope

- Go service skeleton with package boundaries for listener/ingest/db.
- SQLite schema for:
  - `repositories`
  - `patch_events`
  - `review_events`
  - `thread_cache`
  - `review_log`
  - `meta_review_log`
- Idempotency/state transition API for `review_log`:
  - `pending -> reviewing -> published|failed`
  - unique `(patch_event_id, repo_id)` gate
- Nostr listener wired to kinds:
  - `30617`, `30618`
  - `1617`, `1618`, `1619`
  - `1621`, `1111`
  - `1630-1633`
  - `1985`
- Ingestion path reuses `fiatjaf.com/nostr/nip34` for repository and patch parsing.
- 30618 snapshot integration:
  - stores latest repository state per repo (`repository_snapshots`)
  - skips review queue enqueue when patch/PR tip commit is already present in latest snapshot refs
- Deterministic context builder module (`internal/contextbuilder`):
  - strict priority layers with hard 64K token budget
  - deterministic drop behavior (`context-layers-used` / `context-layers-dropped`)
  - exclusion rules for generated/binary/lock artifacts
- LLM planner+reviewer routing engine (`internal/reviewengine`):
  - planner JSON output with explicit `model_route` (`coder32b|llm70b|coder14b`)
  - route-aware reviewer execution against OpenAI-compatible local endpoints
  - file-type checklist injection and security-sensitive prompt augmentation
  - strict reviewer findings schema validation
- Review publisher (`internal/publisher`):
  - builds and publishes kind `1111` summary comments (+ high/critical detail comments)
  - includes required metadata footer fields (`context-layers-dropped` always present)
  - relay fanout = patch-seen relays + repository announcement relays
  - explicit guard to never emit status kinds `1631` / `1632`
- Bunker signer helper (`internal/signing`):
  - supports NIP-46 bunker / NIP-05 bunker identity inputs only (no embedded nsec requirement path)
- Meta-review pipeline (`internal/metareview`):
  - gating: low confidence, security-sensitive paths, random baseline sample
  - context-hash dedupe with changed-line Jaccard reuse
  - persisted critique logs + feedback routing decisions
  - few-shot positive/negative example updates with cap pruning
- Held-out eval harness (`internal/eval` + `cmd/drydock-eval`):
  - dataset-driven monthly run flow with persisted `eval_runs`
  - metrics: recall, false-positive rate, confidence calibration (MAE), high-confidence precision
  - sample dataset scaffold at `eval/heldout-sample.json`

## Run

```bash
go run ./cmd/drydock
```

## Run eval harness

```bash
go run ./cmd/drydock-eval
```

## Container deployment

1. Copy env template:
```bash
cp .env.example .env
```
2. Build and run:
```bash
docker compose up --build -d
```

Listener mode uses `DRYDOCK_MODE=listener` (default).  
Eval mode uses `DRYDOCK_MODE=eval`.

### Makefile shortcuts

```bash
make up      # build + start listener service
make logs    # follow logs
make eval    # run eval harness in container
make down    # stop stack
```

## Environment

- `DRYDOCK_DATABASE_URL` (default: `file:drydock.db?...`)
- `DRYDOCK_RELAYS` (comma-separated)
- `DRYDOCK_LOG_LEVEL` (`debug|info|warn|error`)
