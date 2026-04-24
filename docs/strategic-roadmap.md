# Drydock Strategic Review & Roadmap

> **Date**: 2026-04-23
> **Scope**: Full codebase audit, competitive analysis, and implementation roadmap to make Drydock a world-class Nostr-first AI code review service.

---

## Part 1: What Drydock Provides Today

### Core Pipeline

Drydock is a fully automated NIP-34 code review agent. It listens for patch and PR events on Nostr relays, reviews them with local LLMs, and publishes structured review comments back to the protocol. No code ever leaves the operator's infrastructure.

| Capability | Implementation | Status |
|-----------|---------------|--------|
| **Nostr event listener** | Subscribes to 12 NIP-34 event kinds via pool with NIP-42 AUTH | ✅ Production |
| **Event ingestion** | Signature verification, dedup, staleness/closed-root filtering | ✅ Production |
| **Repository management** | Git clone/fetch with LRU cache, per-repo locking, URL validation | ✅ Production |
| **Patch series ordering** | Reply-chain topology sort with timestamp fallback | ✅ Production |
| **Context builder** | 8-layer priority system, 64K token budget, deterministic assembly | ✅ Production |
| **Tree-sitter AST** | Symbol extraction for 9 languages (Go, Python, JS, TS, Rust, C, C++, Java, Ruby) | ✅ Production |
| **Workspace detection** | Monorepo boundary detection (npm, pnpm, Cargo, Go workspaces, Lerna) | ✅ Production |
| **LLM review pipeline** | Two-stage planner → reviewer with model routing (14B/32B/70B) | ✅ Production |
| **Structured findings** | JSON schema with severity, category, file, line, evidence, confidence | ✅ Production |
| **Review publishing** | Kind 1111 (NIP-22) comments with NIP-40 expiration, relay fanout | ✅ Production |
| **Signer chain** | NIP-46 bunker → NIP-5F socket → NIP-55L DBus → local nsec | ✅ Production |
| **Meta-review** | Self-improvement loop: quality evaluation, false positive detection | ✅ Production |
| **Few-shot management** | Positive/negative example extraction from meta-reviews | ✅ Production |
| **Prompt refinement** | Automated prompt versioning with eval-gated rollback | ✅ Production |
| **RAG context** | Qdrant vector search for NIP specs and project documentation | ✅ Production |
| **LSP bridge** | Type-aware symbol analysis via language server sidecar | ✅ Production |
| **Evaluation harness** | Held-out dataset with recall, FPR, calibration, precision metrics | ✅ Production |
| **Drift guard** | Convention drift detection and flagging from meta-review samples | ✅ Production |
| **Observability** | Prometheus-compatible /metrics with 17 counters/gauges/summaries | ✅ Production |
| **Crash recovery** | Pre-publish breadcrumbs, failed-review requeue, stuck-review reset | ✅ Production |
| **Health checks** | /healthz, /readyz, graceful shutdown with drain timeout | ✅ Production |

### Self-Improvement Loop (Unique)

No competitor has a comparable automated self-improvement loop:

```
Review published
    │
    ├── Meta-review (second LLM pass)
    │   ├── Missed findings → route feedback
    │   ├── False positives → negative few-shot
    │   └── Prompt gaps → queue refinement
    │
    ├── Prompt refinement (batched gaps → LLM rewrite)
    │   └── Eval gate → activate or rollback
    │
    └── Few-shot update (positive/negative examples)
```

### Nostr-Native Architecture

| Capability | Nostr NIP | Description |
|-----------|-----------|-------------|
| Code collaboration | NIP-34 | Patches, PRs, repository announcements, status events |
| Review comments | NIP-22 | Kind 1111 threaded comments |
| Relay auth | NIP-42 | Automatic AUTH challenge response |
| Remote signing | NIP-46 | Bunker-based key isolation |
| Socket signing | NIP-5F | Unix domain socket signer (Signet) |
| DBus signing | NIP-55L | Linux session bus signer |
| Event expiration | NIP-40 | TTL on review comments (90d default, 7d superseded) |
| Relay probing | NIP-11 | Capability detection at startup |

