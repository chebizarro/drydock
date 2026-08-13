# Context Builder

Drydock has two context paths that produce the same `ContextBundle`: agentic discovery selects frozen-snapshot artifacts and passes an exact serialized-token gate; the deterministic builder remains the rollout and loop-exhaustion fallback. Neither path may bypass finalization. Operational limits and failure procedures are in [Agentic Review Operations](agentic-review.md).

## Why Deterministic Context Matters

LLM output is sensitive to input context. By enforcing a fixed priority order and hard budget, the context builder ensures:

- **Reproducibility**: The same patch against the same repo state always produces the same context bundle
- **Auditability**: The review footer always lists which layers were used and which were dropped
- **Quality**: The most important context (the diff itself, modified files) is never sacrificed for lower-priority context

## Layer Priority Table

Layers are assembled in stable priority/name order. When a layer exceeds the remaining budget, priority-1/2 content may be truncated to a useful prefix; otherwise that layer is dropped and later layers are still considered.

| Priority | Layer | Provider | Source | Caps |
|----------|-------|----------|--------|------|
| 1 | `patch` | `patchDiffProvider` | Authoritative filtered unified diff | 40 KB hard cap |
| 2 | `modified-files` | `fileContextProvider` | Changed-file contents | 20 KB total, 4 KB per file |
| 2 | `change-impact` | `changeImpactProvider` | Changed symbols and downstream callsites | bounded search results |
| 2 | `taint` | `taintProvider` | Taint-oriented source/sink analysis, with optional LSP | bounded analysis output |
| 3 | `symbols` | `symbolsCallsitesProvider` | Tree-sitter declarations plus LSP or rg/git-grep callsites | 12 symbols max |
| 4 | `tests` | `testsProvider` | Test files referencing changed symbols | — |
| 5 | `imports-exports` | `importsExportsProvider` | Import/export lines extracted from the diff | 100 lines max |
| 6 | `commit-history` | `commitHistoryProvider` | `git log --oneline -n 10` for changed files | 10 commits |
| 7 | `project-docs` | `projectDocsProvider` | Workspace-local and repository docs | 15 KB total, 4 KB per file |

These are the **9 core providers** returned by `DefaultProviders`. Configured Qdrant or Chartroom retrieval is appended at priority 8; audit configuration may append security-surface providers.

## Token Budget

| Parameter | Default | Description |
|-----------|---------|-------------|
| Token budget | 64,000 | Maximum tokens in the assembled bundle |
| Token counter | `TiktokenCounter` | Uses tiktoken-go (`cl100k_base`). Deterministic-only mode may use `ApproxTokenCounter`; agentic startup rejects approximate fallback as a hard error |

### Drop Policy

When a layer would exceed the remaining budget:

1. Priority-1/2 content is truncated when a useful prefix fits
2. Otherwise that layer is dropped and later layers are still considered
3. Dropped or truncated status, messages, and token counts are recorded in `LayerStatuses`
4. Dropped layer names are recorded in `LayersDropped` and publication metadata

## Agentic Discovery and Exact Finalization

```
authoritative patch + changed files
              │
              ▼
     mandatory selection seed
              │
      model uses read/search/git tools
      and selection.add/remove/status
              │
              ▼
       selection.finalize
        │             │
   over budget      success
        │             ▼
   prune + retry   immutable ContextBundle
```

Discovery runs with a snapshot-bound `context_discovery` role. The patch and changed-file artifacts are mandatory; optional full files, line ranges, and codemaps can be added or removed. `selection.finalize` re-verifies snapshot hashes, renders the exact package, counts the serialized content with the authoritative tokenizer, applies configurable headroom (10% by default), and freezes the selection only on success.

If discovery exhausts its turn, tool-call, token, or model-context limit without successful finalization, `contextbuilder.Builder.Build` runs against a materialization of the same snapshot. Its output must pass the same exact gate. If fallback building or gating fails, review fails rather than reviewing partial or approximate context.

## Symbol Extraction (Tree-sitter)

The `symbolsCallsitesProvider` uses tree-sitter for accurate, AST-based symbol extraction across 9 languages:

| Language | Declaration Node Types |
|----------|----------------------|
| Go | `function_declaration`, `method_declaration`, `type_spec` |
| Python | `function_definition`, `class_definition` |
| JavaScript | `function_declaration`, `class_declaration`, `method_definition` |
| TypeScript | + `interface_declaration`, `type_alias_declaration`, `enum_declaration` |
| Rust | `function_item`, `struct_item`, `enum_item`, `trait_item`, `impl_item` (container) |
| C | `function_definition`, `struct_specifier`, `enum_specifier` |
| C++ | + `class_specifier` |
| Java | `class_declaration`, `interface_declaration`, `method_declaration`, `enum_declaration` |
| Ruby | `method`, `class`, `module` |

**Process**:
1. Parse the diff to identify changed files and line ranges
2. For each file in a supported language, parse the full file with tree-sitter
3. Walk the AST to find declaration nodes overlapping changed lines
4. Extract up to 12 unique symbol names
5. Search for callsites using ripgrep (preferred) or git grep (fallback)

**Fallback**: For unsupported languages or when CGO is disabled (tree-sitter requires CGO), a regex fallback extracts `func`, `type`, `class`, and `def` declarations from diff lines.

### Callsite Search

Symbol callsite search uses a priority chain:
1. **ripgrep** (`rg`) — parallel, respects .gitignore, word-boundary matching
2. **git grep -P** — Perl regex with `\b` word boundaries
3. **git grep -F** — fixed-string fallback (for systems without PCRE support)

The searcher auto-detects `rg` in `$PATH` at startup and caches the result.

## Workspace Boundary Detection

For monorepos, the context builder auto-detects workspace boundaries to prevent cross-module context pollution:

| Config File | Workspace Type | Field |
|-------------|---------------|-------|
| `package.json` | npm/yarn | `workspaces` array or `workspaces.packages` |
| `pnpm-workspace.yaml` | pnpm | `packages` list |
| `Cargo.toml` | Cargo | `[workspace]` `members` |
| `go.work` | Go | `use` directives |
| `lerna.json` | Lerna | `packages` array |

**Behavior**:
1. On each build, scan the repo root for workspace config files
2. Resolve glob patterns to actual directories (e.g., `packages/*` → `packages/auth`, `packages/core`)
3. Determine which workspace(s) contain the changed files
4. Scope search providers (symbols, tests) and project docs to relevant workspace directories

When no workspace config is found, or changed files are outside all workspaces, the entire repo is searched (backward-compatible default).

## Qdrant Retrieval (Layer 8)

When Qdrant and an embedding server are configured, the `QdrantProvider` adds a retrieval-augmented context layer:

1. Embed the patch diff
2. If the patch looks Nostr-related (detected via keyword matching for NIP, relay, event kind, etc.), query the `nip_specs` collection
3. Always query the `project_docs` collection
4. Concatenate results as the `qdrant-docs` layer

**Nostr detection keywords**: `nip`, `nostr`, `relay`, `event`, `kind`, `pubkey`, `npub`, `nsec`, `naddr`, `nevent`, `nprofile`, `tag`.

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

Register your provider in `DefaultProviders()` in `internal/contextbuilder/providers.go`. The builder discovers providers from this list and uses the `BuilderOptions` pattern for optional service dependencies:

```go
// Example: adding a provider that needs an external client
func WithMyService(client *myservice.Client) func(*BuilderOptions) {
    return func(opts *BuilderOptions) {
        opts.myClient = client
    }
}
```

No other changes are required — the builder discovers providers from `DefaultProviders` and wires options automatically.
