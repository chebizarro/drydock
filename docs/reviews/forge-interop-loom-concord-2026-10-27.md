# Forge interoperability: Loom CI and Concord

Date: 2026-10-27. Scope: additive, default-off integrations. This is local
implementation/test evidence, **not** hosted CI, worker execution, relay delivery,
or Armada/Budabit client acceptance evidence.

## Protocol sources and verified vocabulary

### Loom and Budabit/Hive CI

Pinned Loom worker: [`fd38451c3bdaaad4b543195418a7b3d7feff184f`](https://github.com/Pleb5/loom-worker/tree/fd38451c3bdaaad4b543195418a7b3d7feff184f).
Its [constants](https://github.com/Pleb5/loom-worker/blob/fd38451c3bdaaad4b543195418a7b3d7feff184f/src/config/constants.ts)
assign job request **5100**, job result **5101**, cancellation **5102**,
parameterized progress **30100**, and worker advertisement **10100**.
[`publishJobResult`](https://github.com/Pleb5/loom-worker/blob/fd38451c3bdaaad4b543195418a7b3d7feff184f/src/nostr/service.ts#L510-L550)
signs with the worker key and emits `e=<job event id>`, `p=<requester>`,
`success`, `exit_code`, `duration`, `stdout`, and `stderr`. It does **not**
provide a repository, commit, or patch pointer, nor a separate CI-attestation kind.
A successful compute job is not itself evidence of a successful workflow.

Budabit's [pipeline architecture](https://github.com/Pleb5/budabit-pipelines-extension/blob/3cbd0942237abca336d5fc1e5cd5fd71259278cd/README.md)
provides the CI layer. The [watcher's event constructors](https://github.com/Pleb5/hive-ci-watcher/blob/0b97fea5fc039fb79386174c3f3dec9b7251ac6c/src/dispatch/events.ts)
make a **5401** workflow run containing `a=<repository coordinate>`, `commit`,
`publisher=<per-run ephemeral pubkey>`, `workflow`, and trigger metadata.
The author also signs a **5100** job with `e=<5401 id>` and `p=<worker>`.
The [runner script](https://github.com/Pleb5/flotilla-budabit/blob/4c85781b77570b18d42398017b360ea7c2fd704d/packages/budabit-pipelines-extension/packages/iframe-app/src/lib/runner-script.ts#L138-L171)
publishes **5402**, with `e=<5401 id>`, `status=success|failed`, `exit_code`,
`duration`, and optional `log_url`, using the per-run publisher credential.
That signer is **not the worker identity**.

The [actual layered subscriptions](https://github.com/Pleb5/budabit-pipelines-extension/blob/3cbd0942237abca336d5fc1e5cd5fd71259278cd/packages/iframe-app/src/lib/nostr.ts#L52-L159)
confirm that results need exact lineage filters, not invented result `a` tags.
The requested article, **“BudaBit: CI/CD Without Capture”**, was not located by
web searches or the accessible [Budabit site](https://budabit.club/). Its contents
were not assumed. The implementation relies on the pinned primary code above.
This is the older Hive CI/Loom vocabulary, not a claim of supporting every
newer Nostr CI protocol or release-artifact co-signing scheme.

### Concord

Pinned specification: [`f84d55523593688c70c1b1a1b765992103157f1b`](https://github.com/concord-protocol/concord/tree/f84d55523593688c70c1b1a1b765992103157f1b).
It is evolving; later spec changes require a fresh review.

- [CORD-01, Stream Event / Encoding](https://github.com/concord-protocol/concord/blob/f84d55523593688c70c1b1a1b765992103157f1b/01.md#stream-event):
  **1059** wrap signed by the shared stream key; ephemeral `p`; encryption under
  NIP-44 self-ECDH of the stream key, **not** the `p` key. An encrypted **20013**
  seal signed by the real author contains the unsigned rumor. Both layers use
  that conversation key. Timestamps are not randomized. This differs from the
  existing ordinary NIP-59 ContextVM path, which is left unchanged.
- [CORD-02 §4–5 and Appendix A.2](https://github.com/concord-protocol/concord/blob/f84d55523593688c70c1b1a1b765992103157f1b/02.md):
  the community ID is not a posting pubkey; each plane has a derived stream
  address. Key possession enables publishing/decryption. Control is separate;
  Guestbook is not a message inbox. Rumors carry millisecond remainder `ms`.
- [CORD-03 §3](https://github.com/concord-protocol/concord/blob/f84d55523593688c70c1b1a1b765992103157f1b/03.md#3-messages)
  and [message example §2.1](https://github.com/concord-protocol/concord/blob/f84d55523593688c70c1b1a1b765992103157f1b/examples.md#21-kind-9--message):
  messages are kind **9**, binding `channel` and decimal `epoch` inside the
  author-signed rumor. Receivers must check those against the opening key.

There is **no address-only non-member posting mechanism** in this subset.
The user explicitly approved operator-provisioned, pre-derived channel stream
credentials instead of inventing one. Drydock becomes a **key-holding channel
participant**, able to decrypt messages at that stream/epoch, including retained
history encrypted with that key. It receives neither other channel keys nor
Control Plane authority. This integration does not subscribe to or read chats,
but possession still creates that capability.

## Implementation and boundaries

### CI context

- `DRYDOCK_LOOM_ENABLED=false` by default. Primary filters are only monitored
  repositories' `#a` workflow anchors. Exact per-run `#e` + author filters follow
  the declared publisher and same-author job; worker result filters additionally
  bind the authenticated job's `p`. No kinds-only discovery was added.
- All filters travel through `relayroute.Routes` and the existing listener
  equality/debounce/resubscribe path on base and admitted repository relays.
  A merged route-change signal includes lineage changes and expiry. The expiry
  timer is resource cleanup, never assumed completion or message polling.
- SQLite migration **19** adds bounded active `loom_runs` lineage and indexed
  `ci_attestations`. Both migration-count pins moved from 18 to 19. Active runs
  default to 64/repo, 1024/global, TTL 24 hours from anchor time; knobs and hard
  ranges are in [configuration](../configuration.md#optional-forge-interoperability).
  Truncation prefers newest anchors and warns. One newest authenticated job is
  tracked per run. First terminal result atomically stores the claim and removes
  lineage; later workflow/compute results for that run are intentionally not
  collected. Expired/capped runs are incomplete evidence, not failed CI.
- Ingest uses existing ID/signature/timestamp validation and handler dedup.
  Results from undeclared signers or ambiguous references do not become claims.
  New lineage tables copy no arguments, payment tokens, or encrypted secrets;
  the existing generic ingest event retention still applies.
- Context lookup is exact repository + resolved PR tip, or the patch event's
  unique full `commit`/`c` value when patch preparation has no git tip. Never
  match abbreviated hashes or substitute the base branch. The schema/read API
  also supports exact patch-event references, but the verified upstream format
  supplies no standardized one, so ingestion does not fabricate that linkage.
- The compact, token-budgeted “CI attestations” layer shows at most 16 recent
  matches: claim type, status, worker (or explicitly unknown), actual signer,
  timestamp, event ID. Publisher-signed workflow claims never masquerade as
  worker signatures. These are third-party claims, not execution verification,
  authorization, payment credit, enrollment, or deterministic status-gate input.
  Drydock never dispatches jobs, waits for CI, or downloads result logs here.
  The pipeline freezes the rendered claim snapshot before context discovery.
  Both deterministic context and successful agentic discovery include it; the
  latter reuses the existing exact token gate and drops an oversized optional
  section rather than changing authoritative patch bytes or blocking review.

### Community notices

- `DRYDOCK_CONCORD_ENABLED=false` by default; `.drydock.yaml` opts a repository
  into `notifications.concord` using only community ID, channel ID, explicit
  epoch, and validated relays. The normal base-branch policy loader supplies it.
- The one operator environment variable `DRYDOCK_CONCORD_STREAM_KEYS` maps exact
  `community/channel/epoch` locators to pre-derived stream secrets. No key field
  exists in repository config. Parser errors are deliberately generic, including
  errors from libraries that otherwise echo invalid secret input. Credentials
  are not added to logs, metrics, notices, or review events.
- Envelope construction is one pure, fixture-tested function. It accepts all
  content/time/entropy/key/epoch inputs; a first call prepares the seal, the
  existing author signer signs outside that boundary, and a second call verifies
  the supplied signature and builds the stream-signed wrap. No relay, database,
  environment lookup, clock, or remote signing enters the construction function.
- After the review is successfully marked published, an asynchronous best-effort
  attempt emits a kind-9 notice containing only a public review `nostr:nevent`
  link (including author and review-relay hints). Four attempts may run at once;
  overload drops/logs a notice rather than delaying review. Missing credentials
  warn once per locator and skip. Transport/signing failures never fail review.
- Key derivation/provisioning, posting permission, channel identity verification,
  and epoch/key rotation are **operator responsibilities**. Replace the public
  epoch and corresponding operator credential, then restart to reload keys.
  Supplying only a stream credential does not let Drydock verify its root-key
  derivation or fold current bans/roles. Provision only channels that permit this
  service to post. No automatic invites, rekey, moderation/metadata sync,
  disappearing-message timer, or deletion support is claimed; use channels with
  expiration disabled. Ephemeral recipient secrets are not retained.
- Generic relay transport reuses existing OK/quorum handling. It is not a new
  durable outbox: crashes, process shutdown, capacity drops, or repeated caller
  publication may lose or duplicate notices. Relay acceptance is not rendering.

## Changed-file map

| Area | Production/documentation files | Tests |
|---|---|---|
| Composition | `cmd/drydock/main.go` | Existing composition suite |
| Operator config | `internal/config/config.go`, `internal/config/forge_interop.go` | `internal/config/forge_interop_test.go` |
| Repository config | `internal/repoconfig/config.go`, `internal/repoconfig/notifications.go` | `internal/repoconfig/notifications_test.go` |
| Kinds | `internal/eventkind/kinds.go` | Ingest/envelope assertions |
| Persistence | `internal/db/store.go`, `internal/db/store_loom.go` | `internal/db/store_test.go` pins; Loom/context tests |
| Ingest/lineage | `internal/ingest/processor.go`, `internal/loom/service.go` | `internal/ingest/loom_test.go`, `internal/loom/service_test.go` |
| Routing | `internal/relayroute/source.go`, `internal/relayroute/loom.go`, `internal/listener/listener.go` | `internal/listener/loom_test.go` |
| Review context | `internal/contextbuilder/builder.go`, `internal/contextbuilder/ci_attestations.go`, `internal/pipeline/runner.go`, `internal/pipeline/forge_interop.go` | `internal/contextbuilder/ci_attestations_test.go`, `internal/pipeline/forge_interop_test.go` |
| Agentic success path | `internal/agenticreview/discovery.go`, `internal/agenticreview/ci_context.go` | `internal/agenticreview/ci_context_test.go` |
| Publication | `internal/publisher/service.go`, `internal/publisher/concord.go`, `internal/publisher/concord_envelope.go` | `internal/publisher/concord_test.go` |
| Docs | This review, `docs/event-kinds.md`, `docs/configuration.md`, `docs/reviews/monitoring-authority-security-review-2026-10-27.md` | — |

## Verification

New tests use synthetic signed events, local SQLite, deterministic time/entropy
fixtures, and injected transports; no live network is used by these tests.

| Area | Verified locally |
|---|---|
| Ingest | Signed run/result persistence, duplicate result idempotency, invalid-ID/signature refusal, disabled no-op |
| Lineage | Unmonitored/ambiguous anchors add nothing; exact ID **and** author filters; undeclared result author ignored; native and workflow identities distinguished |
| Bounds | Out-of-order anchors retain newest; per-repo/global caps; TTL and result eviction; active lineage stored in SQLite |
| Routing | A monitored run triggers the existing listener resubscription; exactly its publisher/result filter appears; unrelated repo relay does not receive it |
| Context | Exact repo/commit and patch-ID read matching, no cross-repo/unrelated commit leakage, labelled compact rendering, provider-disabled no-op, patch/PR commit selection, successful agentic discovery inclusion and non-blocking budget drop |
| Concord | Fixed canonical rumor hash, both decryption layers, no rumor signature field, seal/wrap signatures, kinds/tags, untweaked timestamps, self-ECDH rather than recipient encryption, explicit rotated-key isolation |
| Delivery/config | Validated public opt-in, secret input errors do not echo keys, missing-key warn-once, disabled no-op, injected blocked/failing transport cannot block/fail review publication |
| Migration | Versioned migration and both count pins; full existing migration suite |

`make ci` **passed** locally: `CGO_ENABLED=1` vet, build, and uncached full tests.
Focused new Loom, listener-resubscription, and publisher tests also passed with
`CGO_ENABLED=1 go test -race -gcflags=all=-d=checkptr=0` and a name filter;
this was not a full-suite race run. The checkptr exception is the existing
upstream workaround documented in `AGENTS.md`, not a change to this feature.
No live Loom job, external client
conformance vector, public relay EOSE/OK readback, Armada/Budabit UI rendering,
production signer, or deployment acceptance was exercised. Pinned source shapes
and local cryptographic tests do not establish those external properties.
