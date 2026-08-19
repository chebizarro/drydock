# Betterleaks Support in Drydock: Plan

## Goal
Replace the gitleaks integration with betterleaks as drydock's secret scanner, running it in both full-repository audits and the per-patch/PR review path, with redacted findings, repo config/baseline passthrough, and opt-in live credential validation. Install betterleaks in the runtime Docker image.

## Background
- Drydock is a Nostr-native automated code-review service (README.md:5-31).
- Secret scanning today: gated by `security.secret_scan` (default false, `internal/repoconfig/config.go:88-102`); audit engine runs `gitleaks detect --source <repo> --report-format json --report-path - --exit-code 0` with a fallback retry for older versions (`internal/auditengine/engine.go:893-912`). Audit-only; the PR review path has no external secret scanner.
- External tools run via the mockable `ToolRunner` seam (`internal/auditengine/engine.go:108-135`) with PATH detection, generic JSON normalization into `reviewengine.Finding` (`engine.go:923-983`).
- Review path: isolated checkout at `prep.RepoPath` (`internal/pipeline/runner.go:360-364`), authoritative diff `PatchAnalysis.FilteredDiff`, changed files as repo-relative `[]string`. Context providers implement `contextbuilder.Provider` (`internal/contextbuilder/builder.go:108-112`); `securityscan.Provider` (priority 1) is the model for a deterministic scanner feeding LLM context (`internal/securityscan/provider.go`), and post-review dedup/merge happens in `internal/securityscan/dedup.go` + `runner.go:744-784`.
- **Leak risk**: detail events publish `Finding.Evidence` verbatim to public Nostr relays (`internal/publisher/service.go:464-491`); no secret redaction exists anywhere in the publish path.
- Docker: runtime stage is `alpine:3.22` with no scanners installed (`Dockerfile:13-15`); tools must land in the runtime stage's PATH.
- Prior art: `docs/security-review.md:245-309` documents the current gitleaks/SCA behavior; no prior plans on secret scanning.

## Betterleaks facts (v1.7.4)
- Successor to gitleaks by the same authors; compatible with `.gitleaks.toml`, `.gitleaksignore`, gitleaks JSON baselines.
- Commands: `dir`, `git`, `stdin`, `validate`; flags: `--config`, `--report-format json|sarif|csv`, `--report-path -`, `--baseline-path`, `--exit-code`, `--redact`, `--validation`.
- Config: TOML `.betterleaks.toml` (falls back to `.gitleaks.toml`).
- Distribution: release binaries, `go install github.com/betterleaks/betterleaks@latest`, `ghcr.io/betterleaks/betterleaks` image.
- Links: https://github.com/betterleaks/betterleaks · docs/scanning.md in that repo · releases/latest.

## Approach

One shared `internal/betterleaks` package serves both paths. Patch reviews run betterleaks **once** before context construction; the result feeds a priority-1 context layer and is reused for post-review merge — no duplicate scans (or duplicate validation calls).

