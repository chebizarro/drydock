# Investigation: Drydock review/PR mismatch (2-week ~800-review corpus)

## Summary
(TBD)

## Symptoms
- Reviews sometimes reference files not in the PR/patch diff, or clearly from a different PR
- Some reviews explicitly state the diff/file content is missing from provided context (e.g. review event d04e3b31... : "patch section contains a Nostr event describing the change, but does not include the actual file content or a diff")
- Overall review quality mixed; unclear how meta-review ("code review review") with frontier model works — meta-review-log.jsonl in corpus has only a header row (0 data rows)

## Corpus
`/Users/bizarro/Documents/Projects/drydock-test-corpus`
- manifest.json: 858 db review events, 777 relay union, schema drydock.review-corpus.v1
- review-log.jsonl (1996 rows), review-events-db.jsonl (858), review-publication-outbox.jsonl (857)
- drydock.db (7.3GB sqlite), drydock-container.log (678MB)
- Source code: `/Users/bizarro/Documents/Projects/drydock`

## Background / Prior Research

## Investigator Findings

### Executive conclusion

The mismatches are not caused by one cache returning another job's prompt. The corpus and source show **four distinct target-integrity failures**, in descending order of demonstrated impact:

1. **Historical kind-1618/1619 prompt bug (confirmed, and already fixed in current source):** Drydock used the PR event's prose `Content` as though it were a unified diff. It did **not** put `raw_event_json` in the prompt; it put the cover letter/description extracted from that JSON. The corpus contains 55 model-backed review summaries that explicitly report that the implementation/diff was absent.
2. **Wrong/overbroad PR base (confirmed in the live corpus):** current code diffs the PR tip against the cached canonical clone's `origin/HEAD`/main/master. For the Amethyst events that generated repeated irrelevant findings, the advertised canonical remote was stale: the resulting Drydock diff had 1,709–4,608 files and 21.6–42.3 MB, while the target tip commits changed only 1–17 files. Those huge diffs contain the apparently unrelated files, so the changed-file guard accepts them; the 40 KiB patch-layer cap then biases the LLM toward early, unrelated paths.
3. **Shared mutable checkout race (confirmed code defect and confirmed runtime exposure):** every concurrent review for a `repo_id` shares one checkout, and locks cover individual Git operations rather than the full review lifetime. In the captured log, 87 of 324 successfully prepared task intervals overlapped another successfully prepared task for the same repo. Later providers/scanners/autofix can therefore read a different PR's checkout.
4. **Kind-1617 root/series versus revision mismatch (confirmed):** Drydock applies every stored kind-1617 event in the root thread, but the prompt patch layer and published target remain the selected event only. Fifteen corpus failure reviews target one revision while their `Reason: patch ...` names an earlier root-series member.

There is no evidence of a primary-review result cache mixing arbitrary jobs. `context_hash` is only SHA-256 of the rendered bundle and is used for publication/meta-review reuse; it is not a primary-review cache key.

### Corpus accounting and method

Read-only SQLite queries used `file:drydock.db?mode=ro&immutable=1`; event text came from `review-events-db.jsonl`; task/concurrency and meta-review measurements came from streaming JSON parsing of `drydock-container.log`.

- The DB contains 858 kind-1111 events associated with **721 distinct patch/repo targets**: 720 summary outbox rows and 137 detail rows (one DB event is not in the outbox export). Target kinds are 597 kind-1618, 78 kind-1619, and 46 kind-1617.
- Of the 721 target-level summary events, 344 have `model: none` (mostly apply/fetch failures) and 377 are model-backed: Gemma 176, `llm70b` 154, `coder32b` 38, and `coder14b` 9.
- A mechanically selected “diff/patch missing or not provided” set produced 56 candidates; manual inspection removed one lexical false positive (“sha1sum is not available”), leaving **55 true missing-diff reviews**: 55/721 = 7.6% of all target summaries, or 55/377 = **14.6% of model-backed summaries**. All 55 target kind 1618. Models: `llm70b` 40, `coder32b` 11, `coder14b` 4. Repos: strfry 29, armada 5, ngit 3, two each in two Amethyst repo identities and nostr-git, and one each in 14 other repos. Their timestamps are 2026-07-18 21:02 UTC through 2026-07-19 21:27 UTC (54 on July 19).
- The exact literal phrase “missing from the provided context” occurs in 11 exported events (9 strfry, 1 Citrine, 1 editor; 6 coder32b, 3 llm70b, 2 Gemma).
- For the only clean subset where the stored event itself is an authoritative unified diff—20 successful kind-1617 review outputs—the target diffs were parseable and yielded 84 walkthrough/finding file references. **0/84 referenced paths were outside their selected event's diff.** This is important negative evidence: the broad mismatch is concentrated in PR diff/base handling and series/failure handling, not a universal publisher attachment bug.

