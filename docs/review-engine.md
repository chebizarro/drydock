# Review Engine

The review engine owns planning, checklist construction, consensus, scope validation, severity compatibility, and walkthrough generation. Its reviewer stage is pluggable: the default agentic executor iterates over frozen-snapshot tools and can succeed only through `review.submit`; the legacy single-shot executor remains available during rollout. See [Agentic Review Operations](agentic-review.md) for tool policy, configuration, failure handling, and rollout.

## Pipeline Overview

```
Patch Context Bundle
        │
        ▼
  ┌──────────┐     PlannerOutput (JSON)
  │  Planner  │─────────────────────────┐
  │  (14B)    │                         │
  └──────────┘                         ▼
                                 model_route
                              ┌─────────────┐
                     coder14b │   coder32b   │ llm70b
                              └──────┬──────┘
                                     │
                                     ▼
                              ┌──────────────┐
                              │   Reviewer    │
                              │ (routed model)│
                              └──────┬───────┘
                                     │
                                     ▼
                              ReviewerOutput (JSON)
                              • summary
                              • findings[]
                              • needs_more_context[]
```

## Agentic Reviewer Loop

```
model response
    │
    ├─ read/search/git tool calls ──▶ frozen snapshot
    │                                  │
    │◀──────── bounded tool results ───┘
    │
    ├─ invalid review.submit ──▶ structured tool error, continue
    │
    └─ valid review.submit ──▶ validated ReviewerOutput
```

`review.submit` is the only successful stop condition. Every finding cites successful tool-call IDs from the current member's evidence ledger and includes explicit examined-file coverage. Turn, tool-call, cumulative-token, model-context, and tool-result limits are enforced independently.

## Planner Stage

The planner receives the full context bundle and changed file list, then outputs a structured JSON plan:

