# Design: Security Code Review

**Status:** Proposed
**Date:** 2026-08-06
**Scope:** Add a first-class security review capability to Drydock, exposed as two pathways — a deep whole-repository audit and a security lens on the existing per-patch/PR review — built on local open-weight models.

---

## 1. Motivation and goals

Drydock today produces general code review (correctness, architecture, style, test-coverage, and a thin `security` category) driven by a two-stage planner→reviewer pipeline. Security is present but shallow: a regex SAST scanner (`internal/securityscan`) injects deterministic pattern matches as a context layer, and the reviewer is nudged with a data-flow instruction when a changed file name matches `auth`/`crypto`/`security`. There is no dedicated audit flow, no taint/data-flow evidence, no false-positive suppression pass, and no way to run a security sweep across an entire repository rather than a single diff.

This design adds security review as a distinct concern with two entry points:

- **Pathway A — Deep Audit.** An on-demand, whole-repository (or subtree) security sweep. Latency-tolerant, thorough, runs the heavier models. Produces a ranked, deduplicated, adversarially-verified findings report published back over Nostr (public summary + gift-wrapped detail) and optionally as SARIF.
- **Pathway B — PR/Patch Security Lens.** A security-focused stage layered into the *existing* review pipeline (kinds 1617/1618/1619), gated for CI-style pre-merge use. Fast, diff-scoped, reuses the audit machinery but bounded to the change and its blast radius.

**Design principles** (carried from the evaluation work that motivated this feature):

1. *The model is the judgment layer, not the search layer.* Deterministic tooling (SAST, taint, graphs) finds and narrows; the LLM confirms, explains, rates severity, and catches logic-class flaws that rules miss. Hybrid pipelines outperform raw model calls by a wide margin.
2. *Fail closed, anchor to reality.* Reuse Drydock's existing guards: empty changed-file set aborts before any LLM call; findings are filtered to a deterministic file set. Extend the same discipline to audit (findings must anchor to real files/lines the scan actually read).
3. *Suppress false positives explicitly.* An adversarial verification stage is the single highest-leverage quality lever for security review, where false-positive fatigue kills adoption.
4. *Everything stays local and Nostr-native.* No code leaves the operator's infrastructure; all inter-component signalling is event-driven per `AGENTS.md`.