### 1. What actually enters the prompt

The worker prepares and then reloads exactly `task.PatchEventID` (`internal/pipeline/runner.go:302-306,337-345`). Current source selects event `Content` for kind 1617, but replaces it with `prep.Diff` for kinds 1618/1619 and fails closed if no PR diff exists (`internal/pipeline/runner.go:412-425`). It passes that value as `BuildInput.PatchEventContent` (`internal/pipeline/runner.go:427-446`).

The patch provider emits the trimmed value verbatim but caps it at 40 KiB (`internal/contextbuilder/providers.go:62-76`). The builder then renders it as `## patch\n...` with the other layers (`internal/contextbuilder/builder.go:295-337`). Planner, reviewer, and walkthrough receive the same bundle through `plannerUserPrompt`, `reviewerUserPrompt`, and `walkthroughUserPrompt` (`internal/reviewengine/prompts.go:42-44,84-97`; calls at `internal/reviewengine/engine.go:75-119`).

**Historical deployed behavior explains the 55 missing-diff outputs.** Immediately before commit `e090e6d817114d7f46e1feb588fae33c29e8e472`, the runner unconditionally did:

- `patchDiffContent := patchEvent.Content`; then
- `PatchEventContent: patchDiffContent`

at `e090e6d^:internal/pipeline/runner.go:322-336`. At the same revision, PR preparation checked out the tip but returned no diff at `e090e6d^:internal/repo/service.go:134-148`. Commit `e090e6d` (2026-07-19) explicitly says: “PR-style NIP-34 events carry a cover letter in event content, not a diff. The pipeline fed that prose to the context builder.”

Thus hypothesis (a) is **close but not literally correct**: the prompt did not receive the entire raw Nostr JSON envelope. Ingest persists both `event.Content` and `event.String()` in separate columns (`internal/db/store.go:909-927`), and the runner unmarshals the latter but uses `patchEvent.Content`. For exemplar target `18c600bc...`, the DB `content` is a 354-byte prose description while `raw_event_json` is a separate column; review event `d04e3b31...` accurately observes that the patch section describes the change but has no file content/diff (`review-events-db.jsonl:1`). The model called it a “Nostr event,” but the supplied text was the event's cover letter.

Commit `f917cff745de6039fb8c0cfed9c59aca26b1f2c6` added changed-file anchoring after an observed kind-1618 review presented README/CONTRIBUTING as modified files. Current filtering drops reviewer findings and walkthrough file summaries not in the parsed changed set (`internal/reviewengine/changedset.go:30-85`). That guard is effective only if Drydock computed the correct diff.

### 2. The current PR diff can be valid Git but wrong for the target

For kinds 1618/1619, Drydock extracts the target tip, checks it out, fetches the tip into the separately cached canonical clone when possible, and computes `git diff <merge-base> <tip>` (`internal/repo/service.go:161-198`). `DiffAgainstDefaultBranch` chooses the first existing `origin/HEAD`, `origin/main`, or `origin/master`; it does not use a base commit asserted by the PR event or a repository snapshot current at the PR's creation (`internal/repo/manager.go:235-274`).

A reproduction using the canonical clone URL stored in the corpus explains the repeated Amethyst mismatch exactly:

- Event `7b8f9054...` targets tip `9a53b0ff...` (`drydock-container.log:4865`).
- The advertised canonical remote's `origin/main` resolves to `ff71bc14...`, dated 2026-06-09, and is the merge-base selected by the current algorithm.
- `git diff ff71bc14... 9a53b0ff...` is **3,251 files / 32,602,877 bytes**; `git diff-tree -r 9a53b0ff...` is only **7 files**.
- Both repeatedly published paths—`.github/actions/import-macos-cert/action.yml` and `commons/.../PrivacyLockStateTest.kt`—are absent from the 7-file tip delta but present in the 3,251-file stale-base diff.
- Meta-review for that event attempted a 9,508,470-token request and was rejected against a 262,144-token limit (`drydock-container.log:7573`). The 32.6 MB reproduced diff and that token count are mutually corroborating evidence that the enormous diff, not just a small correct PR delta, flowed through the live pipeline.

Across 17 fetched Amethyst tip commits implicated by these repeated findings, Drydock's merge-base algorithm yields **1,709–4,608 files / 21.6–42.3 MB**, versus **1–17 files in each tip commit**. Extending the reconstruction to 33 target events found 28 targets with published file references: all **300/300 references were outside the tip-commit delta**, while **0/300 were outside Drydock's enormous merge-base diff**. The two same unrelated findings recur on 28 and 22 distinct PR targets respectively (all published under the Gemma model).

