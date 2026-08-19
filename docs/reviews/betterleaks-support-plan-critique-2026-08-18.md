# Critique: Betterleaks Support Plan (2026-08-18)

Scope: `docs/plans/betterleaks-support-2026-08-18.md` only. Bias toward deletion and clarification.

## 1. Top 3 under-specified seams

**(a) Where the scan actually runs in the pipeline.** Today the SAST scan happens at step 6e, *after* the LLM review (`internal/pipeline/runner.go:743-762`), and `securityscan.Provider.Build` runs its own independent scan inline (`internal/securityscan/provider.go:32-45`). The plan calls that provider "the model" but proposes the inverse: scan once in the runner, pass a pre-rendered string via `BuildInput.SecretScanContext`. Unspecified: exact insertion point relative to `buildInput` construction (`runner.go:518`) and `AssertPreparedReview` (`runner.go:747`); and if the provider receives a *string*, then rendering lives in the runner, which contradicts work item 2's "`provider.go` … + `FormatContext`". Pick one: (i) provider holds the scanner and caches its result, or (ii) runner scans and renders. Not both.

**(b) `BuildInput` has ≥5 construction sites.** `runner.go:518`, `auditengine/engine.go:692`, `idegateway/handler.go:524`, `agenttools/handlers.go:311`, `contextbuilder/audit_facades.go:147`. Adding an optional field silently no-ops the layer everywhere except the runner — including `idegateway`, a third review entry point the plan never mentions. State explicitly whether IDE-gateway reviews are out of scope (and whether their output path is a leak vector at all).

**(c) Sanitizer coverage list is incomplete.** The plan names `publisher/service.go`, `publisher/security_audit.go`, SARIF. Also copying `Evidence`: `internal/metareview/types.go:114-118` (into its own finding type, then re-serialized) and `internal/auditengine/engine.go:772-775, 834-860` (reads/mutates evidence for CWE tagging). Enumerate which hops must preserve `Sensitive` and which are terminal.

## 2. Contradictions / missing dependencies

- **Reordering not called out.** Work item 5's "failure before LLM" test only holds if the scan moves ahead of the review. That is a pipeline reordering (from 6e to ~5), with a real behavior change: scan failures now abort before any LLM spend. Say so.
- **Wrong parser named.** Work item 1 says export "the added-line diff parser from `internal/securityscan/scanner.go`", but file-level extraction lives in `provider.go:70+` (`extractChangedFiles`). The runner already has `analysis.ChangedFiles`, so only the *line-range* parser needs exporting — if that.
- **`Sensitive` and JSON.** `reviewengine.Finding` is unmarshalled from LLM output (`metareview/types.go:274`). Decide and state: `Sensitive` is `json:"-"` — never settable or clearable by model output.
- **Policy filtering interaction.** Dedup output flows through `applyReviewPolicy` and `FilterOutputToChangedFiles` (`runner.go:766-772`). Unspecified whether a `critical`/0.99 validated-credential finding can be dropped by a repo's severity threshold or the changed-file filter. It should not be.

## 3. Over-planning — cut or simplify

- **Cut the span-match sensitivity restoration** (work items 3 and 4). `securityverify.Engine.Run` filters and classifies **in place**; `llmClassifier.Classify` mutates a copy of the input struct and returns it (`internal/securityverify/engine.go:98-118, 287-291`). A `Sensitive bool` field propagates for free. This deletes a lossy heuristic and its tests.
- **Collapse the fixture matrix.** "Fixtures for all validation states" is backwards while the Open Question stands. The severity table has two branches (validated-success vs. everything else) — one fixture per branch is sufficient.
- **One redactor, not three.** "Canonical safe evidence text", "dedup canonicalization", and "publication sanitizer" should be a single exported function called from two places. Keep dedup-time marking (LLM findings that quoted the secret out of the diff are a real vector); drop the separate canonical-text concept.
- **Trim the `--validation` gating section** to one line ("operator env, default off") and move the trust-boundary rationale to `docs/security-review.md`. It is currently 3× longer than the code it implies.

## 4. Questions that would change implementation order

1. **Is the publication sanitizer independent of betterleaks?** `service.go:486` publishes `Evidence` verbatim *today*, including existing SAST findings. If so, work item 3 is a standalone pre-existing-leak fix that should ship **first and alone**, not third behind a new dependency.
2. **Provider shape (2a above)?** Determines whether item 5 blocks item 2 or the reverse.
3. **Does `betterleaks@v1.7.4` actually resolve in the Go builder?** Unverified here. If the pin does not build, item 7 gates everything and moves to first — not last.
4. **Is `idegateway` in scope?** If yes, item 5 roughly doubles.