```json
{
  "change_type": "feature",
  "risk_areas": ["concurrency", "error-handling"],
  "needed_context": ["test coverage for new mutex"],
  "review_focus": "Thread safety of the new cache layer",
  "model_route": "coder32b"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `change_type` | string (required) | Nature of the change (feature, bugfix, refactor, etc.) |
| `risk_areas` | string[] | Identified risk categories |
| `needed_context` | string[] | Additional context the reviewer should look for |
| `review_focus` | string | Primary focus instruction for the reviewer |
| `model_route` | enum (required) | General routes `coder32b`, `llm70b`, `coder14b`; security routes `sec70b`, `secclassify`, `seclocalize` |

## Model Routing

The planner selects one of three model routes based on patch complexity:

| Route | Typical Model | Use Case |
|-------|--------------|----------|
| `coder14b` | Qwen 2.5 Coder 14B | Simple patches, style changes, documentation |
| `coder32b` | Qwen 2.5 Coder 32B | Complex code logic, multi-file refactors, API changes |
| `llm70b` | Llama 3.3 70B | Architecture review and nuanced reasoning |
| `sec70b` | Security-specialized 70B | Full security-audit reviewer |
| `secclassify` | Security classifier | Candidate classification |
| `seclocalize` | Security localizer | Finding localization |

Each route maps to independently configurable endpoint and model name via environment variables. See [Configuration](configuration.md#llm-endpoints).

Route aliases are internal. Published reviews are labeled with the model the
endpoint **actually reported serving** for that run: every chat-completion
response's `model` field is carried through `RunOutput.ServedModel`, with a
fallback to the served-model registry (seeded by a startup `/v1/models`
probe of each endpoint) and finally the configured model name.

## Reviewer Stage

The reviewer receives the finalized context bundle, planner analysis, an auto-generated checklist, optional few-shot examples, and any compacted session history. In agentic mode it submits evidence-backed structured findings:

```json
{
  "summary": "The patch adds concurrent map access without synchronization.",
  "findings": [
    {
      "priority": "P1",
      "category": "concurrency",
      "file": "cache/store.go",
      "line": 15,
      "evidence_tool_call_ids": ["call-read-store"],
      "explanation": "Map write without mutex in a type documented as concurrent-safe.",
      "suggestion": "Add sync.RWMutex and lock around map operations.",
      "confidence": 0.92
    }
  ],
  "coverage": {
    "examined_files": ["cache/store.go"],
    "outcome": "findings",
    "summary": "Read the changed store and its callers."
  }
}
```

### Finding Schema

| Field | Type | Constraints | Description |
|-------|------|------------|-------------|
| `priority` | enum | `P0`, `P1`, `P2` | Canonical agentic priority |
| `severity` | enum | P0→`critical`, P1→`high`, P2→`medium` | Derived legacy compatibility field |
| `category` | enum | `security`, `correctness`, `architecture`, `style`, `test-coverage` | Finding type |
| `file` | string | required, non-empty | Affected file path |
| `line` | integer | required, > 0 | Line number in the file |
| `evidence_tool_call_ids` | string[] | one or more successful current-run calls | Evidence provenance; converted to legacy `evidence` text |
| `explanation` | string | | Why this is a problem |
| `suggestion` | string | | Recommended fix |
| `confidence` | float | [0.0, 1.0] | Model's confidence. Below 0.6 requires `needs_more_context` |

### Validation Rules

- `summary` is required and non-empty
- Agentic findings require canonical P0/P1/P2 priority, a valid category, and current-run evidence tool-call IDs
- Priority/severity normalization is total: P0/critical, P1/high, P2/medium; conflicting pairs are rejected
- `file` must be non-empty and `line` must be positive
- `confidence` must be in [0, 1]
- If any finding has `confidence < 0.6`, the `needs_more_context` array must be non-empty

### Changed-File Anchoring

The reviewer sees contextual layers (project docs, related code) alongside
the diff, and can hallucinate findings against files the change never
touched. Two deterministic guards run outside the LLM:

- The pipeline **fails closed before any LLM call** when the changed-file
  set parsed from the diff is empty — a review anchored to nothing but
  context would be baseless.
- After the reviewer (and after ensemble merging), patch-review findings and walkthrough `file_summaries` outside the deterministic changed-file set are dropped and logged.
- Security-audit mode instead validates findings against the full frozen snapshot root, so verified issues may be reported in unchanged files.

## Checklist Injection

Before the reviewer call, `BuildChecklist` generates review checklist items based on changed file paths:

| File Pattern | Injected Checklist Item |
|-------------|----------------------|
| `*.sql`, `*query*`, `*orm*` | SQL injection and unsafe query construction checks |
| `*auth*`, `*session*`, `*permission*` | Session management and privilege escalation checks |
| `*crypto*`, `*cipher*`, `*sign*` | Timing attack, key handling, nonce/IV reuse checks |
| `*handler*`, `*input*`, `*request*` | Input sanitization and validation checks |
| `*migration*`, `*schema*` | Migration rollback safety and constraint violation checks |

If any changed file matches `auth`, `crypto`, or `security`, the reviewer prompt is augmented with additional data-flow tracing instructions.

## Ensemble and Session Behavior

Ensemble members share one finalized package but receive fresh reviewer executors from a `ReviewerExecutorFactory`. Transcripts, evidence ledgers, counters, and replay caches are isolated per member. Ordinary member failures are dropped and reported as degraded status; the run fails only when every member fails. Parent cancellation or deadline fails the whole run with no partial review, even if one member already succeeded. During deduplication, explanatory fields come from the highest-confidence representative, but the merged cluster retains the highest canonical priority (P0 over P1 over P2) and derives compatible legacy severity from it. Consensus runs before one optional walkthrough.

Stateful IDE follow-ups persist an opaque 128-bit `chat_id`, owner, snapshot hashes, ordered artifacts, turns, and normalized messages. Each continuation supplies `expected_version`, a unique request ID, and a message. The store applies optimistic version checks and request-ID idempotency before the reviewer runs. Broken and expired sessions are rejected before snapshot restore or model execution. Code context is re-rendered from the frozen snapshot and is never silently dropped during history compaction.

## Retry Behavior

LLM calls are wrapped in a `RetryingClient` with exponential backoff:

| Parameter | Default | Description |
|-----------|---------|-------------|
| Max attempts | 3 | Total tries including the first |
| Base delay | 2 seconds | Initial backoff delay |
| Max delay | 30 seconds | Backoff cap |

**Transient errors** (retried): HTTP 429, HTTP 5xx, network timeouts, connection refused, DNS failures.

**Non-transient errors** (fail immediately): HTTP 4xx (except 429), context cancellation/deadline, malformed responses. Agentic loops classify cancellation and deadlines as `StopCancelled` and return no partial review.

Backoff formula: `baseDelay × 2^attempt`, capped at `maxDelay`.

## Adding a New Model Route

To add a new route (e.g., `coder70b`):

1. Add the constant in `internal/reviewengine/types.go`:
   ```go
   RouteCoder70B ModelRoute = "coder70b"
   ```

2. Add the endpoint field in `internal/reviewengine/engine.go`:
   ```go
   type Config struct {
       // ...
       Coder70B ModelEndpoint
   }
   ```

3. Add the switch case in `routeEndpoint` in `engine.go`

4. Add env vars in `internal/config/config.go`:
   ```go
   Coder70BBaseURL: envOrDefault("DRYDOCK_CODER70B_BASE_URL", "..."),
   Coder70BModel:   envOrDefault("DRYDOCK_CODER70B_MODEL", "..."),
   ```

5. Wire the config in `cmd/drydock/main.go`
