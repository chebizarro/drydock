# Per-Repository Configuration (`.drydock.yaml`)

Repositories can tune Drydock's behavior by committing a `.drydock.yaml` to
the root of the default branch. The file is always read from the **canonical**
repository's base branch — never from the incoming patch or a PR fork — so a
change under review cannot alter its own review policy.

A missing file means all defaults apply. A file that fails to parse is logged
and ignored (defaults apply), except when it contains a `payments` section, in
which case the review fails closed rather than risk skipping payment policy.

```yaml
version: 1

review:
  severity_floor: info          # minimum severity to publish (info|low|medium|high|critical)
  detail_severity_floor: high   # severity threshold for detailed per-finding comments
  categories: []                # restrict findings to these categories (empty = all)
  walkthrough: true             # generate the per-file walkthrough
  statuses: [open]              # NIP-34 root statuses reviewed automatically

context:
  token_budget: 0               # override the context token budget (0 = default)
  exclude_paths: []             # glob paths excluded from context layers
  include_docs: true            # include the project-docs context layer

status:
  enabled: false                # publish kind 1630 status events for blocking findings
  open_severity_floor: critical # findings at or above this trigger a 1630
  min_confidence: 0.90

autofix:
  enabled: false                # synthesize kind 1617 fix patches from findings
  min_confidence: 0.97
  max_findings: 3

ensemble:
  enabled: false                # run multiple reviewer models and merge by consensus
  models: [coder32b, llm70b]
  consensus_boost: 0.10
  require_consensus: false

payments:                       # payment gating (see docs/payments.md)
  enabled: false
  price_sats: 0                  # set to a positive value when enabled
  accept_zaps: true                # NIP-57 receipts may cover price_sats
  free_reviews_per_day: 0        # per-author daily quota
  free_pubkeys: []               # npub or 64-character hex; normalized to hex
  free_for_maintainers: true     # repository owner/maintainers review free
  subscription_price_sats: 0
  subscription_days: 0

instructions: |                 # appended to the reviewer system prompt (max 4 KiB)
  Focus on concurrency and error handling.
```

## Review trigger statuses

`review.statuses` controls which NIP-34 root statuses are reviewed
**automatically** when a patch (kind 1617) or PR (kind 1618/1619) event
arrives:

| Value | Meaning |
|-------|---------|
| `open` | Roots whose latest status is kind 1630 — or that have **no status event at all** (the NIP-34 default for a fresh patch/PR). Included by default. |
| `draft` | Roots whose latest status is kind 1633. Off by default; add it to opt in to draft reviews. |

Applied/merged (kind 1631) and closed (kind 1632) roots are **never** reviewed
automatically and cannot be enabled here — configuring `applied`, `merged`, or
`closed` is a parse error. A status-gated skip is recorded permanently and is
not retried by the failed-review sweep; a later status change back to open
arrives as a new event and reviews normally.

## Validation rules

- `version` must be `1` (or omitted).
- `severity_floor` / `detail_severity_floor` must be valid severities.
- `review.statuses` values must be `open` or `draft`.
- `payments.free_pubkeys` entries must be valid npub or 64-character hex public keys.
- `payments.free_for_maintainers` defaults to `true`; set it to `false` to require payment from the repository announcement owner and maintainers.
- `payments.accept_zaps` defaults to `true` when payments are enabled. A kind 9735 receipt must target the patch and cover `payments.price_sats * 1000` millisatoshis.
- `instructions` above 4096 bytes is a parse error (defaults apply).
- Unknown fields are rejected (strict parsing), so typos fail loudly in the
  logs rather than being silently ignored.
