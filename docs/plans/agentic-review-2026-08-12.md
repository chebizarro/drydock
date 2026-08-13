# Agentic Review: Adopting RepoPrompt's Oracle / Context Builder / MCP Patterns in Drydock

## Goal

Evolve Drydock's review stack from single-shot structured completions into a model-driven agentic loop — an agentic context builder, an iterative reviewer with tools, and stateful review sessions — exposed through a real MCP server layer and shared by all three orchestration families (pipeline, inline IDE, security audit). Optimize for review accuracy and depth; no cost/latency hard constraints.

## Background

### Drydock today

- All LLM steps are single-shot: `internal/reviewengine/client.go:25-41` exposes only `ChatCompletion` (system + user, optional JSON mode) — no tool schemas, no iterative turns.
- Review is a staged pipeline: planner → routed reviewer → finding validation → optional walkthrough (`internal/reviewengine/engine.go:66-125`); ensemble mode fans out reviewers and merges by consensus (`internal/reviewengine/ensemble.go:43-124`).
- Context is assembled deterministically *before* the LLM call by priority-ordered Go providers (patch, modified files, symbols, tests, imports, history, docs, retrieval) under a default 64K budget (`internal/contextbuilder/builder.go:109-180`, `internal/contextbuilder/providers.go:21-195`).
- Three orchestration families exist: stored-patch pipeline (`internal/pipeline/runner.go:206-264`), inline IDE review (`internal/idegateway/handler.go:409-477`), and full security audit (`internal/auditengine/engine.go:240-359`).
- ContextVM is JSON-RPC-over-Nostr transport (`internal/contextvm/methods.go`), not model-facing MCP tool invocation.
- Prior art: meta-review self-improvement loop (`docs/meta-review.md`), conversational review threads (commit `331dbcd`, `docs/codebase-chat.md:63-94`), NIP-78 session state for IDE (`docs/ide-integration.md:39-96`).

### RepoPrompt patterns to import

- **Agentic context builder**: a discovery sub-agent with a read-only tool allowlist (file tree, code structure, search, read, git) iteratively builds a selection under a hard token budget, with a mandatory final token-verification gate before halting (`SystemPromptService.swift:411-519`, `MCPClientToolPolicy.swift:35-101`).
- **Capability-filtered tool policy**: the tool surface is enforced by capability filtering, not prompt text alone (`MCPClientToolPolicy.swift:181-226`).
- **Stateful oracle chats**: `chat_id` continuation with conversation history reconstructed per turn; code context re-packaged fresh from a frozen selection + git snapshot (`OracleViewModel+MCP.swift:1086-1186`).
- **Review-mode packaging**: frozen, fail-closed git-diff artifacts + selected files + codemaps, with a severity rubric (P0/P1/P2) review prompt (`PromptViewModel.swift:1989-2030`, `PromptContextPreAssemblyService.swift:300-479`).

### External facts

- Official Go MCP SDK `modelcontextprotocol/go-sdk` is mature (v1.7.0, Jul 2026): stdio + Streamable HTTP transports; sampling deprecated in protocol `2026-07-28` in favor of MRTR. https://github.com/modelcontextprotocol/go-sdk
- OpenAI-compatible tool loops in Go are typically hand-rolled around `openai-go/v3`; no dominant agent runtime. https://github.com/openai/openai-go

## Approach

One shared component, `internal/agenticreview.Service`, consumed by all three orchestration families. It owns: freeze an immutable workspace snapshot → run model-driven context discovery through a capability-filtered tool registry → verify the finalized selection against a hard token budget → execute the review via an iterative tool-calling loop → persist the session. Existing planner/ensemble/verification/meta-review stages stay in place around it. The same tool registry is exposed over real MCP (official Go SDK) so external agents and the internal loop share one implementation and one security policy.

### New packages

| Package | Responsibility |
|---|---|
| `internal/agenttools` | Canonical tool definitions, JSON schemas, capability policy, snapshot-bound scopes, handlers |
| `internal/workspacesnapshot` | Immutable snapshots: pinned git refs (copy+hash only for mutable IDE workspaces), leases/expiry/GC |
| `internal/reviewsession` | Session domain: history reconstruction, optimistic versioned continuation, compaction |
| `internal/agenticreview` | The shared service: snapshot → discovery → finalize → review loop → persist |
| `internal/mcpserver` + `cmd/drydock-mcp` | MCP adapter (stdio + Streamable HTTP) over `agenttools.Registry`; no duplicate handlers |

### Tool registry & capability roles (mirrors RepoPrompt's MCPClientToolPolicy)

