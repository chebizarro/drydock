# Configuration Reference

All configuration is via environment variables, almost all with the `DRYDOCK_` prefix. This page lists every variable `cmd/drydock` reads, plus those of the optional sidecars. Start from a template:

```bash
cp .env.example .env               # development, local models
cp .env.production.example .env    # production
```

For the step-by-step setup see [Deploying Drydock](deployment.md); for model choice and serving see [Local Models](local-models.md). Drydock validates the whole configuration at startup, logs every error and warning, and exits if there is any error.

## Runtime

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_ENV` | string | *(empty)* | `production` (or `prod`) enables [production mode](#production-mode). Anything else, typically `development`, is development mode. |
| `DRYDOCK_PRODUCTION` | boolean | `false` | `true` also enables production mode, whatever `DRYDOCK_ENV` says. |
| `DEV_MODE` / `DRYDOCK_DEV_MODE` | boolean | `false` | Parsed but currently has no effect. Development behaviour comes from not being in production mode. |
| `DRYDOCK_MODE` | `listener` \\| `eval` \\| `drift-guard` | `listener` | Deployment mode. `scripts/entrypoint.sh` routes `eval` to `cmd/drydock-eval`; `listener` and `drift-guard` both exec `cmd/drydock`, which also reads `DRYDOCK_MODE` itself (`main.go`) and switches into the drift-guard subcommands (`export`/`flag`/`list`) when it is `drift-guard`. |
| `DRYDOCK_LOG_LEVEL` | `debug` \| `info` \| `warn` \| `error` | `info` | Structured JSON log level. `debug` is verbose and includes raw LLM responses. |

### Process environment (not `DRYDOCK_`-prefixed)

| Variable | Default | Description |
|----------|---------|-------------|
| `TIKTOKEN_CACHE_DIR` | `$TMPDIR/data-gym-cache` (Compose sets `/data/tiktoken-cache`) | Where the `cl100k_base` tokenizer file is cached after its first download. Startup fails without it in every mode, so put it on persistent storage and pre-seed it on air-gapped hosts ([Tokenizer cache](deployment.md#tokenizer-cache)). |
| `SSL_CERT_FILE` | system roots | PEM bundle Drydock trusts for HTTPS. It **replaces** the system roots, so a bundle for a private CA must also contain the public roots that relays and git hosts need ([Production TLS](deployment.md#production-tls)). |

### Production mode

With `DRYDOCK_ENV=production` (or `DRYDOCK_PRODUCTION=true`), startup fails unless:

- relays are set explicitly (`DRYDOCK_RELAYS`, or both `DRYDOCK_READ_RELAYS` and `DRYDOCK_WRITE_RELAYS`) and include none of the built-in public relays;
- `DRYDOCK_MONITORED_REPOS_AUTHOR` is a valid public key, and a signer is configured;
- the planner, coder32b, llm70b, coder14b and meta endpoints each have an explicit `*_BASE_URL`, `*_MODEL` and an API key (`DRYDOCK_LLM_API_KEY` or the per-role key). The built-in model defaults are empty in production;
- every configured LLM (including the optional security and agentic-discovery endpoints), Qdrant, embedding and version-registry URL uses `https://` and is not localhost or loopback;
- `DRYDOCK_QDRANT_URL`, `DRYDOCK_QDRANT_API_KEY`, `DRYDOCK_EMBED_BASE_URL`, `DRYDOCK_EMBED_MODEL` and `DRYDOCK_EMBED_API_KEY` are set.

An empty `DRYDOCK_TRUSTED_ZAPPERS` only warns. Development mode relaxes all of these; missing optional services become warnings.

## Database & Storage

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_DATABASE_URL` | string | `file:drydock.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)` | SQLite connection string. Includes pragma directives for foreign keys, write lock timeout, and WAL journaling. |
| `DRYDOCK_REPO_CACHE_DIR` | path | `repos` | Directory for cloned git repositories. |
| `DRYDOCK_REPO_CACHE_MAX_COUNT` | integer | `50` | Maximum number of cached repositories before LRU eviction. Set to `0` to disable count-based eviction. |
| `DRYDOCK_REPO_CACHE_MAX_SIZE_MB` | integer | `10240` | Maximum total cache size in MB (default 10 GB). Set to `0` to disable size-based eviction. |
| `DRYDOCK_PR_DIFF_MAX_FILES` | integer | `1000` | Pull requests whose computed diff touches more files are rejected rather than truncated. |
| `DRYDOCK_PR_DIFF_MAX_BYTES` | integer | `10485760` | Same, by diff size in bytes (10 MiB). |

### Review context token budget

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_CONTEXT_TOKEN_BUDGET` | integer | `64000` | Operator ceiling for deterministic review context, including discovery fallback. Must be between `1024` and `1048576`. Repository `.drydock.yaml` `context.token_budget` can only lower this ceiling. |
| `DRYDOCK_AGENTIC_PACKAGE_TOKEN_BUDGET` | integer | Context budget | Inherits `DRYDOCK_CONTEXT_TOKEN_BUDGET` when unset. An explicit value must be at least the context budget so the single-shot package gate can accept built bundles. |

The effective context prompt budget is the context budget minus
`DRYDOCK_AGENTIC_TOKEN_HEADROOM` (default `0.10`), rounded down. Builders count
rendered layer headings and separators too. This fixes the previous mismatch
with the package gate: default bundles now cap at about **57,600 tokens**, not
64,000. Headroom applies consistently to deterministic builders, including the
benchmark and discovery fallback paths. The setting applies wherever Drydock
builds context (listener, agentic discovery and its fallback, and the bench's
agentic mode); `drydock-eval` and `drydock-bench` single-shot mode use the context
supplied in their datasets.

Size the budget approximately as **served context minus system prompts and
completion headroom**; for a 32k server, start with
`DRYDOCK_CONTEXT_TOKEN_BUDGET=24000` (21,600 effective context tokens with default
headroom). With llama.cpp `-np N`, the per-slot context is `-c` divided by `N`,
not the full `-c` allocation. Leave room for other request content as well as the
model's answer.

