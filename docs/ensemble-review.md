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

1. **Parallel Execution**: The patch is sent to 2-3 configured models concurrently
2. **Independent Reviews**: Each model produces findings independently
3. **Finding Merge**: Duplicate findings (same file, line, category) are merged
4. **Consensus Boost**: Findings confirmed by multiple models receive confidence boosts

## Configuration

Enable ensemble mode in your repository's `.drydock.yml`:

```yaml
ensemble:
  enabled: true
  models:
    - route: "anthropic/claude-3.5-sonnet"
    - route: "openai/gpt-4o"
    - route: "local/llama-3.1-70b"
  consensus_boost: 0.15      # Confidence boost per additional model
  require_consensus: false    # If true, only publish findings with 2+ model agreement
```

Or via environment variables:

```bash
DRYDOCK_ENSEMBLE_ENABLED=true
DRYDOCK_ENSEMBLE_MODELS="anthropic/claude-3.5-sonnet,openai/gpt-4o,local/llama-3.1-70b"
DRYDOCK_ENSEMBLE_CONSENSUS_BOOST=0.15
DRYDOCK_ENSEMBLE_REQUIRE_CONSENSUS=false
```

## Model Routes

Model routes follow the format `provider/model-name`:

| Provider | Route Example | Notes |
|----------|---------------|-------|
| `anthropic` | `anthropic/claude-3.5-sonnet` | Requires `ANTHROPIC_API_KEY` |
| `openai` | `openai/gpt-4o` | Requires `OPENAI_API_KEY` |
| `local` | `local/llama-3.1-70b` | Uses `DRYDOCK_LLM70B_BASE_URL` |
| `ollama` | `ollama/codellama:34b` | Uses local Ollama instance |

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
1. The finding with highest original confidence is used as the base
2. Consensus boost is applied based on agreeing model count
3. Message content from the primary finding is preserved

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

If a model fails during ensemble review:
- Remaining models continue independently
- If all models fail, the review falls back to single-model mode
- Failures are logged with model route for debugging

## Example Output

```json
{
  "findings": [
    {
      "file": "src/auth.go",
      "line": 42,
      "category": "security",
      "message": "SQL injection vulnerability in user input",
      "confidence": 0.95,
      "models_agreed": 3,
      "consensus_boosted": true
    }
  ]
}
```