This also explains why the July-19 changed-set fix did not prevent these reviews: changed files are parsed from the full, wrong diff before token budgeting (`internal/contextbuilder/builder.go:222-235`), so thousands of unrelated paths are “authorized.” The actual patch layer is then cut to the first 40 KiB (`internal/contextbuilder/providers.go:62-76`), making early paths such as `.github/.../action.yml` disproportionately visible. The filter correctly enforces membership in the wrong set.

Existing cached clones are only `git fetch --all --prune`; `EnsureRepo` does not verify or change `origin` when later events for the same repo ID advertise different fork URLs (`internal/repo/manager.go:78-101`). If fetching into the canonical clone fails, PR preparation deliberately falls back to diffing in the PR/fork clone (`internal/repo/service.go:178-187`), where a fork-controlled or stale `origin/HEAD` determines the base. Both paths need an explicit, provenance-checked base.

### 3. Shared checkout/concurrency is a real second source of cross-PR bleed

Ordinary PR reviews for one `repo_id` share `SHA256(repo_id)`; the canonical clone uses a separate `SHA256("canonical\\x00"+repo_id)` path (`internal/repo/manager.go:427-441`). Locks are in-memory mutexes keyed only by repo path (`internal/repo/manager.go:24-29,500-503`) and are released after each Git operation. Checkout is protected only while reset/clean/fetch/checkout executes (`internal/repo/manager.go:210-233`); the runner then spends the rest of the review indexing, building context, calling LLMs, scanning, and possibly autofixing with no lifetime lock (`internal/pipeline/runner.go:400-446,500-619`).

Filesystem providers read `in.RepoPath` directly after preparation; for example, file context joins each changed path to the shared working tree and calls `os.ReadFile` (`internal/contextbuilder/providers.go:78-112`). Cleanup later performs `git checkout -` and deletes the branch, so its result depends on intervening checkouts (`internal/repo/manager.go:320-352`). Kind-1617 preparation is additionally unsafe because it resets the current `HEAD` and creates the review branch from that current checkout rather than explicitly from the canonical default ref (`internal/repo/manager.go:136-163`).

Runtime quantification from 60,268 task-attempt intervals in the container log:

- 324 intervals successfully logged “prepared PR tip” or “prepared patch series.”
- 101/324 overlapped some other same-repo task interval.
- **87/324 overlapped another successfully prepared same-repo interval** (55 overlapping pairs).
- Example: Amethyst event `584c77e...` prepared tip `c984d88a...` at 20:44:02 and ran until 21:27:14, while `0bbfa6e8...` prepared a different tip `2f6868f1...` at 20:46:52 and ran until 21:46:02 (`drydock-container.log:7557,7930,11616,12417`). The second checkout therefore replaced the filesystem beneath the first task for roughly 40 minutes.

This proves exposure to cross-PR filesystem reads. It does **not** prove that every mismatched review came from the race; the historical cover-letter and stale-base defects independently explain large measured subsets. The appropriate fix is per-review worktrees (or a repo lock held from prepare through every filesystem-dependent stage and cleanup), with checkout identity/base/tip assertions before context building and scanning.

### 4. Root versus revision handling for kind 1617

`RootEventID` prefers a marked root `e/E`, then any `E`, then any `e` (`internal/db/store.go:1926-1950`). For a selected kind-1617 event, `ListPatchThreadEvents` loads **all** kind-1617 rows with the same `root_id` and `repo_id` (`internal/db/store.go:1052-1081`); `preparePatchSeries` orders and applies all of them and records all IDs (`internal/repo/service.go:80-117`; ordering logic `internal/repo/series.go:10-75`). But `PrepareResult.Diff` remains empty, so the prompt's patch layer is only the selected target event's `Content` (`internal/pipeline/runner.go:412-420`).

Corpus evidence:

- 34/46 kind-1617 review targets belong to roots containing 2–6 stored patch events.
- Fifteen apply-failure summaries publish against one target/footer patch ID but say `Reason: patch <different ID> does not apply`; e.g. target `a0d3306b...` reports failure of earlier series member `d493f81f...`. This is deterministic series semantics leaking through a per-revision publication interface.
- Successful kind-1617 file anchoring was clean (0/84 absent refs), so the observed kind-1617 mismatch is principally failure attribution/cumulative checkout versus selected-event context, not random file hallucination.

Drydock should decide explicitly whether a job reviews a whole patch set/root or one revision. A set review should synthesize and prompt with the cumulative applied diff, publish all applied IDs/base identity, and attach at root/set scope. A revision review should apply only the root-to-selected ancestry required for that revision and attribute failures to both the failing member and requested target.

### 5. `context_hash` and cache verdict

