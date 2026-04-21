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
| 2 | `modified-files` | `fileContextProvider` | Full file contents from repo checkout (workspace-scoped) | 20 KB total, 4 KB per file |
| 3 | `symbols` | `symbolsCallsitesProvider` | Tree-sitter AST → changed declarations → ripgrep/git grep callsites | 12 symbols max |
| 4 | `tests` | `testsProvider` | ripgrep/git grep in test files for changed symbols (workspace-scoped) | — |
| 5 | `imports-exports` | `importsExportsProvider` | Import/export lines extracted from the diff | 100 lines max |
| 6 | `commit-history` | `commitHistoryProvider` | `git log --oneline -n 10` for changed files | 10 commits |
| 7 | `project-docs` | `projectDocsProvider` | Workspace-local + repo-level CONTRIBUTING.md, README.md, style guides | 15 KB total, 4 KB per file |
| 8 | `qdrant-docs` | `QdrantProvider` | Vector search: NIP specs (for Nostr patches) + project documentation | Requires Qdrant + embedding server |

## Token Budget

| Parameter | Default | Description |
|-----------|---------|-------------|
| Token budget | 64,000 | Maximum tokens in the assembled bundle |
| Token counter | `TiktokenCounter` | Uses tiktoken-go (cl100k_base encoding); falls back to `ApproxTokenCounter` (rune_count / 4) if loading fails |

### Drop Policy

When a layer would exceed the remaining budget:

1. That layer is dropped
2. **All** lower-priority layers are also dropped (hard stop)
3. Dropped layer names are recorded in `LayersDropped`
4. The `context-layers-dropped` field appears in every published review footer

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