## Repository Monitoring and Security Ceiling

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_MONITORED_REPOS_AUTHOR` | public key | *(empty)* | Optional operator **ceiling**, bootstrap only. When set, only repositories named in this identity's NIP-51 kind-30001 `drydock:monitored-repositories:v1` list can be monitored; until that list is seen, membership is empty (readiness is unaffected). It does not grant membership: repositories are enrolled by their own owners/maintainers (see below). A value set through `monitoring/operator` is persisted and overrides this on later starts. |
| `DRYDOCK_MONITORING_ADMINS` | comma-separated public keys | *(empty)* | Identities allowed to rotate, roll back, or read the operator ceiling through the authenticated ContextVM `monitoring/operator` method, in addition to the current operator. Without admins, the operator cannot clear itself (that would leave no principal). |
| `DRYDOCK_MONITORED_REPOS_AUTHOR_FORCE` | boolean | `false` | Audited local recovery: make `DRYDOCK_MONITORED_REPOS_AUTHOR` override the persisted control-plane operator at the next start. The prior value becomes the rollback target. |
| `DRYDOCK_REPO_ALLOWLIST` | comma-separated repository IDs | *(empty)* | Optional hard security ceiling, as `npub:identifier` or `64-character-hex-pubkey:identifier`. |
| `DRYDOCK_REPO_OWNER_ALLOWLIST` | comma-separated public keys | *(empty)* | Optional hard security ceiling by repository-announcement owner. Accepts npub or hex. |
| `DRYDOCK_REPO_RELAYS_MAX_PER_REPO` | integer 0–16 | `3` | How many relays from a monitored repository's kind-30617 `relays` tag Drydock will subscribe to, in declaration order. `0` disables per-repository relays. |
| `DRYDOCK_REPO_RELAYS_MAX_TOTAL` | integer 0–512 | `32` | Global cap on distinct customer relays across all monitored repositories. Relays shared by more repositories win; the rest are logged as truncated. |
| `DRYDOCK_REPO_RELAYS_ALLOW` | comma-separated host suffixes | *(empty)* | When set, only declared relays whose host matches a suffix (e.g. `grasp.example.org`) are admitted. |
| `DRYDOCK_REPO_RELAYS_DENY` | comma-separated host suffixes | *(empty)* | Declared relays whose host matches are refused. Deny wins over allow. |
| `DRYDOCK_MONITORING_ENROLL_PRICE_SATS` | integer | `0` | Price of a ContextVM `monitoring/enroll`. `0` makes enrollment free. When set, the request event must carry a payment the payment service accepts (Cashu token, zap, or active subscription); maintainers are not free. |
| `DRYDOCK_MONITORING_ENROLL_TTL` | duration | `0` | Lifetime applied to an enrollment that names no `expires_at`. `0` means no expiry; required to be `> 0` when a price is set, so paid enrollments (including those covered by an active review subscription) always expire. |
| `DRYDOCK_MONITORING_ENROLL_MAX_TTL` | duration | `8760h` | Upper bound on a requested `expires_at`. |

These settings have separate roles. **Automatic reactive** review of kinds 1617/1618/1619 is opted into by repository maintainers: a repository is monitored when its kind-30617 owner, or a pubkey in its `maintainers` tag, publishes a NIP-51 kind-30001 list (`d` = `drydock:monitored-repositories:v1`) that names the Drydock signer pubkey in a `p` tag and the repository in an `a` tag. Many authors may enroll; membership is the union. Authority is re-derived from the latest stored announcement, so dropping a maintainer from the announcement withdraws their enrollments. See [event-kinds.md](event-kinds.md) for the full contract. Because lists must name the signer, reactive review requires a configured signer.

The operator list (`DRYDOCK_MONITORED_REPOS_AUTHOR`) and the static repository/owner allowlists are ceilings. A target is inside the static ceiling when either its repository ID or announcement owner matches; empty static allowlists impose no additional ceiling. Ceilings apply to reactive work; the static ceiling also bounds on-demand orders. Paid `review/order` requests may target repositories outside the monitored set, but never outside the static ceiling.

Reactive processing is fail-closed: an unknown repository (no stored announcement), an author without authority, or a list that does not name this service grants nothing. Control-plane subscriptions are not bounded by the listener high-water mark, and winning revisions are restored from SQLite on restart.

### ContextVM enrollment

Besides publishing a NIP-51 list, a maintainer can enroll a single repository with an authenticated ContextVM request (`monitoring/enroll`, kind 25910) and revoke it with `monitoring/unenroll`. The same authority rule applies — the sender must be the owner of, or a maintainer in, the latest stored kind-30617 announcement — and the signed request event is kept as provenance together with the verified announcement revision, the payment decision, and the expiry. Enrollments are re-evaluated with every announcement change and stop granting at their expiry without a restart. They are subject to the operator and static ceilings like list entries; an enrollment outside a ceiling is accepted and stored but reported as `monitored: false`.

### Live operator ceiling

The operator ceiling is runtime state, not a restart-only setting. `monitoring/operator` (kind 25910; `action` = `set`, `rollback`, or `get`) is accepted from `DRYDOCK_MONITORING_ADMINS` or the current operator, with the same envelope rules as enrollment (service `p` tag, method tag, in-window `expiration`). `set` swaps the ceiling author: the new state is persisted first, then published to the registry, which re-derives membership in the same step and keeps it empty until the new operator's list has been received through the resubscription the change triggers; the prior state is retained and `rollback` restores it (also across restarts). Removing the ceiling widens scope and is refused unless `allow_scope_expansion` is true — the flag acknowledges ceiling *removal* only; replacing one operator by another is not gated because a different list cannot authorize strangers, it only selects among existing maintainer opt-ins. Every change is logged with the sender, request event id, and the monitored count before and after. The value is persisted in `runtime_settings` and wins over the environment on restart; a differing environment value is logged at startup and only takes effect with `DRYDOCK_MONITORED_REPOS_AUTHOR_FORCE=true`.

`/readyz` reports service health (`monitored_repositories` = persisted state recovered). Enrollment state — whether the ceiling list has arrived, how many repositories are monitored — is visible in the startup log and in the `monitoring/operator get` response, and is enforced fail-closed on membership rather than on readiness.

### Migration from the single trusted author

Earlier releases granted membership from the operator's list. Now the list is only a ceiling, so a repository in it is monitored only once its owner or a maintainer has opted in (list entry naming Drydock, or `monitoring/enroll`). Before upgrading a live deployment, run the inventory against a copy of the database:

```bash
DRYDOCK_MODE=monitoring-inventory DRYDOCK_INVENTORY_SERVICE_PUBKEY=<drydock-hex> drydock --json --strict
```

It reports, per repository in the operator list, whether it stays `monitored` or is `dropped` (`no_announcement`, `no_maintainer_optin`, or `maintainer_list_does_not_name_drydock`), and exits 2 with `--strict` when anything drops. It writes nothing. Rollback is a redeploy of the previous image: schema changes are additive and the operator list is untouched. No scope is ever expanded by the migration — a repository can only lose membership until a maintainer opts in.

### Per-repository relay routing

Repositories hosted on GRASP, Buzz, or any other NIP-34 forge usually keep their patches on their own relay, declared in the kind-30617 `relays` tag. For every monitored repository Drydock subscribes to those relays in addition to `DRYDOCK_READ_RELAYS`, with filters scoped to that repository: NIP-34 work items (kinds 1617/1618/1619) by repository address, and the repository's own announcement and state (30617/30618) by owner and identifier. Base relays additionally carry a no-`Since` announcement filter for every declared repository, so authority can be recovered on a fresh database.

Customer-declared state can never widen egress beyond the operator's policy: relays must be `wss://` and not loopback, private, or link-local hosts (both relaxed only when `DRYDOCK_ENV=development`), must pass `DRYDOCK_REPO_RELAYS_ALLOW`/`DENY`, and are capped per repository and globally. Shared relays are deduplicated. When membership, a list author, or an announcement changes, the listener resubscribes with the new plan from its live high-water mark — no restart, and no loss of persisted enrollment or review state. Rejected and truncated relays are logged with the reason.

