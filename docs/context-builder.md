# Context Builder

The context builder assembles a deterministic context bundle for LLM review. It enforces a strict token budget with priority-based layer selection — higher-priority layers are always included first, and lower-priority layers are dropped when the budget is exhausted.

## Why Deterministic Context Matters

LLM output is sensitive to input context. By enforcing a fixed priority order and hard budget, the context builder ensures:

- **Reproducibility**: The same patch against the same repo state always produces the same context bundle
- **Auditability**: The review footer always lists which layers were used and which were dropped
- **Quality**: The most important context (the diff itself, modified files) is never sacrificed for lower-priority context

## Layer Priority Table

Layers are assembled in strict priority order. Once the token budget is reached, the current layer and all remaining layers are dropped.

| Priority | Layer | Provider | Source | Caps |
|----------|-------|----------|--------|------|
| 1 | `patch` | `patchDiffProvider` | Event content (unified diff) | 40 KB hard cap |
| 2 | `modified-files` | `fileContextProvider` | Full file contents from repo checkout | 20 KB total, 4 KB per file |
| 3 | `symbols` | `symbolsCallsitesProvider` | `git grep` for changed function/type names | 12 symbols max |
| 4 | `tests` | `testsProvider` | `git grep` in test files for changed symbols | — |
| 5 | `imports-exports` | `importsExportsProvider` | Import/export lines extracted from the diff | 100 lines max |
| 6 | `commit-history` | `commitHistoryProvider` | `git log --oneline -n 10` for changed files | 10 commits |
| 7 | `project-docs` | `projectDocsProvider` | CONTRIBUTING.md, README.md, style guides | 15 KB total, 4 KB per file |

## Token Budget

| Parameter | Default | Description |
|-----------|---------|-------------|
| Token budget | 64,000 | Maximum tokens in the assembled bundle |
| Token counter | `ApproxTokenCounter` | Approximation: `rune_count / 4` |

The approximate counter is intentionally conservative — it overestimates slightly to prevent exceeding real token limits. You can inject a custom `TokenCounter` implementation (e.g., tiktoken) via `Builder.Counter`.

### Drop Policy

When a layer would exceed the remaining budget:

1. That layer is dropped
2. **All** lower-priority layers are also dropped (hard stop)
3. Dropped layer names are recorded in `LayersDropped`
4. The `context-layers-dropped` field appears in every published review footer

## Symbol Extraction

The `symbolsCallsitesProvider` extracts function and type names from the diff using a regex:

```
^[+-]\s*(?:func|type|class|def)\s+([A-Za-z_][A-Za-z0-9_]*)
```

This matches Go `func`/`type`, Python `def`/`class`, and similar declarations. Up to 12 unique symbols are extracted, sorted, and deduplicated. For each symbol, `git grep` finds usages across the repository.

## Excluded Paths

The following paths are excluded from the `modified-files` and `commit-history` layers:

| Pattern | Reason |
|---------|--------|
| `*.proto` | Protocol buffer definitions (generated code) |
| `package-lock.json`, `cargo.lock`, `poetry.lock`, `pnpm-lock.yaml`, `bun.lock`, `yarn.lock` | Lock files (no review value, high token cost) |
| `*__generated__*`, `*generated/graphql*` | Generated code |
| `*migration*` + `*snapshot*` | Migration snapshots |

Binary files are detected by the presence of null bytes and silently skipped.

## Adding a New Provider

Implement the `Provider` interface:

```go
type Provider interface {
    LayerName() string           // Unique layer identifier
    Priority() int               // Lower number = higher priority
    Build(ctx context.Context, in BuildInput) (string, error)
}
```

Register your provider in `DefaultProviders()` in `internal/contextbuilder/providers.go`:

```go
func DefaultProviders() []Provider {
    return []Provider{
        patchDiffProvider{},
        fileContextProvider{},
        symbolsCallsitesProvider{},
        testsProvider{},
        importsExportsProvider{},
        commitHistoryProvider{},
        projectDocsProvider{},
        yourNewProvider{},  // added here
    }
}
```

No other changes are required — the builder discovers providers from this list.