Tools: `repo.file_tree`, `code.structure` (tree-sitter), `code.search` (ripgrep→git-grep), `code.read`, `code.references` (LSP), `tests.search`, `git.read` (diff/show/log/blame), `context.layer` (existing providers by name), `security.trace`, `selection.add/remove/status/finalize`, `review.submit`. Roles gate capabilities: `context_discovery` (reads + selection mutation, no submit), `code_reviewer` (reads + `review.submit`, no selection mutation), `security_auditor` (reviewer + taint/snapshot-wide scope), `external_readonly` / `external_reviewer` for MCP clients. Capabilities are enforced at both `tools/list` and dispatch; all paths resolve inside the frozen snapshot, fail-closed (no absolute paths, `..`, symlinks, or live-workspace fallback). Existing provider internals are refactored into shared analysis facades so deterministic providers and tool handlers use one implementation.

Ship the core set first (`file_tree`, `structure`, `search`, `read`, `git.read`, `selection.*`, `review.submit`); `code.references`, `context.layer`, and `security.trace` land with the audit migration (item 14) rather than up front. `external_reviewer` submissions must bind to a pre-authorized session + snapshot created server-side — an external client can never create its own scope; until that binding is specified, external MCP access is `external_readonly` only.

### Discovery loop (mirrors RepoPrompt's context_builder)

Seed with the authoritative filtered patch + changed files + budget; the model explores via tools and mutates a run-local selection; `selection.finalize` server-renders the exact package, counts real serialized tokens (not estimates), and refuses to freeze if over budget — the mandatory final verification gate. Output is a standard `contextbuilder.ContextBundle`, so downstream consumers are unchanged. Deterministic `Builder.Build` remains a rollout-flag fallback that passes the same gate. Defaults: 24 turns, 96 tool calls, 64K package budget.

Two specifications the gate depends on: (a) **tokenizer authority** — `tiktoken.go:25-42` silently degrades to `ApproxTokenCounter` on encoding-load failure; in agentic mode that degradation is a hard startup error, and the budget gate applies a configurable headroom (default 10%) to absorb tokenizer skew for non-OpenAI models. (b) **Failure semantics** — if discovery exhausts turns/tool calls/tokens without a successful `selection.finalize`, the run falls back to deterministic `Builder.Build` output passed through the same gate (distinct from the rollout flag: this is an error path, recorded in metrics and meta-review trace). If that also fails the gate, the review fails.

### Iterative reviewer (mirrors Oracle review + agent loop)

Add a conversational `Complete()` interface beside `ChatCompletion` in `reviewengine/client.go` (messages, tool schemas, tool calls, usage); retry/circuit-breaker wrappers extend to it. `Engine` gains a `ReviewerExecutor` seam (`RunWithExecutor`, `RunEnsembleWithExecutors`) — planner, checklist, finding filtering, walkthrough stay engine-owned. The reviewer loop gets read-only tools plus `review.submit`, the only successful stop condition: schema-validated findings with P0/P1/P2 priority, evidence tool-call IDs per finding, and coverage statement. Ensemble members share one finalized package but have isolated transcripts/evidence; a member that exhausts its loop without `review.submit` is dropped from consensus (run fails only if all members fail). Consensus, security lens, SAST merge, and meta-review compose unchanged downstream (meta-review additionally receives agent trace metadata: turns, tools, evidence, stop reason).

Severity contract: consensus sorts on lowercase `Severity` via lookup maps (`ensemble.go:313-314,369-370`) — a missing key silently ranks P0 last. The canonical field and total P0/P1/P2 ↔ severity mapping must be fixed in item 1's output (with an audit of every downstream severity consumer: consensus, SAST merge, publication envelopes, meta-review), not decided during item 8.

### Stateful sessions (mirrors Oracle chat_id)

Five additive SQLite tables: `review_snapshots` (storage path, manifest/diff hashes, ref-count, expiry), `review_sessions` (opaque 128-bit `chat_id`, owner, mode, snapshot FK, optimistic `version`), `review_session_artifacts` (ordered selection entries with content hashes), `review_session_turns` (turn/request-ID idempotency), `review_session_messages` (normalized model/tool messages). Continuation: optimistic version check + request-ID idempotency → reload frozen selection, verify manifest hashes → re-render code context fresh from the snapshot → reconstruct conversation history → run the reviewer loop. History compacts deterministically; code context is never silently dropped — over-budget turns fail. Snapshot lease TTL is bound to session lifetime: an active session holds a lease, so a resumable session can never reference a GC'd snapshot; expiry marks the session `expired` before the snapshot is collected.

### Entry-point consumption

- **pipeline.Runner** (`runner.go:490-620`): replace `Builder.Build` + `Engine.Run` with `agenticreview.Prepare` / `ReviewPrepared`; repo prep, payment, envelopes, security stages, publication, meta-review stay pipeline-owned.
- **idegateway** (`handler.go:409-477`): freeze the live workspace before any tool call; use bundle `ChangedFiles` (not client-supplied); add optional `chat_id`/`expected_version`/`message` protocol fields for follow-ups; raise the 60s timeout to a configurable ~10min. The IDE path is the highest-frequency snapshot producer, so the snapshot storage strategy (full copy vs. copy-on-write/pinned-ref) is decided *before* item 4 starts, not during it — for the stored-patch pipeline a pinned commit SHA + patch ref + path allowlist suffices as a snapshot; full file copying + hashing is only justified where the IDE's mutable live workspace requires it. The VS Code extension releases independently, so the protocol additions ship early and backward-compatible (item 13a) to open a compat window.
- **auditengine** (`engine.go:240-359`): security-audit mode with `security_auditor` role and snapshot-root (not diff) finding validation; deterministic scans, localization, verification, progress unchanged.