## Betterleaks Secret Scanning

Repository policy enables secret scanning with `security.secret_scan: true` in the root `.drydock.yaml`. The same setting applies to patch/PR reviews and full-repository audits. The shipped runtime image includes betterleaks; custom deployments must install `betterleaks` on `PATH`. Once enabled, scanning fails closed if the binary is missing, the command fails, or its JSON report is malformed.

Scanner policy files are root-level fixed names; repository configuration cannot supply arbitrary paths:

1. `.betterleaks.toml`, falling back to `.gitleaks.toml`
2. `.betterleaks-baseline.json`, falling back to `.gitleaks-baseline.json`

Patch reviews read those files from the trusted **base commit**, not from the proposed patch. Consequently, a PR that introduces or changes scanner policy is evaluated under the previous base policy; the new policy becomes active after merge. Full audits read policy from the audited commit.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_BETTERLEAKS_VALIDATION` | boolean | `false` | Operator-only live credential validation for every repository with `security.secret_scan: true`. Invalid values fail closed to `false`. Enabling this may send detected credentials to third-party validation endpoints from the Drydock network; repositories cannot opt in themselves. |

Betterleaks always runs with redaction. Drydock discards secret and match fields from scanner output, emits only canonical safe finding text, and sanitizes sensitive fields again before Nostr and SARIF publication. Validated credentials map to `critical` severity at `0.99` confidence; all other validation states map to `high` at `0.90`.

## ContextVM Rate Limits

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_CODECHAT_RATE_LIMIT_REQUESTS` | integer | `20` | Maximum encrypted codebase-chat requests per sender and window. |
| `DRYDOCK_CODECHAT_RATE_LIMIT_WINDOW` | duration | `1h` | Codebase-chat rate-limit window. |
| `DRYDOCK_REVIEW_ORDER_RATE_LIMIT_REQUESTS` | integer | `20` | Maximum new generic `review/order` requests per requester and window. Idempotent receipt retries do not consume quota. |
| `DRYDOCK_REVIEW_ORDER_RATE_LIMIT_WINDOW` | duration | `1h` | Generic order rate-limit window. |
| `DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_REQUESTS` | integer | `100` | Maximum marketplace feedback notifications per sender and window. |
| `DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_WINDOW` | duration | `24h` | Marketplace feedback rate-limit window. |

All request counts must be at least 1 and all windows must be positive. Limits are backed by the configured rate-limit store; backend failures deny processing rather than silently allowing traffic.

## Payment Service

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_FREE_PUBKEYS` | comma-separated public keys | *(empty)* | Patch authors that bypass payment gating for every repository. Accepts npub or 64-character hex public keys and normalizes npub values to hex. |
| `DRYDOCK_TRUSTED_ZAPPERS` | comma-separated public keys | *(empty)* | Zapper (LNURL provider) public keys whose zap receipts are accepted as payment. Empty rejects all zap receipts (warning in production). |
| `DRYDOCK_NWC_CONNECTION_STRING` | string (secret) | *(empty)* | Nostr Wallet Connect URI used to authorize paid reviews. |
| `DRYDOCK_CASHU_TRUSTED_MINTS` | comma-separated URLs | *(empty)* | Cashu mints whose tokens are accepted. Falls back to `DRYDOCK_CASHU_MINT_URL`. Setting NWC without a trusted mint, or the reverse, logs a warning. |
| `DRYDOCK_CASHU_MINT_URL` | comma-separated URLs | *(empty)* | Legacy name for the trusted mint list, used when `DRYDOCK_CASHU_TRUSTED_MINTS` is empty. |

Repository-specific payment policy, free pubkeys, maintainer access, pricing, quota, and subscriptions are configured under `payments` in `.drydock.yaml`; see [Payments](payments.md) and [Per-Repository Configuration](repo-config.md).

## Nostr Relays

Drydock supports separate relay lists for reading (subscribing to events) and writing (publishing reviews). If read/write-specific lists are not set, `DRYDOCK_RELAYS` is used for both.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_RELAYS` | comma-separated URLs | `wss://relay.damus.io,wss://nos.lol,wss://relay.primal.net` | Fallback relay list used for both reading and writing when specific lists are not set. |
| `DRYDOCK_READ_RELAYS` | comma-separated URLs | *(empty — falls back to `DRYDOCK_RELAYS`)* | Relays to subscribe to for incoming events. |
| `DRYDOCK_WRITE_RELAYS` | comma-separated URLs | *(empty — falls back to `DRYDOCK_RELAYS`)* | Relays to publish review comments to. |