---

## Part 2: Competitive Landscape

### vs. CodeRabbit (Market Leader)

| Feature | CodeRabbit | Drydock | Gap |
|---------|-----------|---------|-----|
| Incremental review | Reviews only changed hunks | Reviews full diff | ⚠️ Minor |
| **Inline code suggestions** | Diff-ready code blocks, one-click apply | Findings describe fixes but don't provide replacement code | ❌ Critical |
| **Conversational threads** | Reply to comments, get follow-up analysis | One-shot reviews, ignores replies | ❌ Critical |
| **Review profiles** | Per-repo `.coderabbit.yaml` | Single global config | ❌ Critical |
| Path filtering | Configurable per repo | Hardcoded exclusion list | ⚠️ Moderate |
| **Auto-fix generation** | One-click apply of suggestions | Not available | ❌ High |
| **PR summary / walkthrough** | Auto-generated walkthrough | Summary is finding-focused | ⚠️ Moderate |
| Sequence diagrams | Visual flow diagrams | Not available | Low priority |
| Knowledge graph | Learns project patterns | Few-shot partial coverage | ⚠️ Partial |
| **Dashboard** | Web UI for history, metrics, trends | /metrics only, no UI | ❌ Moderate |
| Security scanning | SAST + LLM | LLM-only + checklist injection | ⚠️ Moderate |
| Data sovereignty | ❌ Code sent to cloud | ✅ All local | **Drydock wins** |
| Self-improvement | ❌ Static | ✅ Auto prompt refinement | **Drydock wins** |
| Cryptographic provenance | ❌ Platform-signed | ✅ Nostr-signed, verifiable | **Drydock wins** |

### vs. Greptile (Deep Codebase Understanding)

| Feature | Greptile | Drydock | Gap |
|---------|---------|---------|-----|
| **Full codebase indexing** | Entire repo semantic search | Project docs only | ❌ High |
| **Codebase chat** | Interactive Q&A | Not available | ❌ Differentiating |
| Cross-repo learning | Patterns transfer between repos | Global few-shots | ⚠️ Moderate |
| API endpoint | Programmatic access | Not available | ⚠️ Moderate |
| Change impact analysis | "What else might break?" | Symbols + callsites partial | ⚠️ Partial |

---

## Part 3: Drydock's Structural Advantages

Capabilities centralized competitors **cannot replicate**:

1. **Sovereignty & Privacy** — All inference local. No vendor lock-in. Operators choose models and hardware.
2. **Cryptographic Provenance** — Reviews are signed Nostr events. Anyone can verify authorship. Portable across clients.
3. **Censorship Resistance** — Multi-relay publishing. No single point of failure.
4. **Self-Improvement Loop** — Automated prompt refinement with eval-gated rollback. No competitor has this.
5. **Decentralized Monetization** — NWC/Cashu payment designs. Peer-to-peer, no middleman.
6. **Interoperability** — Any NIP-34 client triggers reviews. Reviews visible in any kind-1111-aware client.

---

## Part 4: Implementation Roadmap

### Phase 1: Core Competitive Parity (Weeks 1–3)

#### 1.1 Inline Code Suggestions
Extend Finding schema with `suggested_diff` / `suggested_code` fields. Update reviewer prompt with few-shot example. Format as fenced diff blocks in published comments. **Effort: 2d**

#### 1.2 Per-Repository Configuration
`.drydock.yaml` in repo root: severity floor, category filter, exclude paths, custom instructions, token budget override. New `internal/repoconfig` package. Wire into pipeline and publisher. **Effort: 3d**

#### 1.3 Change Walkthrough / PR Summary
Separate lightweight LLM call for change description. Walkthrough + file summaries prepended to review. Configurable via `.drydock.yaml`. **Effort: 1.5d**