## Work Items

1. **Validate hidden contracts** — characterize `reviewengine/ensemble.go`, `auditengine/engine.go` + `localizer.go`, DB migration numbering, IDE protocol types; fix the canonical severity/priority mapping and audit all downstream severity consumers; decide snapshot storage strategy (pinned-ref vs. copy) for each entry point.
1a. **IDE protocol compat window** — ship backward-compatible `chat_id`/`expected_version`/`message` fields in the IDE protocol + VS Code extension ahead of the backend migration.
2. **Conversational LLM transport** — `Complete()` with messages/tools/usage in `internal/reviewengine/client.go`; OpenAI-compatible encoding; retry/breaker wrappers. `ChatCompletion` preserved.
3. **Extract analysis facades** — export patch analysis, search, symbols, tests, history, docs, LSP from `internal/contextbuilder`; providers call the facades. (The non-nil-counter constructor change is its own trivial commit, plus the agentic-mode hard error on tokenizer fallback from `tiktoken.go:25-42`.)
4. **Workspace snapshots** — new `internal/workspacesnapshot`: pinned-ref snapshots for git-backed paths, copy+manifest-hash only for the mutable IDE workspace; safe path resolution; leases/GC with lease TTL ≥ session lifetime.
5. **Tool registry** — new `internal/agenttools`: tools + role matrix above, dual capability enforcement, snapshot-bound scopes, replay caching.
6. **Selection + exact renderer** — run-local selection state, coalescing, immutable finalization, exact-token budget gate; renders `ContextBundle`.
7. **Budgeted loop runner + discovery agent** — new `internal/agenticreview/loop.go`, `discovery.go`: turn/tool/token limits, sequential tool execution, `selection.finalize` as sole success stop, deterministic-builder error fallback on loop exhaustion.
8. **`ReviewerExecutor` seam in reviewengine** — `RunWithExecutor`, ensemble executor factory, P0/P1/P2 on `Finding` (mapped to legacy severity), single post-consensus walkthrough.
9. **Agentic reviewer executor** — `review.submit` validation, evidence ledger, patch-vs-snapshot scope validation, conversion to existing outputs.
10. **Session schema + store** — five tables via existing migrations; `internal/reviewsession` with versioned reservation, idempotency, compaction.
11. **Assemble `agenticreview.Service`** — `Prepare`, `ReviewPrepared`, `StartSession`, `Continue`; no orchestrator can bypass finalization.
12. **Migrate pipeline.Runner** — two-phase service calls; old path behind rollout fallback flag only.
13. **Migrate inline IDE** — snapshot freeze, continuation protocol fields, configurable timeout, turn-scoped fix IDs; VS Code extension protocol updates.
14. **Migrate security audit** — security-mode service calls; snapshot-root scope tests for findings outside patch diffs.
15. **MCP server** — official SDK v1.7, stdio (`cmd/drydock-mcp`) + authenticated Streamable HTTP, per-connection role/scope binding, filtered `tools/list`. No sampling/MRTR for the internal loop — it calls the registry directly.
16. **Config, metrics, lifecycle** — model/limit/snapshot/timeout/MCP-auth config; metrics for turns, tools, budget utilization, finalization failures, session conflicts, stop reasons.
17. **End-to-end + security coverage** — all three entry points on identical snapshots; capability escalation, path traversal, duplicate continuations, over-budget selection, ensemble isolation.
18. **Docs + rollout** — update `docs/architecture.md` and runbooks; rollout sequence: shadow discovery → agentic context with single-shot reviewer → iterative reviewer → sessions → MCP → remove fallback.

## Open Questions

- `auditengine/engine.go:240-359` internals were mapped but not fully read — item 1 must confirm audit candidate/result types before item 14's adapter names are final (boundary itself is fixed).
- Whether one-shot pipeline reviews should eagerly create sessions (promotable later) or only on request — plan assumes on-request.
- The exact server-side binding for `external_reviewer` MCP submissions (which session/snapshot, what auth) — until specified, external access is read-only (see §Tool registry).

## References

- [`docs/architecture.md`](../architecture.md), [`docs/review-engine.md`](../review-engine.md), [`docs/context-builder.md`](../context-builder.md), [`docs/meta-review.md`](../meta-review.md) (drydock)
- MCP Go SDK: https://github.com/modelcontextprotocol/go-sdk · mcp-go: https://github.com/mark3labs/mcp-go
- MCP sampling spec: https://modelcontextprotocol.io/specification/draft/client/sampling