## Listener

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_LISTENER_LOOKBACK_MIN` | integer (minutes) | `5` | How far back to look when starting a fresh subscription with no persisted high-water-mark. |
| `DRYDOCK_LISTENER_HWM_OVERLAP` | duration | `30s` | Overlap subtracted from a valid persisted high-water mark when resubscribing. |
| `DRYDOCK_LISTENER_MAX_FUTURE_SKEW` | duration | `10m` | Maximum accepted future clock skew. Events beyond this bound cannot advance the listener cursor, and an already-future persisted cursor is ignored on startup. |
| `DRYDOCK_LISTENER_MAX_EVENT_AGE` | duration | `8760h` | Maximum accepted event age (one year by default). |

## Signing

Drydock signs review comments with its own Nostr identity. It uses the first signer configured, in this order:

1. **NIP-46 bunker** (recommended): the key stays in the signer.
2. **Local nsec**, from `DRYDOCK_SIGNER_NSEC_FILE` (preferred) or `DRYDOCK_SIGNER_NSEC`.

With no signer, production mode fails validation. Development mode starts listen-only: events are ingested, but the review pipeline is disabled and nothing is published. See [step 2 of the deployment guide](deployment.md#step-2-create-drydocks-nostr-identity-and-signer) for creating keys and running a bunker.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_SIGNER_BUNKER_URL` | string | *(empty)* | NIP-46 bunker URL (`bunker://<signer-pubkey>?relay=wss://...&secret=...`) or a NIP-05 bunker identifier. Used first when set. |
| `DRYDOCK_SIGNER_NSEC_FILE` | path | *(empty)* | File containing the nsec (bech32) or 64-character hex private key, for example a mounted Docker secret. Overrides `DRYDOCK_SIGNER_NSEC`. |
| `DRYDOCK_SIGNER_NSEC` | string (secret) | *(empty)* | The key itself. It's a plaintext secret in the environment, so prefer the file or a bunker. |

## LLM Endpoints