#### 1.4 Conversational Review Threads
Subscribe to kind 1111 replies tagging Drydock's pubkey. New `internal/conversation` package. Look up original context, generate focused response, publish reply. Rate limit 3 turns/review. **Effort: 4d**

### Phase 2: Depth & Intelligence (Weeks 4–6)

#### 2.1 Full Codebase Indexing
New Qdrant `code_chunks` collection. Tree-sitter function/class chunking. Incremental re-indexing on HEAD change. New context builder provider for semantic code retrieval. **Effort: 5d**

#### 2.2 Security Scanner Integration
Curated regex/AST rule set for common vulnerabilities. Optional semgrep integration. Inject scan results as high-priority context layer. Deduplicate against LLM findings. **Effort: 4d**

#### 2.3 Cross-Repo Learning
Tag few-shots with language/project type. Qdrant similarity-based retrieval. Weight by recency, similarity, quality scores. **Effort: 3d**

#### 2.4 Review Analytics Dashboard
Embedded web UI (htmx + Alpine.js). API endpoints for stats, review history, quality trends, model performance. Server-rendered, no SPA. **Effort: 5d**

### Phase 3: Nostr-Native Differentiation (Weeks 7–9)

#### 3.1 Cashu Payment Gate
`internal/payment` package. Extract Cashu token from event tags, redeem against mint. Free tier rate limiter. Payment receipt in review footer. Atomic redemption + BeginReview. **Effort: 4d**

#### 3.2 Auto-Fix Patch Generation
Apply `suggested_diff` to checkout. Generate kind 1617 fix patch. Publish as reply. High-confidence guard (>0.9). **Effort: 4d**

#### 3.3 NIP-34 Status Integration
Publish kind 1630/1631 status based on review outcome. Authorization via `.drydock.yaml`. Advisory only. **Effort: 2d**

#### 3.4 Change Impact Analysis
LSP `findReferences` for modified symbols. Codebase index queries. "This function has 15 call sites across 8 files." **Effort: 3d**

### Phase 4: Platform Play (Week 10+)

#### 4.1 Codebase Chat
Nostr-native Q&A. Users query via kind 1111, Drydock responds from code index. **Effort: 5d**

#### 4.2 Multi-Model Ensemble
Run 2-3 models in parallel. Consensus-based confidence. Single-model findings flagged. **Effort: 3d**

#### 4.3 Review Marketplace
Operators publish quality scores. Submitters choose reviewers. Payment + quality market. **Effort: 5d**

#### 4.4 IDE Integration
VS Code / Neovim extension subscribing to Drydock review events. Inline annotations. **Effort: 5d**

---

## Part 5: Architecture Decisions

### Near-Term (Keep SQLite)
Phases 1–3 work fine with SQLite. LLM inference is the bottleneck, not the DB. New tables: `conversations`, `repo_configs`, `payment_log`, `code_chunks_meta`.

### Medium-Term (Consider PostgreSQL)
If needed for: multiple concurrent operators, >100 reviews/hour, complex dashboard queries. The `Store` abstraction isolates all SQL — migration is mechanical.

### Long-Term (Distributed)
For marketplace: replace in-process queue with NATS/Redis Streams. Separate listener, pipeline, publisher into independent services. Qdrant distributed mode.

---

## Summary

| Phase | Features | Effort | Outcome |
|-------|----------|--------|---------|
| Phase 1 | Inline suggestions, per-repo config, walkthrough, conversations | ~10.5d | Interactive review partner |
| Phase 2 | Codebase indexing, security scanner, cross-repo learning, dashboard | ~17d | Deep analysis + operator visibility |
| Phase 3 | Cashu payments, auto-fix patches, status integration, impact analysis | ~13d | Nostr-native moats |
| Phase 4 | Codebase chat, ensemble review, marketplace, IDE integration | ~18d | Platform |

**Total Phases 1–3: ~40 engineering days.**
