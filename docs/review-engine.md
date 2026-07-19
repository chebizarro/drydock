# Review Engine

The review engine is a two-stage LLM pipeline: a lightweight **planner** selects the appropriate model and review focus, then a **reviewer** produces structured findings.

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
| `model_route` | enum (required) | One of `coder32b`, `llm70b`, `coder14b` |

## Model Routing

The planner selects one of three model routes based on patch complexity:

| Route | Typical Model | Use Case |
|-------|--------------|----------|
| `coder14b` | Qwen 2.5 Coder 14B | Simple patches, style changes, documentation |
| `coder32b` | Qwen 2.5 Coder 32B | Complex code logic, multi-file refactors, API changes |
| `llm70b` | Llama 3.3 70B | Architecture review, security analysis, nuanced reasoning |

Each route maps to independently configurable endpoint and model name via environment variables. See [Configuration](configuration.md#llm-endpoints).

Route aliases are internal. Published reviews are labeled with the model the
endpoint **actually reported serving** for that run: every chat-completion
response's `model` field is carried through `RunOutput.ServedModel`, with a
fallback to the served-model registry (seeded by a startup `/v1/models`
probe of each endpoint) and finally the configured model name.

## Reviewer Stage

The reviewer receives the context bundle, the planner's analysis, an auto-generated checklist, and optional few-shot examples. It outputs structured findings:

```json
{
  "summary": "The patch adds concurrent map access without synchronization.",
  "findings": [
    {
      "severity": "high",
      "category": "concurrency",
      "file": "cache/store.go",
      "line": 15,
      "evidence": "func (s *Store) Set(key, value string) { s.items[key] = value }",
      "explanation": "Map write without mutex in a type documented as concurrent-safe.",
      "suggestion": "Add sync.RWMutex and lock around map operations.",
      "confidence": 0.92
    }
  ],
  "needs_more_context": []
}
```

### Finding Schema

| Field | Type | Constraints | Description |
|-------|------|------------|-------------|
| `severity` | enum | `critical`, `high`, `medium`, `low`, `info` | Impact level |
| `category` | enum | `security`, `correctness`, `architecture`, `style`, `test-coverage` | Finding type |
| `file` | string | required, non-empty | Affected file path |
| `line` | integer | required, > 0 | Line number in the file |
| `evidence` | string | | Code snippet demonstrating the issue |
| `explanation` | string | | Why this is a problem |
| `suggestion` | string | | Recommended fix |
| `confidence` | float | [0.0, 1.0] | Model's confidence. Below 0.6 requires `needs_more_context` |

### Validation Rules

- `summary` is required and non-empty
- Each finding must have valid `severity` and `category` enum values
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
- After the reviewer (and after ensemble merging), findings and walkthrough
  `file_summaries` whose paths are not in the deterministic changed-file set
  are dropped and logged. Only the parsed diff is authoritative for what
  changed.

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

## Retry Behavior

LLM calls are wrapped in a `RetryingClient` with exponential backoff:

| Parameter | Default | Description |
|-----------|---------|-------------|
| Max attempts | 3 | Total tries including the first |
| Base delay | 2 seconds | Initial backoff delay |
| Max delay | 30 seconds | Backoff cap |

**Transient errors** (retried): HTTP 429, HTTP 5xx, network timeouts, connection refused, DNS failures.

**Non-transient errors** (fail immediately): HTTP 4xx (except 429), context cancellation, malformed responses.

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