Drydock uses OpenAI-compatible `/chat/completions` endpoints. Five core model slots are configured independently. They can all point at the same server, as [Local Models](local-models.md) recommends for one GPU, or be spread across servers. The defaults below apply in development mode only; in production they are empty and must be set. The defaults name older models that benchmark poorly, so set every slot explicitly; see [Which models](local-models.md#which-models).

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_LLM_API_KEY` | string | *(empty)* | Shared API key for all LLM endpoints. Sent as `Authorization: Bearer <key>`. |
| `DRYDOCK_PLANNER_API_KEY`, `DRYDOCK_CODER32B_API_KEY`, `DRYDOCK_LLM70B_API_KEY`, `DRYDOCK_CODER14B_API_KEY` | string | *(falls back to `DRYDOCK_LLM_API_KEY`)* | Per-role keys, when roles use different servers. |
| `DRYDOCK_PLANNER_BASE_URL` | URL | `http://127.0.0.1:11434/v1` | Base URL for the planner model (lightweight, selects review route). |
| `DRYDOCK_PLANNER_MODEL` | string | `qwen2.5-coder-14b-instruct-q4_k_m` | Model name for planner requests. |
| `DRYDOCK_CODER32B_BASE_URL` | URL | `http://127.0.0.1:11434/v1` | Base URL for the 32B coder model (complex code, multi-file). |
| `DRYDOCK_CODER32B_MODEL` | string | `qwen2.5-coder-32b-instruct-q4_k_m` | Model name for coder32b requests. |
| `DRYDOCK_LLM70B_BASE_URL` | URL | `http://127.0.0.1:11435/v1` | Base URL for the 70B model (architecture, security). |
| `DRYDOCK_LLM70B_MODEL` | string | `llama-3.3-70b-instruct-q4_k_m` | Model name for llm70b requests. |
| `DRYDOCK_CODER14B_BASE_URL` | URL | `http://127.0.0.1:11434/v1` | Base URL for the 14B coder model (simple patches, style). |
| `DRYDOCK_CODER14B_MODEL` | string | `qwen2.5-coder-14b-instruct-q4_k_m` | Model name for coder14b requests. |
| `DRYDOCK_META_BASE_URL` | URL | `http://127.0.0.1:11436/v1` | OpenAI-compatible base URL dedicated to meta-review. |
| `DRYDOCK_META_MODEL` | string | `llama-3.3-70b-instruct-q4_k_m` | Model name for meta-review requests. The development default is a local quantized Llama model, **not** a frontier model. |
| `DRYDOCK_META_API_KEY` | string | *(falls back to `DRYDOCK_LLM_API_KEY`)* | Dedicated bearer token for the meta-review provider. |
| `DRYDOCK_META_MAX_INPUT_BYTES` | integer | `131072` | Combined byte budget for patch diff and context bundle in a meta-review prompt. Oversized sections are deterministically truncated with omission metadata. |

For production-quality meta-review, explicitly point the dedicated slot at an
OpenAI-compatible frontier provider rather than relying on the local
development default:

```bash
DRYDOCK_META_BASE_URL=https://frontier-provider.example/v1
DRYDOCK_META_MODEL=provider-frontier-model
DRYDOCK_META_API_KEY=${FRONTIER_PROVIDER_API_KEY}
DRYDOCK_META_MAX_INPUT_BYTES=131072
```

Use the exact model identifier served by your provider and size the input
budget below that model's context limit, leaving room for the local-review
JSON, system prompt, and completion.

### Security and agentic-discovery endpoints (optional)

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_SEC70B_BASE_URL`, `DRYDOCK_SEC70B_MODEL` | URL, string | *(empty; the sec70b route then uses the llm70b endpoint)* | Deep security reviewer route ([Security Review](security-review.md)). |
| `DRYDOCK_SECCLASSIFY_BASE_URL`, `DRYDOCK_SECCLASSIFY_MODEL` | URL, string | *(empty)* | CWE classification and severity calibration route. |
| `DRYDOCK_SECLOCALIZE_BASE_URL`, `DRYDOCK_SECLOCALIZE_MODEL` | URL, string | *(empty)* | Vulnerability localization route (audits). |
| `DRYDOCK_AGENTIC_DISCOVERY_BASE_URL`, `DRYDOCK_AGENTIC_DISCOVERY_MODEL`, `DRYDOCK_AGENTIC_DISCOVERY_API_KEY` | URL, string, string | *(the coder32b endpoint, model and key)* | Model for the agentic context-discovery loop. |

The security endpoints use `DRYDOCK_LLM_API_KEY`. In production every configured endpoint must be HTTPS and not loopback.

### Completion token cap

A cap on completion tokens, sent as `max_tokens` on every `/chat/completions`
request (single-shot and agentic turns). It bounds runaway generations such as
a thinking model stuck in a degenerate reasoning loop. Without it, that loop
runs until the request times out or the context fills.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_LLM_MAX_TOKENS` | integer | `0` (no cap) | Global completion-token cap. `0` sends no `max_tokens`. |
| `DRYDOCK_PLANNER_MAX_TOKENS` | integer | *(inherits global)* | Planner and walkthrough calls. The conversation and code-chat handlers also use the planner endpoint. |
| `DRYDOCK_CODER32B_MAX_TOKENS` | integer | *(inherits global)* | coder32b reviewer route. |
| `DRYDOCK_LLM70B_MAX_TOKENS` | integer | *(inherits global)* | llm70b reviewer route. |
| `DRYDOCK_CODER14B_MAX_TOKENS` | integer | *(inherits global)* | coder14b reviewer route. |
| `DRYDOCK_SEC70B_MAX_TOKENS` | integer | *(inherits global)* | sec70b route and the security verifier. |
| `DRYDOCK_SECCLASSIFY_MAX_TOKENS` | integer | *(inherits global)* | secclassify route and the security classifier. |
| `DRYDOCK_SECLOCALIZE_MAX_TOKENS` | integer | *(inherits global)* | seclocalize route and the audit localizer. |
| `DRYDOCK_META_MAX_TOKENS` | integer | *(inherits global)* | Meta-review and prompt refinement. |
| `DRYDOCK_AGENTIC_DISCOVERY_MAX_TOKENS` | integer | *(inherits `DRYDOCK_CODER32B_MAX_TOKENS`)* | Each agentic discovery turn. Its fallback matches the discovery base URL and model. Agentic reviewer turns use the cap of the reviewer route they run on. |

Each value must be a non-negative integer; anything else fails startup
validation. A per-role `0` disables the cap for that role only.

When a structured call (planner, reviewer, walkthrough, meta-review) stops
with `finish_reason=length`, Drydock treats the output as invalid even if a
fragment parses. It then goes through the normal JSON repair attempts. In an
agentic loop a truncated turn is an ordinary turn: a cut-off tool call
fails argument validation, and the error goes back to the model.

**Why the default is off.** vLLM and TGI reject any request whose prompt
tokens plus `max_tokens` exceed the served context window, with a
non-retryable HTTP 400. A global default large enough for thinking models
would therefore fail every review on a 32k-context deployment. A default small
enough to avoid that would truncate legitimate thinking-mode reviews. Ollama
and llama.cpp accept any cap. Choose the cap per deployment, using these
measurements from the local-model benchmark:

| Reviewer mode | Observed output per review | Suggested cap |
|---------------|----------------------------|---------------|
| Thinking off | ~1.2k–3.7k tokens | `8192` |
| Thinking on | up to ~19k tokens | `24576` |

Keep prompt tokens plus the cap within each endpoint's context window. Also
remember that each LLM call has a [per-call timeout](#llm-call-timeout), so a
cap your hardware cannot generate within that time still ends in a timeout.

### LLM call timeout

A deadline for one LLM call: one `/chat/completions` request, from sending
the prompt to reading the last byte of the response. It covers single-shot
calls, JSON repair calls and each agentic turn separately. It is not a
deadline for a whole review.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_LLM_TIMEOUT` | Go duration | `180s` | Global per-call timeout. |
| `DRYDOCK_PLANNER_TIMEOUT` | Go duration | *(inherits global)* | Planner and walkthrough calls. The conversation and code-chat handlers also use the planner endpoint. |
| `DRYDOCK_CODER32B_TIMEOUT` | Go duration | *(inherits global)* | coder32b reviewer route. |
| `DRYDOCK_LLM70B_TIMEOUT` | Go duration | *(inherits global)* | llm70b reviewer route. |
| `DRYDOCK_CODER14B_TIMEOUT` | Go duration | *(inherits global)* | coder14b reviewer route. |
| `DRYDOCK_SEC70B_TIMEOUT` | Go duration | *(inherits global)* | sec70b route and the security verifier. |
| `DRYDOCK_SECCLASSIFY_TIMEOUT` | Go duration | *(inherits global)* | secclassify route and the security classifier. |
| `DRYDOCK_SECLOCALIZE_TIMEOUT` | Go duration | *(inherits global)* | seclocalize route and the audit localizer. |
| `DRYDOCK_META_TIMEOUT` | Go duration | *(inherits global)* | Meta-review and prompt refinement. |
| `DRYDOCK_AGENTIC_DISCOVERY_TIMEOUT` | Go duration | *(inherits `DRYDOCK_CODER32B_TIMEOUT`)* | Each agentic discovery turn. Its fallback matches the discovery base URL and model. Agentic reviewer turns use the timeout of the reviewer route they run on. |

Values use Go duration syntax (`90s`, `4m`, `2m30s`). A bare number such as
`180` has no unit and is rejected. Each value must be greater than zero and at
most `30m`; anything else fails startup validation.

**Sizing.** The default was 120 seconds until local GPUs proved it too short.
On 2× Tesla P40 a reviewer call prefills a 5–15k-token bundle and decodes
~1–4k tokens at roughly 10–30 tok/s, so a normal review can run 2–3 minutes.
A thinking-mode review can produce up to ~19k tokens and needs more time. To
size a role's timeout, divide its completion-token cap by your decode speed
and add the prefill time. For example, 24576 tokens at 20 tok/s is about
20 minutes. Use `drydock-bench` to measure this; its `calls_over_prod_timeout`
counts calls slower than the configured timeout for their role.

**Retries and surrounding deadlines.**

- A timed-out call is **not retried**. Drydock's retry wrapper retries only
  HTTP 429, 5xx and connection errors. A call that ran out of time on slow
  hardware would likely run out again, and retrying would multiply the wait.
- Each retry of a transient failure gets its own full timeout. The circuit
  breaker counts a timeout as one failure.
- The per-call deadline is derived from the caller's context, so it can only
  shorten a review's deadline, never extend it. Retries stop as soon as that
  context ends, even during backoff. A review bounded by
  `DRYDOCK_IDE_AGENTIC_TIMEOUT` (`10m` by default) therefore ends at that
  deadline whatever the per-call timeout and retry count are.
- Startup validation warns when a timeout is not shorter than
  `DRYDOCK_IDE_AGENTIC_TIMEOUT`, because one slow call could then use the
  whole IDE review budget.
- Pipeline reviews from Nostr events have no review-level deadline. Their
  worst case is per-call timeout × attempts (3 for review and meta-review
  calls, 2 for conversation replies) plus backoff, for each LLM call the
  review makes.

### Model name verification

Configured model names are deployment metadata and can go stale. At startup
Drydock probes each endpoint's `/v1/models` listing and logs a warning when a
configured `*_MODEL` value is not among the served models. Published reviews
are labeled with the model identifier the endpoint **actually reported
serving** for that request (from the chat-completion response), falling back
to the served-model registry and then the configured name — so a stale env
value produces warnings, not mislabeled reviews.

### Single-Endpoint Pattern

For one GPU, point every slot at the same server and model, for example one llama-server serving `qwen3.8-27b` ([Local Models](local-models.md#serving-with-llamacpp-recommended)):

```bash
DRYDOCK_PLANNER_BASE_URL=http://127.0.0.1:8080/v1
DRYDOCK_CODER32B_BASE_URL=http://127.0.0.1:8080/v1
DRYDOCK_LLM70B_BASE_URL=http://127.0.0.1:8080/v1
DRYDOCK_CODER14B_BASE_URL=http://127.0.0.1:8080/v1
DRYDOCK_META_BASE_URL=http://127.0.0.1:8080/v1
# and DRYDOCK_{PLANNER,CODER32B,LLM70B,CODER14B,META}_MODEL=qwen3.8-27b
```

This serializes LLM calls through one server; see [Splitting roles across servers](local-models.md#splitting-roles-across-servers) and [Scaling](scaling.md).

## Agentic Review

The default review path is agentic ([Agentic Review Operations](agentic-review.md)). On modest local GPUs, start with `DRYDOCK_AGENTIC_REVIEW_FALLBACK=true`; see [Agentic or single-shot review](local-models.md#agentic-or-single-shot-review).

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_AGENTIC_REVIEW_FALLBACK` | boolean | `false` | `true` uses the deterministic context builder plus a single-shot review (about 3 LLM calls per review) instead of the agentic loops. |
| `DRYDOCK_AGENTIC_DISCOVERY_MAX_TURNS` | integer | `24` | Discovery turn limit. |
| `DRYDOCK_AGENTIC_DISCOVERY_MAX_TOOL_CALLS` | integer | `96` | Discovery tool-call limit. |
| `DRYDOCK_AGENTIC_DISCOVERY_MAX_CUMULATIVE_TOKENS` | integer | `256000` | Discovery cumulative-token limit. It re-counts the whole prompt each turn. |
| `DRYDOCK_AGENTIC_REVIEWER_MAX_TURNS` | integer | `20` | Reviewer turn limit. |
| `DRYDOCK_AGENTIC_REVIEWER_MAX_TOOL_CALLS` | integer | `96` | Reviewer tool-call limit. |
| `DRYDOCK_AGENTIC_REVIEWER_MAX_CUMULATIVE_TOKENS` | integer | `384000` | Reviewer cumulative-token limit. |
| `DRYDOCK_AGENTIC_MAX_TOOL_RESULT_BYTES` | integer | `16384` | Per-tool result size ceiling. |
| `DRYDOCK_AGENTIC_MAX_MODEL_CONTEXT` | integer | `256000` | A turn whose serialized request exceeds this is rejected before sending. Set it to your server's real context size. |
| `DRYDOCK_AGENTIC_PACKAGE_TOKEN_BUDGET` | integer | *(inherits `DRYDOCK_CONTEXT_TOKEN_BUDGET`)* | Token budget of the finalized context package (also gates the single-shot bundle). See [Review context token budget](#review-context-token-budget). |
| `DRYDOCK_AGENTIC_TOKEN_HEADROOM` | float | `0.10` | Fraction of the package budget reserved for tokenizer/model skew (0 ≤ x < 1). |
| `DRYDOCK_AGENTIC_HISTORY_TOKEN_BUDGET` | integer | `64000` | Compacted conversation-history budget for continued sessions. |
| `DRYDOCK_IDE_AGENTIC_TIMEOUT` | duration | `10m` | End-to-end deadline for an IDE review or continuation. |
| `DRYDOCK_IDE_WORKSPACE_BINDINGS` | list | *(empty)* | Comma-separated `lowercase-hex-pubkey=/absolute/workspace` bindings for inline IDE reviews; repeat a pubkey for several roots. Empty disables inline filesystem review. |
| `DRYDOCK_REVIEW_SNAPSHOT_STORAGE_PATH` | path | `<DRYDOCK_REPO_CACHE_DIR>/review-snapshots` | Frozen review snapshots. |
| `DRYDOCK_REVIEW_SNAPSHOT_TTL` | duration | `24h` | Lifetime of an unleased snapshot. |
| `DRYDOCK_REVIEW_SNAPSHOT_LEASE_TTL` | duration | `24h` | Default lease lifetime; must be at least the session lifetime. |
| `DRYDOCK_REVIEW_SESSION_LIFETIME` | duration | `24h` | Review-session lifetime. |
| `DRYDOCK_REVIEW_SNAPSHOT_GC_INTERVAL` | duration | `15m` | Session-expiry and snapshot garbage-collection cadence. |

All integer limits must be positive.

### MCP HTTP (optional)

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_MCP_HTTP_ENABLED` | boolean | `false` | Serve the read-only MCP tool surface over authenticated Streamable HTTP. |
| `DRYDOCK_MCP_HTTP_ADDR` | `host:port` | `127.0.0.1:8090` | Listen address. |
| `DRYDOCK_MCP_HTTP_BEARER_TOKEN` | string (secret) | *(empty)* | Required when enabled. |
| `DRYDOCK_MCP_HTTP_SESSION_ID` | string | *(empty)* | Required when enabled: the 32-character ID of a server-created review session. |
| `DRYDOCK_MCP_MAX_REQUEST_BODY_BYTES` | integer | `4194304` | Request-body ceiling. |
| `DRYDOCK_MCP_SHUTDOWN_TIMEOUT` | duration | `30s` | Graceful shutdown deadline. |

Don't expose the MCP listener publicly without TLS termination.

## Security Review

See [Security Review](security-review.md) for what these enable.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_SECURITY_ENABLED` | boolean | `false` | Parsed but not currently consulted. The security stage is enabled per repository through the `security:` section of `.drydock.yaml` ([Security Review §10.1](security-review.md#101-drydockyaml--new-security-section)). |
| `DRYDOCK_SECURITY_AUDIT_WORKERS` | integer | `2` | Concurrent full-repository audits. |
| `DRYDOCK_SECURITY_NOSTR_ENABLED` | `auto` \| `true` \| `false` | `auto` | Operator switch for the Nostr security lens. `false` disables it everywhere. `auto` and `true` leave it to each repository's `.drydock.yaml` Nostr setting. Invalid values fail closed to `false`. |
| `DRYDOCK_SECURITY_NOSTR_PROBE_TARGETS` | comma-separated | *(empty)* | Targets for the Nostr protocol probe. |
| `DRYDOCK_SECURITY_NOSTR_PROBE_ACTIVE` | boolean | `false` | Allow active probing of those targets (sends traffic to them). |

## Qdrant Vector Store (Optional)

Qdrant provides vector similarity search for NIP spec retrieval, project documentation, and few-shot review examples. All Qdrant features are disabled when `DRYDOCK_QDRANT_URL` is empty.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_QDRANT_URL` | URL | *(empty)* | Qdrant REST API endpoint (e.g., `http://qdrant:6333`). Empty disables all RAG features. |
| `DRYDOCK_QDRANT_API_KEY` | string | *(empty)* | API key for Qdrant authentication. Optional in development, required in production. |
| `DRYDOCK_QDRANT_COLLECTION_NIP_SPECS`, `_PROJECT_DOCS`, `_FEW_SHOT`, `_CODE_CHUNKS` | string | `nip_specs`, `project_docs`, `few_shot_reviews`, `code_chunks` | Collection names (letters, digits, `_`, `-`). Override them to share one Qdrant between deployments. |
| `DRYDOCK_QDRANT_RESULTS_PER_COLLECTION` | integer | `3` | Retrieved results per collection. |

Drydock auto-creates four collections on startup:
- `nip_specs` — Nostr Improvement Proposal documentation chunks
- `project_docs` — Per-project documentation embeddings
- `few_shot_reviews` — Positive/negative review examples for prompt enrichment
- `code_chunks` — Tree-sitter code chunks used by semantic code retrieval and codebase chat

## Embedding Model (Optional)

Required when Qdrant is enabled. Any OpenAI-compatible `/embeddings` endpoint works.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_EMBED_BASE_URL` | URL | *(empty)* | Embedding server base URL (e.g., `http://host.docker.internal:11434/v1` for Ollama). |
| `DRYDOCK_EMBED_MODEL` | string | `nomic-embed-text` | Model name for embedding requests. |
| `DRYDOCK_EMBED_API_KEY` | string | *(empty)* | API key for the embedding endpoint. |
| `DRYDOCK_EMBED_DIMENSION` | integer | `768` | Embedding vector dimension; must match the configured model and existing Qdrant collections. |

Qdrant and embeddings must be configured together; setting only one disables retrieval with a warning.

### Chartroom retrieval (optional)

An alternative retrieval backend appended to the context bundle ([Context Builder](context-builder.md)).

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_CHARTROOM_URL` | URL | *(empty)* | Chartroom service endpoint. Empty disables it. |
| `DRYDOCK_CHARTROOM_TOKEN` | string (secret) | *(falls back to `CHARTROOM_HTTP_BEARER_TOKEN`)* | Bearer token. A URL without a token warns. |
| `DRYDOCK_CHARTROOM_CORPUS_IDS` | comma-separated | *(empty)* | Corpora to query. |
| `DRYDOCK_CHARTROOM_SOURCE_IDS` | comma-separated | *(falls back to `DRYDOCK_CHARTROOM_SOURCES`)* | Sources to query. |

## LSP Bridge (Optional)

The LSP bridge is a separate HTTP service that manages language servers for type-aware symbol analysis. Activate via the `lsp` Docker profile.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_LSP_BRIDGE_URL` | URL | *(empty)* | LSP bridge endpoint (e.g., `http://lsp-bridge:8082`). Empty disables LSP-enhanced analysis; drydock falls back to tree-sitter + ripgrep. |
| `DRYDOCK_LSP_BRIDGE_TOKEN` | string (secret) | *(falls back to `LSP_BRIDGE_AUTH_TOKEN`)* | Bearer token Drydock sends to the bridge. |

The bridge itself (`cmd/lsp-bridge`) reads:

| Variable | Default | Description |
|----------|---------|-------------|
| `LSP_BRIDGE_ADDR` | `127.0.0.1:8082` | Listen address (the Compose image listens on all interfaces inside the container network). |
| `LSP_BRIDGE_AUTH_TOKEN` / `LSP_BRIDGE_AUTH_TOKENS` | *(required)* | Accepted token(s), comma-separated. Also read from `DRYDOCK_LSP_BRIDGE_TOKEN(S)`. The bridge refuses to start without one unless `LSP_BRIDGE_DEV=true`. |
| `LSP_BRIDGE_DEV` | `false` | Allow unauthenticated development mode. |
| `DRYDOCK_LSP_ALLOWED_ROOTS` | the working directory | Comma-separated directories the bridge may analyze (the repository cache). |

## Dependency Upgrades (Optional)

Operator-disabled by default; per-repository opt-in lives in `.drydock.yaml` under `upgrades`.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_DEPUPGRADE_ENABLED` | boolean | `false` | Enable the dependency-upgrade service. It then requires `DRYDOCK_DEP_RUNNER_URL` and `DRYDOCK_MONITORED_REPOS_AUTHOR`. |
| `DRYDOCK_DEPUPGRADE_INTERVAL` | duration | `24h` | Periodic sweep and reconciliation cadence. |
| `DRYDOCK_DEPUPGRADE_WORKERS` | integer | `1` | Concurrent upgrade jobs. |
| `DRYDOCK_DEP_RUNNER_URL` | URL | *(empty)* | The dep-runner sidecar (`http://dep-runner:8083` in Compose). |
| `DRYDOCK_DEP_RUNNER_TOKEN` | string (secret) | *(falls back to `DEP_RUNNER_AUTH_TOKEN`)* | Bearer token Drydock sends to the runner. |
| `DRYDOCK_GO_REGISTRY_URL` | URL | `https://proxy.golang.org` | Version source for Go modules. HTTP(S), without credentials, query or fragment; HTTPS in production. |
| `DRYDOCK_NPM_REGISTRY_URL` | URL | `https://registry.npmjs.org` | Version source for npm. |
| `DRYDOCK_CARGO_REGISTRY_URL` | URL | `https://crates.io` | Version source for Cargo. |
| `DRYDOCK_PYPI_REGISTRY_URL` | URL | `https://pypi.org` | Version source for PyPI. |

The runner (`cmd/dep-runner`) executes package managers against untrusted manifests inside a locked-down container, and reads:

| Variable | Default | Description |
|----------|---------|-------------|
| `DEP_RUNNER_ADDR` | `0.0.0.0:8083` | Listen address. |
| `DEP_RUNNER_AUTH_TOKEN` / `DEP_RUNNER_AUTH_TOKENS` | *(required)* | Accepted token(s). The runner refuses to start without one unless `DEP_RUNNER_DEV=true`. |
| `DEP_RUNNER_DEV` | `false` | Allow unauthenticated development mode. |
| `DEP_RUNNER_ALLOW_SCRIPTS` | `false` | Allow package lifecycle scripts (npm postinstall, build.rs, setup.py). They run attacker-controlled code; enable only in a trusted, isolated deployment. |
| `DEP_RUNNER_GOPROXY` (falls back to `GOPROXY`), `DEP_RUNNER_GOSUMDB`, `DEP_RUNNER_NPM_REGISTRY`, `DEP_RUNNER_CARGO_REGISTRY`, `DEP_RUNNER_PIP_INDEX_URL` | tool defaults (Compose pins the public registries) | Registries the package managers use. Restrict egress to exactly these hosts. |
| `DEP_RUNNER_TIMEOUT` | Compose: `4m` | Per-job timeout. |
| `DEP_RUNNER_MAX_MANIFEST_BYTES` | built-in limit | Maximum manifest size accepted. |
| `DEP_RUNNER_WORK_ROOT` | system temp | Per-request work directory root (`/work` tmpfs in Compose). |
| `DEP_RUNNER_TOOLCHAIN_PATH` | `PATH` | Where the go/npm/cargo/pip toolchains live. |

Compose also reads `DEP_RUNNER_WORK_TMPFS`, `DEP_RUNNER_MEM_LIMIT` and `DEP_RUNNER_CPUS` (defaults `2g`, `2g`, `2.0`) to size the container.

## Pipeline

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_PIPELINE_WORKERS` | integer | `2` | Number of concurrent review pipeline workers. Increase only if your LLM endpoints support concurrent requests. |
| `DRYDOCK_APPLY_FAILURE_PUBLICATION` | `notice` or `suppress` | `notice` | Publish a clearly tagged operational notice when fetch/apply preparation fails, or suppress it entirely. Failure notices are never recorded as reviews. |

Apply/fetch and meta-review quality signals are exposed on `/metrics`. Alert on
`drydock_review_apply_failure_ratio`, `drydock_review_model_none_publication_ratio`,
and `drydock_meta_review_failure_ratio`; the corresponding counters remain
available for windowed PromQL `rate()` expressions.

## Kind 0 Profile & Media

At startup Drydock checks the read relays for a kind 0 profile belonging to
its signing identity and publishes one when missing — or republishes when the
configured metadata below has changed. Fields Drydock does not manage (e.g. a
`lud16` set by another tool) are preserved across republishes.

The icon and banner images are pushed to a Blossom media server (BUD-01/
BUD-02) and referenced by their content-addressed URLs. Explicit `*_URL`
values skip the upload; with no Blossom server configured the image fields
are omitted with a warning.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_PROFILE_ENABLED` | boolean | `true` | Publish/refresh the kind 0 profile at startup. |
| `DRYDOCK_PROFILE_NAME` | string | `Drydock` | Profile display name. |
| `DRYDOCK_PROFILE_ABOUT` | string | *(summary of what Drydock does)* | Profile about text. |
| `DRYDOCK_PROFILE_WEBSITE` | URL | *(empty)* | Profile website field. |
| `DRYDOCK_PROFILE_PICTURE_URL` | URL | `https://blossom.sharegap.net/7abc2455…78c8` | Profile picture URL. Set to override; takes precedence over any icon upload. |
| `DRYDOCK_PROFILE_BANNER_URL` | URL | `https://blossom.sharegap.net/775f7d11…51c1e` | Profile banner URL. Set to override; takes precedence over any banner upload. |
| `DRYDOCK_PROFILE_ICON_PATH` | path | *(empty)* | Optional local icon pushed to Blossom for the `picture` field when no picture URL is set. |
| `DRYDOCK_PROFILE_BANNER_PATH` | path | *(empty)* | Optional local banner pushed to Blossom for the `banner` field when no banner URL is set. |
| `DRYDOCK_BLOSSOM_SERVERS` | comma-separated URLs | *(empty)* | Blossom media servers tried in order for image uploads. |

## Health & Monitoring

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_HEALTH_ADDR` | `host:port` | `127.0.0.1:8081` | Listen address for the health, metrics, dashboard, and management HTTP server. The image and Compose set `:8081` inside the container; Compose publishes it on host loopback (`DRYDOCK_HEALTH_HOST_PORT`, default `8081`). |
| `DRYDOCK_DASHBOARD_BEARER_TOKEN` | string (secret) | *(empty)* | Protects `/dashboard/` and `/api/*`. Empty leaves them unauthenticated, so keep the listener on loopback. `/healthz`, `/readyz` and `/metrics` are always open. |

Endpoints:
- `GET /healthz` — Always returns `200 OK`. Use as a liveness probe.
- `GET /readyz` — Returns `200 OK` when the service is started, the database is reachable, and (when `DRYDOCK_MONITORED_REPOS_AUTHOR` is configured) an authoritative monitored-list state has loaded. Returns `503` otherwise. A valid empty or deleted list counts as authoritative.

## Evaluation

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_EVAL_DATASET_PATH` | path | `eval/heldout-sample.json` | Path to the JSON evaluation dataset. |
