# Agentic review foundation contracts

This note records the decisions and characterization established by
DRYDOCK-f87s.1. It is intentionally limited to the foundations needed by later
agentic-review work.

## Finding priority and legacy severity

`reviewengine.Finding.Priority` is the canonical urgency field:

| Priority | Canonical legacy severity | Accepted legacy inputs |
|---|---|---|
| P0 | critical | critical |
| P1 | high | high |
| P2 | medium | medium, low, info |

The reverse mapping is total for all five legacy values. `low` and `info`
remain valid and retain their legacy spelling, but both belong to P2. If both
fields are supplied they must agree. Legacy-only findings are normalized at
review parsing and at downstream boundaries.

Downstream audit:

- Ensemble consensus and deduplication sort through
  `FindingPriorityRank`, rather than local severity maps. P0 therefore cannot
  silently receive a missing-map rank.
- The SAST merge retains its five-level severity upgrade comparison, then
  populates canonical priorities on the merged result.
- Review publication validates and normalizes findings before detail-floor
  filtering or envelope/content construction. Existing Nostr text continues
  to expose legacy severity for compatibility.
- Meta-review normalizes the local review before gating, JSON packaging, and
  few-shot persistence. Its payload therefore contains canonical priority and
  compatible legacy severity.
- Security-audit publication remains a five-level legacy envelope. Audit
  findings pass through review-engine deduplication first; the later audit
  migration must derive its published legacy severity from canonical priority
  when agentic findings enter that path.

## Snapshot storage strategy

- Stored-patch pipeline: pin the resolved base/tip commit SHA and immutable
  patch reference, with a normalized path allowlist. Do not copy the checkout.
- Whole-repository security audit: pin the resolved audit commit SHA and
  subtree/path allowlist. Do not copy the canonical checkout.
- Mutable IDE workspace: copy the allowed workspace content and record
  per-file hashes plus a manifest hash before any agent tool runs. A git ref
  alone cannot freeze uncommitted editor state.

Pinned refs must remain leased for the lifetime of any resumable session.
Copy-and-hash is reserved for mutable IDE state.

## Characterized boundaries

- Database migrations are an ordered in-code slice, currently contiguous from
  version 1 through version 13. The next migration version is 14.
- Audit localization produces ranked internal `candidateUnit` values.
  Per-unit workers return internal `unitResult` values, and `Run` aggregates
  them into exported `auditengine.Result`. Model localization may reprioritize
  only files in the audit allowlist and falls back to heuristic ordering on
  configuration or model failure.
- IDE review requests and responses are JSON-RPC params/results backed by Go
  structs in `internal/idegateway/types.go` and matching TypeScript interfaces
  in `extensions/vscode-drydock`. Additions must remain optional and
  unknown fields remain tolerated for rolling extension/backend upgrades.