The primary pipeline computes `context_hash = SHA256(bundle.Content)` only (`internal/pipeline/runner.go:576-581`). It does not bind repo ID, patch/root ID, base commit, tip commit, or checkout path/state. It therefore detects that text changed but cannot prove that the text belongs to the requested target. Primary planner/reviewer/walkthrough calls always receive the current `RunInput`; no primary result reuse was found.

Corpus hashes provide no evidence of arbitrary cross-repo job mixing: 712 non-empty hashes produced only 7 hashes reused across distinct target event IDs (35 events), none across repo IDs; most such targets advertise the same tip commit or duplicate PR. Reuse by exact context hash plus changed-line Jaccard exists only in meta-review (`internal/metareview/service.go:128-152`; `internal/db/store.go:1723-1745`).

A target-integrity hash should cover a canonical envelope such as `repo_id, root_id, patch_event_id, canonical_remote_identity, base_commit, tip_commit, diff_sha256, bundle_sha256`, and these values should also be published for auditability.

### 6. How meta-review works, and why the export has only its header

Startup always constructs a meta-review service—there is no feature-enable boolean—with a 15% random sample, 0.85 reuse threshold, few-shot cap 500, and concurrency 2 (`cmd/drydock/main.go:536-556`). After a successful review/status/autofix path, the runner calls it asynchronously with the full patch diff, bundle, hash, changed files, filtered review, and verified-security outcomes (`internal/pipeline/runner.go:681-694`).

A run triggers for verified security outcomes, mean finding confidence below 0.7, a changed path containing auth/crypto/security, or the 15% random sample; otherwise it returns without a row (`internal/metareview/service.go:123-127,191-221`). It first tries hash/Jaccard reuse, otherwise calls its configured endpoint at temperature 0.1 and parses strict structured JSON (`internal/metareview/service.go:128-174`).

Despite descriptions calling it “frontier,” the code does not dynamically select a frontier model. Development and Compose defaults are a dedicated local `llama-3.3-70b-instruct-q4_k_m` endpoint on port 11436 (`internal/config/config.go:24-34,231-233`; `docker-compose.yml:34-44`). Production can override the URL/model; the corpus does not preserve those environment values. Endpoint reachability failures are startup warnings, not fatal errors (`internal/config/config.go:498-519`).

Persistence is SQLite, not an application JSONL writer. The `meta_review_log` schema matches the sole line in the exported `meta-review-log.jsonl` exactly (`internal/db/schema.go:242-253`). `InsertMetaReviewLog` is called only after a reused response parses/routes successfully or after a fresh completion parses successfully (`internal/metareview/service.go:138-174`; SQL at `internal/db/store.go:1704-1720`). Drydock logs JSON to stdout; no source writer for `meta-review-log.jsonl` exists. The corpus file is therefore a table export whose header-only state faithfully reflects `SELECT count(*) FROM meta_review_log = 0`.

The container log shows that meta-review **did trigger**, but every observed trigger failed:

- 150 captured `review published` messages.
- 50 `meta-review async run failed` messages and zero DB rows/successes.
- 17 requests exceeded context (2,143,696–12,326,163 tokens; median 9,544,334).
- 31 completions failed strict output parsing/schema validation: 24 encoded numeric `reasoning_quality` as a string, 3 returned an array for boolean `suggested_few_shot`, 2 returned a number for `prompt_gaps`, 1 omitted `prompt_gaps`, and 1 emitted an invalid JSON escape. A representative type failure is `drydock-container.log:26896`.
- 2 additional attempts hit the open circuit breaker.

The oversized requests have a direct code cause: `metaReviewUserPrompt` embeds **the full unbounded `in.PatchDiff`**, then the bundle and local-review JSON (`internal/metareview/service.go:334-347`). The primary patch layer's 40 KiB cap does not protect meta-review. Since parsing precedes insertion, all 50 failures correctly produced no `meta_review_log` rows, but the asynchronous API made the failure invisible to publication.

### Recommended corrective order

1. Require a provenance-checked base commit/ref for PR events; persist and publish base/tip/diff hashes; reject implausibly large file/byte deltas rather than silently truncating.
2. Replace repo-wide mutable checkouts with per-review Git worktrees and keep each worktree until every provider, scanner, autofix, and cleanup step finishes.
3. Make kind-1617 job scope explicit (root/set versus revision) and construct the prompt diff and publication target from the same scope.
4. Keep the current changed-file output filter, but bind it to the verified diff identity; add assertions that referenced files belong to both the computed set and expected target revision.
5. Cap/summarize meta-review diffs, validate/repair structured output before giving up, record failed attempts in a separate audit table, expose metrics/alerts, and use a detached service context with graceful shutdown rather than making quality-control failures publication-invisible.


## Investigation Log

## Root Cause

## Recommendations

## Preventive Measures