**Invocation** (no gitleaks fallback binary):
```
betterleaks dir --redact --report-format json --report-path - --exit-code 0 \
  [--config <materialized>] [--baseline-path <materialized>] [--validation] <repo-path>
```
- `dir` (not `git`): both paths review a pinned snapshot; findings must map to files present for localization/code map.
- Run from a private temp dir; materialize config/baseline via `git show` from a **trusted commit** (`PolicyRef`): base commit for patch reviews (a patch can't weaken policy to hide its own secret), audited commit for audits. Resolution order: `.betterleaks.toml` → `.gitleaks.toml`; baseline `.betterleaks-baseline.json` → `.gitleaks-baseline.json` (fixed constant paths only — never interpolate repo-supplied paths).

**Parsing**: typed decoder (not the generic JSON walker in `engine.go:923-983`). Read only RuleID/File/StartLine/EndLine/validation-state; **never** copy `Secret`/`Match`/stderr into findings. Canonical safe evidence text; `Sensitive: true`. Severity: validated-success → `critical` (confidence 0.99); everything else (disabled/unsupported/failed/invalid) → `high` (0.90). Verify the exact v1.7.4 validation JSON field against fixtures generated with the pinned binary.

**Filtering**: normalize paths against RepoPath (reject escapes); patch scans restrict to changed files ∩ added-line ranges (reuse the existing diff parser from `securityscan`); audits use the code-map file allowlist.

**Fail closed**: when `secret_scan` is enabled, missing binary / command failure / malformed JSON fails the review or audit rather than silently publishing an incomplete result (change from today's log-and-skip).

**Sensitivity propagation + publication defense-in-depth**: add `Sensitive bool` to `securityscan.SecurityFinding` and `reviewengine.Finding` (plus `EndLine`). Dedup marks overlapping LLM findings sensitive and canonicalizes their text. No verifier-restoration step is needed: `securityverify.Engine.Run` classifies findings in place (`internal/securityverify/engine.go:98-118, 287-291`), so `Sensitive` propagates for free. A shared publication sanitizer replaces evidence/explanation/suggestion/suggested-code of any sensitive finding with fixed safe text in `PublishReview` (before outbox reservation) and in audit Nostr/SARIF output — so neither scanner nor LLM-copied secret text can reach public relays (today `service.go:464-491` publishes evidence verbatim). Note this sanitizer is independent of betterleaks and fixes an existing leak — it ships first.

**Provider shape**: unlike `securityscan.Provider` (which scans inline in `Build`, `provider.go:32-45`), the betterleaks provider is a thin pass-through of a pre-rendered `BuildInput.SecretScanContext` string — the pipeline scans once and formats via `betterleaks.FormatContext`. `securityscan` is the model only for priority (1) and the post-review merge, not for provider execution. `BuildInput` has multiple construction sites; sites that don't populate the field (e.g. `internal/idegateway/handler.go:524`) simply get an empty layer — acceptable for v1, noted in docs.

**Validation gating**: operator-only env `DRYDOCK_BETTERLEAKS_VALIDATION` (default false); repo config cannot set it (`KnownFields(true)` keeps rejecting unknown fields). When on, applies to every repo with `secret_scan: true`. Rationale: validation transmits found credentials to third-party endpoints from drydock's network — same trust boundary as operator-only `authorized_targets`.

**Docker**: build pinned `betterleaks@v1.7.4` in the existing Go builder stage (ARG `BETTERLEAKS_VERSION`), copy to `/usr/local/bin` in the alpine runtime stage; smoke-check `betterleaks version`. Do not ship gitleaks.

## Work Items

0. **Publication sanitizer (standalone leak fix — ship first)** — add `Sensitive bool` to `reviewengine.Finding` and `publisher.SecurityAuditFinding`; sanitize sensitive findings in `internal/publisher/service.go` before summary/detail construction and in `internal/publisher/security_audit.go` before Nostr + SARIF. Tests assert no raw evidence in events. Independent of betterleaks.
1. **Metadata + shared helpers** — add `Sensitive`/`EndLine` to `securityscan.SecurityFinding` (Finding side done in item 0); export the added-line diff parser from `internal/securityscan/scanner.go` for reuse. Regression tests.
2. **`internal/betterleaks` package** — `types.go` (scanner iface, `CommandRunner`, severity mapping), `scanner.go` (lookup, policy materialization, exec, typed parse, path/line filtering), `provider.go` (priority-1 `contextbuilder.Provider` reading a new `BuildInput.SecretScanContext` string + `FormatContext`). Fixtures for all validation states; tests for args (`--redact` mandatory), config/baseline precedence, validation gating, cancellation, fail-closed cases, and absence of raw secret data.
3. **Dedup extension** — extend `internal/securityscan/dedup.go`: sensitive scanner findings mark/canonicalize overlapping LLM findings (±3 lines against the scanner span); unmatched scanner findings convert with `Sensitive: true`. No verifier-restoration helper (see Approach). Tests: matched/unmatched, category mismatch, multiple overlaps, non-sensitive regression.
4. **Audit engine swap** — `internal/auditengine/engine.go`: add `SecretScanner` dependency, delete the gitleaks branch from `runOptionalTools` (leave SCA), fail enabled audits on scan error, report `betterleaks` in audit tool metadata. Tests via a fake scanner (note: current `engine_test.go` has no ToolRunner fake — add coverage).
5. **Pipeline wiring (land atomically with provider registration)** — `internal/pipeline/runner.go`: `WithBetterleaksScanner` option; single scan after patch analysis using `prep.RepoPath`/base commit/`analysis.ChangedFiles`/`FilteredDiff`; populate `SecretScanContext`; reuse findings in the post-review dedup. Runner + integration tests (scan-once, disabled = never invoked, failure before LLM).
6. **Operator config + main wiring** — `DRYDOCK_BETTERLEAKS_VALIDATION` in `internal/config`; construct scanner and register provider in `cmd/drydock/main.go` (same path as `securityscan.Provider`, `main.go:441-448`).
7. **Dockerfile** — pinned build + runtime copy + smoke check.
8. **Docs** — update `docs/security-review.md` (replace gitleaks behavior; document redaction, fail-closed semantics, severity mapping) and `docs/configuration.md` (config/baseline filename conventions, base-commit policy timing, operator env var).

## Decisions Resolved
- `--validation` is **operator-only**, default off (see gating rationale above).
- Severity mapping: validated → critical; all other states → high; explicitly-invalid credentials still reported (unsafe handling regardless).
- Snapshot scanning via `dir`; fixed baseline filename conventions.

## Risks & Migration
- Deployments outside the shipped image must install `betterleaks` on PATH — enabled scans now fail closed; gitleaks no longer consulted.
- `.gitleaks.toml`/`.gitleaks-baseline.json` remain supported as fallbacks; no DB migration (evidence isn't stored).
- Policy timing: a PR introducing betterleaks config won't scan itself with it (base-commit policy); active after merge.
- Rollback requires an image with gitleaks — the new image intentionally omits it.

## Verified Facts (2026-08-18, pinned binary + source at tag v1.7.4)
- `go install github.com/betterleaks/betterleaks@v1.7.4` resolves and builds at the repo-root module path (binary prints version `dev`; add ldflags in Docker if a version string matters). Dockerfile item stays last.
- JSON report is `[]report.Finding` via `encoding/json`, no field renames (report/finding.go). Fields: `RuleID`, `File`, `StartLine`, `EndLine`, `Match`, `Secret`, `Description`, `Fingerprint`, `Entropy`, plus `Attributes` map. **An empty report serializes as `null`** — decoder must treat `null` as zero findings.
- With `--redact`, `Secret`/`Match` are still present with value `"REDACTED"` (or embedded in the match line) — the typed decoder must still ignore/drop them.
- Validation state: `ValidationStatus` (`json:",omitempty"` — absent when not validated). Constants (report/validation_status.go): `"valid"`, `"needs_validation"`, `"invalid"`, `"revoked"`, `"unknown"`, `"error"`, and empty/omitted. Optional `ValidationReason` string and `ValidationMeta` map also appear.
- Severity mapping (final): `ValidationStatus == "valid"` → `critical` (confidence 0.99); all other values or absent → `high` (0.90).

## Out of Scope
- IDE-gateway review path (`internal/idegateway/handler.go:524`): will use `secretlint` instead of betterleaks — separate follow-up integration. Its `BuildInput` sites leave `SecretScanContext` empty, which yields an empty layer; no changes needed here.

## References
- betterleaks: https://github.com/betterleaks/betterleaks (v1.7.4, 2026-08-10)
- drydock docs/security-review.md
