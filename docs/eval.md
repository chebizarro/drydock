# Evaluation Harness

The evaluation harness measures review quality against a labeled dataset. Run it periodically to detect regressions when changing models, prompts, or context builder behavior.

## Running

```bash
# Native
go run ./cmd/drydock-eval

# Docker
make eval
```

Results are printed to stdout as JSON and persisted in the `eval_runs` SQLite table.

## Metrics

| Metric | Formula | Description |
|--------|---------|-------------|
| **Recall** | TP / (TP + FN) | What fraction of expected findings were found |
| **False Positive Rate** | FP / (TP + FP) | What fraction of predicted findings were wrong |
| **Calibration MAE** | mean \|confidence − label\| | How well-calibrated the confidence scores are (lower is better) |
| **High-Confidence Precision** | TP / (TP + FP) for confidence ≥ 0.8 | Precision among findings the model was most confident about |

Where:
- **TP (True Positive)**: A predicted finding that matches an expected finding
- **FP (False Positive)**: A predicted finding with no matching expected finding
- **FN (False Negative)**: An expected finding with no matching prediction

## Matching Logic

A predicted finding matches an expected finding when all three match:
- `category` (case-insensitive)
- `file` (path-normalized)
- `line` (exact)

Matching is one-to-one: once an expected finding is matched, it cannot match a second prediction.

## Dataset Format

The dataset is a JSON file with this schema:

```json
{
  "id": "dataset-name-v1",
  "cases": [
    {
      "case_id": "unique-case-id",
      "patch_diff": "diff --git a/file.go b/file.go\n...",
      "changed_files": ["file.go"],
      "context_bundle": "Pre-built context string for LLM input",
      "expected_findings": [
        {
          "category": "security",
          "file": "file.go",
          "line": 42,
          "severity": "high"
        }
      ]
    }
  ]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `case_id` | string | Unique identifier for the test case |
| `patch_diff` | string | Unified diff content (as it would appear in a patch event) |
| `changed_files` | string[] | List of files modified by the patch |
| `context_bundle` | string | Pre-built context (bypasses the context builder — no repo checkout needed) |
| `expected_findings` | array | Ground-truth findings that the reviewer should detect |

### Clean-Pass Cases

Include cases with `"expected_findings": []` to measure the false positive rate. A good target is at least one clean-pass case per five finding cases.

## Adding Test Cases

Edit `eval/heldout-sample.json`. The current dataset includes 5 cases covering:

- **Security**: Empty token validation, SQL injection via string concatenation
- **Correctness**: Swallowed error handling
- **Concurrency**: Unsynchronized map access
- **Clean pass**: Benign refactor with no expected findings

When adding cases:
- Use realistic diffs (not toy examples)
- Set `context_bundle` to include enough context for the reviewer to reason about the code
- Be precise with `line` numbers — they must match the diff
- Include the `severity` field for completeness, though matching currently ignores it

## Persisted Results

Each eval run is written to the `eval_runs` table:

| Column | Type | Description |
|--------|------|-------------|
| `dataset_id` | string | Dataset identifier |
| `total_cases` | integer | Number of test cases |
| `recall` | float | Overall recall |
| `false_positive_rate` | float | Overall FPR |
| `calibration_mae` | float | Confidence calibration error |
| `high_conf_precision` | float | Precision for high-confidence findings |
| `details_json` | string | Full metrics JSON for detailed analysis |
| `created_at` | integer | Unix timestamp |

Query historical runs with:

```sql
SELECT created_at, recall, false_positive_rate, calibration_mae
FROM eval_runs
ORDER BY created_at DESC
LIMIT 10;
```

## Custom Review Runner

The eval harness accepts any implementation of the `ReviewRunner` interface:

```go
type ReviewRunner interface {
    ReviewCase(ctx context.Context, in RunCaseInput) (reviewengine.ReviewerOutput, error)
}
```

The default `EngineRunner` delegates to the full review engine. You can substitute a mock or a different model for comparison testing:

```go
h := eval.Harness{
    Runner: myCustomRunner{},
    Store:  store,
    Logger: logger,
}
```
