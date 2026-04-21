# Meta-Review Pipeline

The meta-review is a self-improvement loop. After the primary review is published, a second LLM pass evaluates whether the review missed findings or produced false positives, then routes feedback to improve prompts and context for future reviews.

## Gating Logic

Meta-reviews are expensive (an additional LLM call per patch). They are triggered when any of these conditions is met:

| Trigger | Threshold | Rationale |
|---------|-----------|-----------|
| Low confidence | Mean finding confidence < 0.7 | Model was uncertain — worth a second opinion |
| Security-sensitive path | Any changed file matches `auth`, `crypto`, `security` | Security findings have high impact if missed |
| Random sample | 15% of all reviews (`RandomSampleRate`) | Baseline quality measurement across all patch types |

If none of these conditions are met, the meta-review is skipped.

## Context-Hash Dedup

To avoid re-running expensive meta-reviews for similar patches, the pipeline checks for prior meta-reviews with the same context hash:

1. Compute SHA-256 of the full context bundle → `context_hash`
2. Extract changed lines from the diff (lines starting with `+` or `-`), sort and deduplicate
3. Query `meta_review_log` for prior entries with the same `context_hash`
4. If a match exists, compute Jaccard similarity between the current and prior changed-line sets
5. If Jaccard ≥ `MinReuseJaccard` (default 0.85), reuse the prior meta-review response

The Jaccard similarity function: `|intersection| / |union|` over the two sets of changed lines.

## Meta-Review Output

The meta-review LLM produces a structured JSON response:

```json
{
  "missed_findings": [
    {
      "type": "security",
      "description": "SQL injection via string concatenation",
      "evidence": "fmt.Sprintf(\"SELECT ... WHERE name = '%s'\", name)",
      "why_missed": "insufficient_context"
    }
  ],
  "false_positives": [
    {
      "finding_index": 2,
      "reason": "The function is only called from a single-threaded init path"
    }
  ],
  "reasoning_quality": 0.75,
  "context_utilization": 0.82,
  "prompt_gaps": ["No instruction to check for SQL injection in non-SQL files"],
  "suggested_few_shot": true
}
```

| Field | Type | Range | Description |
|-------|------|-------|-------------|
| `missed_findings` | array | — | Findings the primary review should have caught |
| `false_positives` | array | — | Primary findings that were incorrect |
| `reasoning_quality` | float | [0, 1] | How well the primary review reasoned about the code |
| `context_utilization` | float | [0, 1] | How effectively the context bundle was used |
| `prompt_gaps` | string[] | — | Specific prompt deficiencies identified |
| `suggested_few_shot` | bool | — | Whether this review should become a few-shot example |

## Feedback Routing

Each missed finding includes a `why_missed` reason that maps to an action:

| `why_missed` | Action | Meaning |
|-------------|--------|---------|
| `insufficient_context` | `flag-context-builder-pattern` | The context builder layers may need tuning for this file pattern |
| `model_limitation` | `flag-model-routing-review` | The planner may have selected the wrong model route |
| `prompt_gap` | `queue-prompt-refinement` | The system prompt needs a new instruction |

These actions are persisted to `meta_review_routes` for analysis. They are currently informational — automatic prompt modification is not yet implemented.

## Few-Shot Example Management

When `suggested_few_shot` is true, the meta-review output becomes a few-shot example for future reviews:

- **Positive examples**: Added when `reasoning_quality` is high — used to teach the reviewer what good analysis looks like
- **Negative examples**: Added when `false_positives` is non-empty — used to calibrate against over-reporting

Examples are stored in the `few_shot_reviews` table and injected into the reviewer system prompt via `RunInput.FewShot`.

### Cap Management

The `FewShotCap` (default 500) limits the total number of stored examples. When exceeded, `PruneFewShotToCap` removes the oldest examples by `created_at`.

## Concurrency

Meta-reviews run asynchronously via `RunAsync`, which spawns a goroutine. A `semaphore.Weighted` with `MaxConcurrent` (default 10) prevents goroutine explosion under high load.

The semaphore is acquired inside the goroutine, so the caller (`pipeline.Runner.process`) never blocks waiting for meta-review capacity.

## Configuration

Meta-review behavior is configured in code via `metareview.Config`:

| Field | Default | Description |
|-------|---------|-------------|
| `RandomSampleRate` | 0.15 | Fraction of reviews to meta-review randomly |
| `MinReuseJaccard` | 0.85 | Minimum Jaccard similarity for dedup reuse |
| `FewShotCap` | 500 | Maximum stored few-shot examples |
| `MaxConcurrent` | 10 | Maximum concurrent meta-review goroutines |

These are not currently exposed as environment variables — they are set in `cmd/drydock/main.go`.
