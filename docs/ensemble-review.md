# Multi-Model Ensemble Review

Drydock supports running code reviews across multiple LLM models simultaneously, merging their findings with consensus-based confidence scoring. This improves accuracy by leveraging diverse model perspectives and reducing false positives through agreement.

## How It Works

```
                    ┌────────────┐
                    │   Patch    │
                    └─────┬──────┘
                          │
                          ▼
              ┌───────────────────────┐
              │   Ensemble Orchestrator │
              └───────────┬───────────┘
                          │
          ┌───────────────┼───────────────┐
          │               │               │
          ▼               ▼               ▼
    ┌──────────┐    ┌──────────┐    ┌──────────┐
    │ Model A  │    │ Model B  │    │ Model C  │
    │ (Claude) │    │(GPT-4o)  │    │ (Llama)  │
    └────┬─────┘    └────┬─────┘    └────┬─────┘
         │               │               │
         └───────────────┼───────────────┘
                         │
                         ▼
              ┌───────────────────────┐
              │   Finding Merger      │
              │  (dedup + consensus)  │
              └───────────┬───────────┘
                          │
                          ▼
              ┌───────────────────────┐
              │  Final Review Output  │
              │  (boosted confidence) │
              └───────────────────────┘
```

1. **One shared preparation**: The planner, checklist, and finalized context package are built once
2. **Isolated parallel execution**: A `ReviewerExecutorFactory` creates a fresh executor for each configured route, so transcripts, evidence ledgers, counters, and replay caches cannot leak between members
3. **Finding merge**: Duplicate findings (same file, nearby line, category) are merged
4. **Consensus boost**: Findings confirmed by multiple models receive confidence boosts while retaining the highest canonical P0/P1/P2 priority
5. **One walkthrough**: Consensus and scope validation run before a single optional walkthrough; members do not generate their own walkthroughs

## Configuration

Enable ensemble mode in your repository's `.drydock.yml`:

```yaml
ensemble:
  enabled: true
  models: [coder32b, llm70b]
  consensus_boost: 0.10      # Confidence boost per additional model
  require_consensus: false    # If true, only publish findings with 2+ model agreement
```

Ensemble configuration is repository-scoped; there are no `DRYDOCK_ENSEMBLE_*` environment variables.

## Model Routes

`models` accepts Drydock's configured review-engine route aliases:

| Route | Endpoint configuration |
|-------|------------------------|
| `coder14b` | `DRYDOCK_CODER14B_BASE_URL`, `DRYDOCK_CODER14B_MODEL` |
| `coder32b` | `DRYDOCK_CODER32B_BASE_URL`, `DRYDOCK_CODER32B_MODEL` |
| `llm70b` | `DRYDOCK_LLM70B_BASE_URL`, `DRYDOCK_LLM70B_MODEL` |

When ensemble mode is enabled with no models, it defaults to `coder32b` and `llm70b`. `consensus_boost` defaults to `0.10` and must be in `[0, 0.5]`.

## Consensus Scoring

When multiple models identify the same finding, confidence is boosted:

```
final_confidence = base_confidence + (consensus_boost × (agreeing_models - 1))
```

Example with `consensus_boost: 0.15`:
- 1 model finds issue at 0.75 confidence → 0.75
- 2 models agree → 0.75 + 0.15 = 0.90
- 3 models agree → 0.75 + 0.30 = 1.00 (capped)

## Finding Deduplication

Findings are considered duplicates if they match on:
- **File path**: Exact match
- **Line number**: Within ±2 lines
- **Category**: Same category (e.g., "security", "performance")

When duplicates are found:
1. The finding with highest original confidence supplies the explanatory fields
2. The cluster retains the highest canonical priority reported by any member (P0 over P1 over P2), and legacy severity is re-derived from that priority
3. Consensus boost is applied based on agreeing model count
4. Final findings are deterministically sorted by canonical priority, confidence, path, and line

## Metrics

Ensemble mode exposes additional Prometheus metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `drydock_ensemble_reviews_run_total` | Counter | Reviews executed in ensemble mode |
| `drydock_ensemble_models_used_total` | Counter (labeled) | Per-model usage count |
| `drydock_ensemble_findings_merged_total` | Counter | Findings merged from multiple models |
| `drydock_ensemble_consensus_boost_total` | Counter | Findings that received consensus boost |

## Best Practices

1. **Model Diversity**: Use models with different training data and architectures for better coverage
2. **Performance Budget**: Each additional model adds latency; 2-3 models is typical
3. **Cost Management**: Mix expensive cloud models with local models for balance
4. **Require Consensus for Noise**: Enable `require_consensus: true` for noisy codebases

## Fallback Behavior

If an ensemble member fails during review:
- The failed member is dropped; remaining members continue independently
- The result records required, succeeded, and failed reviewers, per-member traces, and `degraded: true`
- The run fails if every member fails; it does **not** start a separate single-model fallback
- Parent cancellation or deadline fails the entire run and returns no partial review, even if a member already succeeded
- Failures are logged with their model route and retained trace

## Example Output

```json
{
  "findings": [
    {
      "file": "src/auth.go",
      "line": 42,
      "priority": "P0",
      "severity": "critical",
      "category": "security",
      "explanation": "SQL injection vulnerability in user input",
      "confidence": 0.95
    }
  ]
}
```