**Non-goals:** offensive tooling / exploit generation; replacing dedicated SCA (dependency CVE) or secret-scanning products (we integrate them, we don't reimplement them); cloud-model dependence.

---

## 2. What exists today (and the gap)

| Capability | Today | Gap for security review |
|---|---|---|
| Deterministic SAST | `securityscan.Scanner` — 18 regex rules, diff-aware, injected via `securityscan.Provider` (priority 1) | No taint/data-flow, no severity calibration, regex-only recall |
| LLM review | `reviewengine.Engine` planner→reviewer, routes `coder14b`/`coder32b`/`llm70b` | No security-specialized route, no verify pass, single lens |
| Ensemble | `reviewengine` ensemble + consensus merge (dedup by file/±2 lines/category) | Consensus is used for *agreement*, not *refutation* |
| Context | `contextbuilder` 8-layer, `Provider` interface, token-budgeted | No security-surface map, no code-map/call-graph cache, no repo-wide mode |
| Scope | Per-patch/PR only (diff-driven) | No whole-repo audit flow |
| Config | `.drydock.yaml` (`review`, `ensemble`, `status`, `autofix`, …) | No `security` section |
| Protocol | kinds 1111 / 1617-1619 / 1630-1633 / 25910 / 7000 / 1059 | No audit request method, no findings-report kind |

The good news: the two pathways are mostly *composition* of existing seams — a new `Provider` or two, a new `ModelRoute`, a new pipeline stage, one ContextVM method, and a `.drydock.yaml` section — plus one genuinely new subsystem (the repo-wide audit orchestrator) that still reuses `contextbuilder`, `reviewengine`, and `publisher`.

---

## 3. Architecture overview

```
                         ┌───────────────────────────── shared substrate ─────────────────────────────┐
                         │  repo.Manager (clone/checkout)   contextbuilder.Provider registry           │
                         │  codemap cache (NEW)   securityscan SAST   taint provider (NEW)              │
                         │  security-surface provider (NEW)   Antares localizer (NEW, optional)         │
                         └───────────────────────────────────────────────────────────────────────────┘
                                    ▲                                              ▲
     Pathway A (deep audit)         │                                              │   Pathway B (PR lens)
                                    │                                              │
 kind 25910 security/audit ──▶ auditengine (NEW) ──┐              ingest ▶ pipeline.Runner (existing)
   or repo announcement/          │ chunk repo     │                          │ + security stage (NEW)
   snapshot trigger               │ localize (Antares)                        │ diff + blast radius
                                  │ per-unit review │                         │ SAST + taint packet
                                  ▼                 │                         ▼
                         reviewengine (security route: sec70b)  ◀────────────  reviewengine.Run
                                  │                                              │
                                  ▼  adversarial verify (consensus-refute, NEW) ▼
                         findings triage/classify (Foundation-Sec-8B route, NEW)
                                  │                                              │
                                  ▼                                              ▼
                    publisher: kind 30619 audit report (NEW, addressable)   publisher: kind 1111
                    + gift-wrapped 1059 detail + SARIF artifact             + kind 1630 status gate
```

Both pathways converge on the same three new logical stages — **evidence assembly**, **security review**, **adversarial verification + classification** — and diverge only in scope (whole repo vs diff) and trigger (ContextVM method / repo event vs patch event).

---

## 4. Pathway B — PR/Patch security lens (enhance the existing pipeline)

This is the smaller change and should ship first, because it reuses the running pipeline end-to-end.

### 4.1 Trigger and flow

No new trigger — it runs inside `pipeline.Runner.work` for kinds 1617/1618/1619, after `contextbuilder.Build` and either replacing or augmenting the `reviewengine.Run` call when security review is enabled for the repo. The existing status gate, changed-file anchoring, and payment gating are unchanged.

### 4.2 New pipeline stage: `security review`

Add a stage that runs when `.drydock.yaml` `security.enabled` is true (or a changed file is security-sensitive per `IsSecuritySensitive`). It:

1. **Assembles a security evidence packet** (see §6) from the already-built `ContextBundle` plus new providers: SAST findings (existing), taint paths (new), and the security-surface tags (new). These already flow in as context layers; the stage additionally extracts them into a structured `SecurityEvidence` struct so the verify stage can reason over them.
2. **Runs a security-routed reviewer.** Introduce a dedicated route (`RouteSec70B`, §7) so the security lens can target a stronger/security-tuned model independent of the general reviewer route the planner picks. The reviewer prompt is the existing reviewer prompt with a security-specialized system preamble (trace source→sink, enumerate trust boundaries, map each finding to a CWE).
3. **Adversarial verify** (§8) each candidate finding before it is allowed to gate.
4. **Classify + severity** via the Foundation-Sec route (§7), attaching CWE IDs and a calibrated severity.
5. **Gate.** Confirmed findings at/above `security.gate_severity` with confidence ≥ `security.min_confidence` drive a kind 1630/1633 status per the existing `status` block semantics, so a PR can be marked blocking. Non-gating findings publish as normal kind 1111 comments.

For latency, Pathway B skips the whole-repo localization step and runs at most a single-vote verify (configurable) so it fits CI budgets.

### 4.3 Where the code goes

- New package `internal/securityreview` — the stage orchestrator (`Stage.Run(ctx, ContextBundle, RepoPath, cfg) SecurityResult`). Keeps `securityscan` (regex SAST) untouched and depends on it.
- `internal/pipeline/runner.go` — call `securityreview.Stage.Run` when enabled; merge `SecurityResult.Findings` into the published set (they are `reviewengine.Finding` values with `category:"security"`, so publisher and dedup work unchanged).
- `internal/reviewengine` — add the `sec70b` and `secclassify` routes (see §7).

---

## 5. Pathway A — Deep audit (new subsystem)

### 5.1 Trigger (event-driven, no polling)

Per `AGENTS.md`, the audit is requested via a **ContextVM JSON-RPC method over kind 25910**, `security/audit`, addressed to the Drydock pubkey. Optionally, an operator-configured repo can auto-audit on a fresh repository-state snapshot (kind 30618) — reacting to the event, never polling.

```
{"kinds":[25910], "#p":["<drydock-pubkey>"], "#method":["security/audit"]}
```

`security/audit` params (inside the gift-wrapped payload where the target is private): `repo_addr` (30617 addressable id), optional `subtree` (path prefix), optional `ref` (defaults to snapshot HEAD), `depth` (`quick|standard|deep`), `since_commit` (incremental audits). Completion and progress are signalled with NIP-90 kind 7000 job-feedback events tagged to the request `e` id — again reactive, not polled.

### 5.2 Orchestration: `internal/auditengine`

The audit is a fan-out/verify pipeline over the repository, bounded by a work budget:

1. **Prepare** — `repo.Manager` provides a checkout at `ref` (reuse existing clone/LRU cache and per-repo mutex).
2. **Build the code-map cache** (§6.1) once for the checkout, keyed by tree hash. Cached on disk under the repo cache dir; incremental audits reuse it.
3. **Deterministic sweep** — run `securityscan` across the whole tree (not diff-scoped: call `ScanFiles` with `diffContent==""` so it scans full files), plus optional SCA (Trivy/Grype/OSV) and secret-scan (gitleaks) shell-outs whose JSON is parsed into evidence. This is the free first pass; it produces the initial candidate set and, importantly, seeds the security-surface map.
4. **Localize & prioritize** — rank files/units by security relevance. Two strategies, config-selected: (a) heuristic — union of SAST hits, security-surface sinks, and recently-changed files by `git log`; (b) model-assisted — the optional **Antares localizer** provider (§6.4) given a CWE prompt returns candidate files. Localization exists to keep the deep-model budget focused; log what was dropped (never silently cap).
5. **Per-unit review** — for each prioritized unit (a function/file plus its blast radius from the code map), assemble a security packet (§6.5) and run the `sec70b` reviewer. Units are processed by a bounded worker pool mirroring `DRYDOCK_PIPELINE_WORKERS`.
6. **Adversarial verify + classify** (§8) — same stage as Pathway B, but audit defaults to the full multi-vote refute panel.
7. **Aggregate** — dedup across units (reuse the ensemble dedup: file + ±2 lines + category), rank by calibrated severity × confidence, diff against the stored baseline so re-audits surface only new findings.
8. **Publish** (§9) — a kind **30619** addressable audit report (public summary + counts), gift-wrapped kind 1059 detail to the requester, and a SARIF artifact for CI ingestion.

### 5.3 Budgeting

Audit depth maps to a token/agent budget so a `deep` audit of a large repo is bounded and observable: `quick` = SAST + heuristic localize + single-vote verify on high-severity only; `standard` = model review of localized units + single-vote verify; `deep` = wider unit set + 3-vote adversarial verify + classification. Emit a progress kind 7000 at each phase.

---

## 6. Input optimization (the quality lever)

Precompute repository intelligence once, cache it, and feed the model compact high-signal *packets* instead of raw files. Everything here is a `contextbuilder.Provider` or a cached artifact the providers read from, so it composes with the existing 8-layer budget system.

### 6.1 Code-map cache (`internal/codemap`, NEW)

A disk-cached, tree-hash-keyed bundle built from the existing `symbols` tree-sitter extractor plus lightweight graph construction:

- **Symbol index** — declarations across the repo (extends `symbols.Extractor` from changed-file-only to whole-tree for audit).
- **Call graph + reverse call graph** — callers/callees per symbol, using the existing ripgrep/git-grep callsite chain (or LSP bridge when available for type-accurate edges).
- **Import/module graph** — from the existing imports-exports extraction.
- **Repo map** — a ranked table-of-contents (PageRank over the symbol reference graph) so the model gets an orientation layer without whole files.

Cache invalidation is by blob hash per file; unchanged files reuse prior entries, which makes incremental audits and every PR review cheap.

### 6.2 Taint / data-flow provider (`internal/contextbuilder`, NEW provider)

Highest-value new evidence. For supported languages, enumerate source→sink paths (untrusted input → dangerous sink) and hand the model the concrete path rather than asking it to find injection cold. Implementation options, in order of preference: LSP-bridge-assisted where a server exposes call hierarchy; otherwise a tree-sitter + call-graph reachability approximation seeded by the security-surface sink set. Emits a `taint` layer at priority 2 (just below patch, above symbols) so it is never budget-dropped for security runs.

### 6.3 Security-surface provider (`internal/contextbuilder`, NEW provider)

A tagged index of security-relevant locations: entry points/routes, auth/authz checks, deserialization, `exec`/subprocess, file I/O, crypto, SQL. Built by extending the `securityscan` rule engine with *locator* rules (same regex infra, but classified as "surface" not "finding"). This both focuses the reviewer and seeds taint sinks. Emitted as a compact `security-surface` layer.

### 6.4 Antares localizer (optional, `internal/auditengine`)

An optional model-assisted localization step using a small vulnerability-localization model (Antares-class) exposed over the same OpenAI-compatible endpoint mechanism. Given a CWE description and the repo map, it returns candidate files. Runs only in Pathway A; gated behind `security.localizer: antares`. Cheap enough to run per-CWE. Falls back to heuristic localization when unconfigured (graceful-degradation pattern, consistent with Qdrant/LSP).

### 6.5 Packet assembly

Per review unit, assemble and pass as the `ContextBundle`/`RunInput`:

```
target code (the function/hunk)
+ blast radius (callers + callees from code map)
+ taint path(s) touching the target
+ SAST + security-surface hits in the target
+ CWE hypothesis (from SAST rule → CWE mapping)
+ associated tests (existing tests provider)
```

Token discipline is the point: a focused, tool-primed packet beats dumping whole files at a bigger model. This is exactly the existing token-budget philosophy, applied with security-relevant priorities.

---

## 7. Model routing and hardware mapping

Reuse the route mechanism verbatim (add a constant in `reviewengine/types.go`, an endpoint field + switch case in `engine.go`, env vars in `config.go`, wire in `cmd/drydock/main.go` — the documented "Adding a New Model Route" recipe).

New routes:

| Route | Purpose | Typical model |
|---|---|---|
| `sec70b` | Deep security reviewer (Pathway A units, Pathway B lens) | GLM-class or Llama-3.3-70B-class security-capable model |
| `secclassify` | CWE/severity classification + write-up | Foundation-Sec-8B-Reasoning (security-specialized, cheap) |
| `seclocalize` (optional) | Vulnerability localization | Antares-1B (audit only) |

Each maps to an independent `DRYDOCK_SEC70B_BASE_URL` / `_MODEL` (etc.), so the operator points routes at whatever endpoints their hardware serves. Two reference profiles:

**RTX PRO 6000 Blackwell Max-Q (96 GB, FP8/FP4, vLLM).** Serve a strong generalist/security reviewer for `sec70b` (a GLM-Air-class MoE or a 70B at FP8) with long context, and co-resident `secclassify` (Foundation-Sec-8B) and `seclocalize` (Antares-1B) in the remaining VRAM. High prefill throughput makes this card comfortable driving *both* pathways, including interactive PR gating.

**2× P40 (48 GB, GGUF via llama.cpp, no FP8).** Prefer this rig for Pathway A batch audits (latency-tolerant). Point `sec70b` at a dense 70B Q4 (overnight) or a low-active-param MoE for interactive use; `secclassify`/`seclocalize` run easily on the second card. Keep Pathway B PR gating off this rig unless using the MoE, because Pascal prefill on large packets is the bottleneck.

Because routing is per-endpoint, the same binary runs on either rig by changing env vars only — no code change. `RunOutput.ServedModel` already records the model the endpoint actually served, so audit reports are labeled truthfully.

---

## 8. Adversarial verification + classification stage (`internal/securityverify`, NEW)

Shared by both pathways; the highest-leverage quality component.

**Verify.** For each candidate security finding, spawn N independent verifier calls prompted to *refute* it ("here is a claimed vulnerability with its taint path and evidence; try to prove it is NOT exploitable / is a false positive; default to refuted if uncertain"). This reuses the ensemble execution machinery but inverts its purpose: ensemble consensus today *boosts* agreement; here we *kill* a finding when a majority of refuters succeed. Diversity beats redundancy — give verifiers distinct lenses (reachability/exploitability, input-controllability, existing-mitigation) rather than N identical prompts.

- Pathway B: default N=1 (latency), configurable.
- Pathway A `deep`: default N=3, majority rules.

**Classify.** Survivors go to the `secclassify` route: assign CWE, calibrate severity (a confirmed reachable RCE ranks above a theoretical weak-hash), and draft the remediation note. Output remains a `reviewengine.Finding` (with CWE in `category`-adjacent metadata; see §10) so downstream publish/dedup/autofix are unchanged.

This directly implements the "verify before you gate" and "false-positive suppression" priorities from §1.

---

## 9. Nostr protocol integration

Follows `docs/event-kinds.md` conventions and the `AGENTS.md` event-driven rules (subscribe/react, verify OK, dedupe, gift-wrap private payloads, EOSE-aware backfill).

**New/used kinds:**

| Kind | Standard | Use |
|---|---|---|
| 25910 | ContextVM | New method `security/audit` (request) — subscribed on `#p` = drydock pubkey, `#method` filter |
| 7000 | NIP-90 | Audit progress + completion feedback, tagged to the request `e` id |
| 30619 | NIP-34-adjacent addressable | **New** security audit report (addressable: `30619:<drydock-pubkey>:<repo-id>:<ref>`), public summary + severity counts + SARIF hash |
| 1059 | NIP-59 gift wrap | Private detailed findings (file/line/evidence/taint) to the requester; only the `p` routing tag is public |
| 1111 | NIP-22 | Per-finding security comments on PR/patch threads (Pathway B), same as today |
| 1630/1633 | NIP-34 | Status gating for blocking PR security findings (reuses `status` block) |

**Privacy:** audit detail (paths, snippets, taint traces, exploitability notes) is sensitive and MUST be gift-wrapped (NIP-59) per the encryption guidance; only non-sensitive routing/summary stays public. Reuse the codechat NIP-17/59 construction already in the tree.

**Why 30619 (addressable):** a repo's latest audit is replaceable state — a new audit for the same `repo-id`+`ref` supersedes the old, and clients can fetch "current security posture" by address, matching how 30617/30618 model repo state. A kind 1111 report comment tagged to the repo announcement is retained as a compatibility fallback for clients that reject project kinds. See **Appendix A** for draft spec text suitable for upstreaming to NIP-34.

**Deprecated-kind discipline:** do not resurrect project-specific kinds (31650, 1651-1654, etc.); the audit request is a ContextVM `25910` method, consistent with how review/fix/marketplace already migrated.

---

## 10. Configuration

### 10.1 `.drydock.yaml` — new `security:` section

```yaml
security:
  enabled: false              # master switch for Pathway B security lens
  gate_severity: high         # confirmed findings >= this drive a 1630/1633 status
  min_confidence: 0.90        # gating threshold (post-verify calibrated confidence)
  reviewer_route: sec70b      # route for the security reviewer
  classify_route: secclassify # route for CWE/severity classification
  verify_votes: 1             # adversarial refute votes in PR lens (audit overrides)
  cwe_taxonomy: true          # attach CWE IDs to findings

  sast: true                  # regex SAST evidence (existing securityscan)
  taint: true                 # taint/data-flow provider
  surface: true               # security-surface provider
  sca: false                  # dependency CVE scan (trivy/grype/osv), if installed
  secret_scan: false          # gitleaks, if installed

  audit:                      # Pathway A defaults
    localizer: heuristic      # heuristic | antares
    depth: standard           # quick | standard | deep
    verify_votes: 3
    auto_on_snapshot: false   # react to kind 30618 snapshots (event-driven)
    sarif: true
```

Validation mirrors existing repoconfig rules (strict unknown-field rejection, severity/confidence range checks). A `security` section that fails to parse follows the existing conservative policy: log and apply defaults, except gating fields fail closed (no accidental "blocking" without valid policy), matching how `payments` already fails closed.

### 10.2 Environment variables

`DRYDOCK_SEC70B_BASE_URL`/`_MODEL`, `DRYDOCK_SECCLASSIFY_BASE_URL`/`_MODEL`, `DRYDOCK_SECLOCALIZE_BASE_URL`/`_MODEL`, `DRYDOCK_SECURITY_ENABLED`, `DRYDOCK_SECURITY_AUDIT_WORKERS`. All optional with graceful degradation: no `sec70b` endpoint → fall back to `llm70b`; no localizer → heuristic; no SCA/secret-scan binaries → skip with a logged notice (never a hard fail).

---

## 11. Data model (SQLite, `internal/db/schema.go`)

New tables, consistent with existing WAL/`MaxOpenConns=1` conventions:

- `security_audits` — one row per audit run: `id`, `repo_id`, `ref`, `depth`, `requested_by`, `state` (`pending→running→published|failed`, reuse the review state-machine pattern and `ResetStuckReviews` recovery), `report_event_id`, `sarif_hash`, timestamps.
- `security_findings` — normalized findings per audit: `audit_id`, `file`, `line`, `cwe`, `severity`, `confidence`, `verified` (bool), `refute_votes`, `fingerprint`.
- `security_baseline` — accepted/known findings per repo by `fingerprint`, so re-audits and PR runs surface only *new* issues and support suppression (`wontfix`).

`fingerprint` = stable hash of (normalized file path, CWE, nearby code shape) so a finding survives line drift — this is what makes incremental audit and baseline diffing work.

---

## 12. Phased implementation plan

Sized for `bd` issues (this project tracks work in beads, not markdown TODOs — these are proposed issue titles, not a task list).

**Phase 1 — Evidence foundations (unlocks both pathways)**
- `codemap`: whole-tree symbol index + call graph + repo map, disk cache keyed by tree hash.
- `contextbuilder`: security-surface provider (locator rules on the securityscan engine).
- `contextbuilder`: taint/data-flow provider (LSP-assisted where available, tree-sitter approximation otherwise).
- SAST→CWE mapping table added to `securityscan` rules.

**Phase 2 — Pathway B (PR/patch security lens)**
- `reviewengine`: add `sec70b` + `secclassify` routes.
- `securityverify`: adversarial refute stage (reuse ensemble execution).
- `securityreview`: stage orchestrator; wire into `pipeline.Runner`.
- `repoconfig`: `security:` section + validation; `config`: env vars.
- Status gating on confirmed high/critical findings.

**Phase 3 — Pathway A (deep audit)**
- `auditengine`: orchestrator, budgeting, worker pool, baseline diffing.
- `contextvm`: `security/audit` method; `listener`/`idegateway` subscription + kind 7000 progress.
- `publisher`: kind 30619 report + gift-wrapped 1059 detail + SARIF artifact.
- `db`: `security_audits` / `security_findings` / `security_baseline`.
- Optional Antares localizer route.

**Phase 4 — Hardening & eval**
- Eval dataset of labeled vulns (extend `eval/heldout-sample.json`) measuring precision/recall and false-positive rate with/without the verify stage.
- Prometheus metrics (audits run, findings by CWE, verify kill-rate, FP rate).
- Meta-review integration for security findings.

Each phase is independently shippable; Phase 2 delivers user-visible value on its own.

---

## 13. Testing, eval, and risks

**Testing** follows repo conventions: event-driven tests with mocked EVENT/EOSE/OK (no sleeps), `testutil.fakellm` for deterministic reviewer/verify/classify outputs, table-driven provider tests. Golden-file tests for packet assembly and SARIF output. The audit state machine gets the same stuck-recovery test coverage as `review_log`.

**Eval** is the acceptance gate: the verify stage must measurably cut false positives without dropping true-positive recall on the labeled set, and PR-lens latency must fit a CI budget on the target hardware. Report FP rate explicitly — a security reviewer that cries wolf is worse than none.

**Risks / open questions:**

1. *Kind 30619* — **Decided:** adopt the new addressable kind (draft spec in Appendix A, proposed for upstreaming to NIP-34). A kind 1111 report comment on the repo announcement is retained only as a compatibility fallback for clients that reject project kinds.
2. *Taint accuracy* without full dataflow analysis is approximate; the design leans on the verify stage to suppress the resulting noise. May want per-language LSP requirements documented.
3. *Whole-repo prefill cost on P40* is real; audit depth budgeting and localization are the mitigations, but very large repos may need a subtree-only default there.
4. *Antares/Foundation-Sec licensing* is defensive-use-restricted — fine for this feature, but document it so operators don't repurpose the endpoints.
5. *SCA/secret-scan* are integrations, not builds; decide whether to vendor invocations or leave them operator-installed (current design: operator-installed, skipped-if-absent).

---

## 14. Summary

The feature decomposes into a shared evidence substrate (code map, taint, security surface), a shared review-and-verify core (a security route, an adversarial refute stage, a classifier), and two thin entry points: a security lens inside the existing pipeline (Pathway B) and a new event-triggered whole-repo audit orchestrator (Pathway A). It reuses Drydock's `Provider` interface, model-route mechanism, ensemble machinery, `.drydock.yaml` policy, and Nostr-native event model rather than introducing a parallel stack, and it runs entirely on local open-weight models with the operator's choice of hardware selected purely through endpoint configuration.

---

## Appendix A — Proposed NIP-34 addition: kind `30619` (Repository Security Audit Report)

Draft text written in NIP-spec style, suitable for upstreaming to [NIP-34](https://github.com/nostr-protocol/nips/blob/master/34.md). It defines a public, replaceable "security posture" record while keeping sensitive finding detail off public relays.

### Repository Security Audit Report

`kind: 30619` — an addressable (parameterized replaceable, per NIP-01) event that records the most recent security audit of a repository at a specific ref. Because it is addressable, a newer audit for the same repository and ref **replaces** the previous one; clients fetch a repository's current security posture by address (`30619:<pubkey>:<d>`) without traversing history, exactly as they do for repository state (`30618`).

The event's public `content` is a **non-sensitive summary only**. Detailed findings — file paths, line numbers, code snippets, and data-flow/exploitability traces — MUST NOT appear in a `30619` event. They are delivered out-of-band to authorized recipients via NIP-59 gift-wrapped events, referenced from the report by a content digest.

#### Tags

| Tag | Req | Value | Purpose |
|-----|-----|-------|---------|
| `d` | yes | `<repo-id>` or `<repo-id>:<ref>` | Addressable identifier. Use the bare repo-id for "latest audit of default branch"; append a ref for per-ref reports. |
| `a` | yes | `30617:<owner-pubkey>:<repo-id>` | The NIP-34 repository announcement this audit is for. |
| `r` | rec | `<commit-id>` | The exact commit/ref audited. |
| `t` | rec | `security-audit` | Topic discovery. |
| `severity` | rep | `<level> <count>` | Per-severity finding counts, one tag each, e.g. `["severity","critical","2"]`, `["severity","high","5"]`. |
| `tool` | rep | `<name> <version-or-model>` | Auditing tools/models, e.g. `["tool","drydock","<ref>"]`, `["tool","sec70b","<served-model>"]`. |
| `report` | rec | `<sha256>` | Digest of the full detailed report delivered via gift-wrap, so recipients can verify integrity. |
| `alt` | rec | human-readable | NIP-31 fallback description. |

#### Content

`content` is a JSON object (public, safe to index and to render a badge from):

```json
{
  "schema_version": 1,
  "ref": "<commit-id>",
  "generated_at": 1737000000,
  "depth": "deep",
  "counts": { "critical": 2, "high": 5, "medium": 8, "low": 3, "info": 1 },
  "cwe_top": ["CWE-89", "CWE-78", "CWE-502"],
  "verified": true,
  "report_digest": "<sha256 of the gift-wrapped detail payload>",
  "detail_delivery": "nip59"
}
```

Consumers MUST treat the absence of a `30619` event as "no audit on record," not as "no vulnerabilities."

#### Rationale

This separates a **public, replaceable posture record** (safe to index, safe to drive a repository "security" badge) from **sensitive finding detail** (which could aid an attacker and is therefore gift-wrapped to authorized recipients only). It reuses the addressable-event semantics NIP-34 already applies to repository state (`30618`), so no new discovery or replacement mechanics are introduced — only a new `kind` and a small, well-scoped tag set.
