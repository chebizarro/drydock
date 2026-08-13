# Critique: Agentic Review Plan (2026-08-12)

Scope: structural critique of `docs/plans/agentic-review-2026-08-12.md` only. Three claims spot-checked against source; no other exploration.

## 1. Top 3 under-specified seams

**S1 — "exact token count" is not exact, and it is the halt gate.**
Plan §Discovery loop: `selection.finalize` "counts real serialized tokens (not estimates)" and "refuses to freeze if over budget." The existing counter (`internal/contextbuilder/tiktoken.go:25-42`) silently downgrades to `ApproxTokenCounter` whenever the cl100k data can't be loaded (5s timeout, network-dependent first use), and cl100k is an approximation for any non-OpenAI model anyway. A fail-closed gate built on a counter that degrades by a `slog.Warn` means discovery can become unhaltable on a cold/offline box. Specify: which counter is authoritative, whether approximation is a hard startup error in agentic mode, and the budget headroom that absorbs tokenizer skew.

**S2 — no failure path when discovery exhausts its budget without finalizing.**
`selection.finalize` is "the sole success stop condition," limits are 24 turns / 96 tool calls, and finalize can refuse. The plan never says what happens on turn exhaustion or repeated refusal. `Builder.Build` is described as a *rollout-flag* fallback, not an error fallback — so on the agentic path there is no defined outcome. Compounded in ensemble: members share one package but have isolated transcripts, and `review.submit` is likewise the only success stop; consensus merge (`internal/reviewengine/ensemble.go:43-124`) presumably assumes N results. Specify per-member failure semantics (drop / degrade / fail run) before item 8.

**S3 — P0/P1/P2 "mapped to legacy severity" is a live contract, not a rename.**
`internal/reviewengine/ensemble.go:313-314,369-370` sorts and ranks on `strings.ToLower(result[i].Severity)` via `sevOrder`/`severityRank` maps. Item 8 adds priority to `Finding` with the mapping direction unstated; if either map misses a key the rank silently becomes zero and P0s sort last. This flows into consensus, SAST merge, and publication envelopes. Name the canonical field and the total mapping in item 1's output, not item 8's implementation.

## 2. Contradictions / missing dependencies

- **Snapshot GC vs. session continuation.** Item 4 gives snapshots "leases/expiry/GC"; item 10's `Continue` reloads the frozen selection and verifies manifest hashes. No stated relationship between lease TTL and session lifetime — an expired snapshot makes a stored session permanently unresumable. Item 10 depends on item 4's lease policy; that edge isn't in the plan.
- **Open Question deferral vs. IDE freeze.** Snapshot storage strategy (copy-on-write vs. full copies) is deferred to "decide during item 4," but item 13 freezes the *live workspace* on every inline IDE request — the highest-frequency, lowest-value snapshot in the system. That decision gates item 13's viability and should precede item 4, not sit inside it.
- **External `review.submit` has no sink.** Item 15 defines an `external_reviewer` role and `review.submit` is a registry tool, but nothing describes where an external MCP client's submitted findings land (which session, which snapshot, what authorization beyond per-connection role). Either drop the role or specify the binding.
- **`runner.go` cited at two ranges** (`206-264` background, `490-620` migration) in a 1150-line file; the plan also admits `auditengine/engine.go:240-359` was "mapped but not fully read." Item 1 is doing load-bearing discovery that the rest of the plan already assumes as settled.
- **VS Code extension protocol change** (item 13) crosses a repo/release boundary with no compat window or versioning story.

## 3. Over-planning — cut or simplify

- **Cut `internal/mcpserver` + `cmd/drydock-mcp` (item 15) from this plan.** The stated goal is review accuracy and depth; MCP serves *external* agents and adds dual capability enforcement, transport auth, and per-connection scope binding to every tool in items 5–6. It's a separate plan. The "one implementation, one security policy" argument holds equally if MCP arrives later against a stable registry.
- **Shrink `internal/workspacesnapshot` (item 4).** For the stored-patch pipeline the patch is already immutable and git is already content-addressed: a pinned commit SHA + patch ref + path allowlist is a snapshot. Manifest SHA-256 of every file, leases, and GC is infrastructure for a mutation problem that hasn't been observed. Start with the pinned ref; add hashing only where the IDE's live workspace genuinely requires it.
- **Defer sessions (items 10–11 `StartSession`/`Continue`, five SQLite tables).** The plan's own rollout puts sessions fourth, and Open Question 3 shows the demand model is unsettled ("on request"). Build `Prepare`/`ReviewPrepared` only; sessions land when a caller asks. Two tables, not five, if in-process TTL turns out to suffice.
- **Trim the tool list.** `security.trace`, `code.references` (LSP), and `context.layer` are speculative surface — each is a handler, a schema, a capability row, and an escalation test. Ship `file_tree / structure / search / read / git.read / selection.* / review.submit`; add on evidence.
- **Item 3's "require non-nil token counter at construction"** is a breaking constructor change across all providers buried inside a refactor item. Either make it its own trivial item or drop it in favour of a package-level default.

## 4. Questions that would change implementation order

1. **Is there an authoritative tokenizer for the configured model?** If not, the finalize gate is an estimate and its semantics (hard refuse vs. refuse-with-headroom) must be settled before items 6–7 are written — currently they're written as if exactness is free.
2. **Is external MCP access a requirement now or aspirational?** "Aspirational" removes item 15 and the dual-enforcement/protocol-shape constraints from items 5, 12–14.
3. **Must a continued session survive snapshot GC and process restart?** "Yes" promotes the storage-strategy Open Question ahead of item 4 and makes item 10 a hard dependent. "No" collapses the schema and lets sessions stay in-process.
4. **Does anything downstream of consensus (SAST merge, publication envelopes, meta-review) key on the legacy severity string?** If yes, item 8 is a breaking cross-cutting change and belongs before item 3's facade extraction, so both land in one contract break.
5. **Is the VS Code extension released independently of drydock?** If yes, item 13's protocol addition must be split and started early to open a compat window, ahead of item 12.
