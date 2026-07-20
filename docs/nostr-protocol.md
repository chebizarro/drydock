# Nostr Protocol Integration

Drydock integrates with Nostr as both a consumer and producer of signed events. The current strategy uses standard NIPs where possible and carries application commands through ContextVM JSON-RPC envelopes on kind `25910`, instead of maintaining Drydock-only request/response event kinds.

## Subscribed Event Kinds

| Kind | NIP | Name | Drydock Role |
|------|-----|------|-------------|
| 30617 | NIP-34 | Repository announcement | Repository registry — extracts clone URLs, relay hints, and metadata |
| 30618 | NIP-34 | Repository state snapshot | Staleness gate — if a patch's tip commit is already in the latest snapshot, the review is skipped |
| 1617 | NIP-34 | Patch event | **Primary review trigger** — contains unified diff in `content` |
| 1618 | NIP-34 | PR (pull request) event | PR tip review trigger — `content` is a cover letter; the reviewed diff is computed from git (see below) |
| 1619 | NIP-34 | PR update event | Review trigger — diff computed like 1618; uses `E` tag to find root PR for comment threading |
| 1621 | NIP-34 | PR revision | Monitored and stored; not directly reviewed |
| 1111 | NIP-22 | Comment | Review output kind; also ingested for thread cache |
| 1630 | NIP-34 | Status: open | Root status tracking — reviewed automatically (default) |
| 1631 | NIP-34 | Status: applied/merged | Root status tracking — never auto-reviewed |
| 1632 | NIP-34 | Status: closed | Root status tracking — never auto-reviewed |
| 1633 | NIP-34 | Status: draft | Root status tracking — auto-reviewed only when the repo opts in via `review.statuses` |
| 1985 | NIP-32 | Label | Monitored and stored |
| 30078 | NIP-78 | Application data | IDE session state and replaceable Drydock client state |
| 31990 | NIP-89 | Handler/reviewer profile | Reviewer capability profiles for marketplace discovery |
| 25910 | ContextVM | JSON-RPC transport | Review, fix, assignment, accept, and reject commands |
| 7000 | NIP-90 | Job feedback | Marketplace feedback and review completion feedback |
| 9735 | NIP-57 | Zap receipt | Payment proof addressed to Drydock and linked to a patch/PR event |
| 1059 | NIP-59 | Gift wrap | Encrypted wrapper for private Drydock events |

## NIP-57 Zap Payment Receipts

Kind `9735` receipts are public payment proofs. Drydock requires a `p` tag matching its signer pubkey, an `e` tag naming the patch/PR event, a positive amount from `amount` or `bolt11`, and—when configured—a receipt author in `DRYDOCK_TRUSTED_ZAPPERS`. Repository policy compares the receipt millisatoshis with `payments.price_sats * 1000` and can disable this path with `payments.accept_zaps: false`.

A receipt received after a review was recorded as `payment_blocked` clears that failure and re-enqueues the review. Receipts received before the patch remain stored for authorization when the patch reaches the pipeline.

## Review Diff Derivation

What Drydock actually reviews depends on the trigger kind:

- **Kind 1617 (patch)**: the event `content` *is* the unified diff and is used
  directly, after applying the whole patch series on a throwaway branch.
- **Kind 1618/1619 (PR / PR update)**: the event `content` is a cover letter,
  not a diff. Drydock fetches the `c`-tag tip commit, checks it out on a
  review branch, and computes `git diff <merge-base(default, tip)>..tip`.
  The diff is computed in the **canonical** clone whenever possible so a
  fork-controlled `origin` cannot choose the diff base and hide changes.
  A PR whose tip is already contained in the default branch, or whose
  history is unrelated, fails closed with no review.

The changed-file set parsed deterministically from this diff is authoritative
downstream: reviews refuse to run when it is empty, and findings or
walkthrough file summaries referencing paths outside it are dropped before
publication (the model also sees contextual layers such as project docs and
must not present them as modified files).

## Status Gating

Automatic reviews respect the root's current NIP-34 status at review time
(re-checked after ingest to avoid racing late-arriving status events):

| Root status | Auto-reviewed? |
|-------------|----------------|
| none / kind 1630 (open) | Yes (default) |
| kind 1633 (draft) | Only when the repo config opts in (`review.statuses: [open, draft]`) |
| kind 1631 (applied/merged) | Never |
| kind 1632 (closed) | Never |

Status-gated skips are permanent (not retried by the failed-review sweep); a
later status change back to open triggers reviews normally. An authorized
ContextVM `review/request` with `force: true` can explicitly bypass this status
gate, including for a prior `status_skipped:` target, while scope and payment
gates still apply. See [ContextVM Integration](contextvm-integration.md) and
[Per-Repository Configuration](repo-config.md).

## Kind 0 Profile

On startup Drydock ensures its signing identity has a kind 0 profile: it
fetches the newest kind 0 from the read relays and publishes a fresh one when
none exists or when the configured metadata (name, about, website, picture,
banner) has changed. Unmanaged fields already present in the profile are
preserved. The icon and banner images are pushed to a Blossom media server
(BUD-01/BUD-02) and referenced by content-addressed URLs. See
[Configuration — Kind 0 Profile & Media](configuration.md#kind-0-profile--media).

## Nostr-Native Event Strategy

Drydock avoids bespoke one-off event kinds for application commands. The mapping is:

| Use Case | Previous Kind(s) | Current Kind |
|----------|------------------|--------------|
| IDE session | 31650 | 30078 (NIP-78) |
| IDE review request/response | 1651, 1652 | 25910 (ContextVM JSON-RPC) |
| IDE fix request/response | 1653, 1654 | 25910 (ContextVM JSON-RPC) |
| Reviewer profile | 30620 | 31990 (NIP-89) |
| Marketplace assignment | 1660 | 25910 (ContextVM JSON-RPC) |
| Marketplace accept/reject | 1661, 1662 | 25910 (ContextVM JSON-RPC) |
| Marketplace feedback | 1663 | 7000 (NIP-90 feedback) |

## ContextVM JSON-RPC Transport (kind 25910)

ContextVM messages use kind `25910` with JSON-RPC 2.0 in the event `content`. Nostr supplies identity, signatures, relay transport, and routing tags; ContextVM supplies method names, parameters, correlation IDs, responses, and errors.

Example request:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "req-01HZX...",
    "method": "review/request",
    "params": {
      "session_id": "session-uuid",
      "file": "src/auth.go",
      "selection": {"start": 10, "end": 25},
      "trigger": "save"
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["t", "drydock"],
    ["method", "review/request"]
  ]
}
```

Example response:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "req-01HZX...",
    "result": {
      "diagnostics": []
    }
  },
  "tags": [
    ["p", "<requester-pubkey>"],
    ["e", "<request-event-id>"],
    ["t", "drydock"],
    ["method", "review/request"]
  ]
}
```

Supported methods are documented in [ContextVM Integration](contextvm-integration.md).

## Encryption (NIP-59 Gift Wrap)

Private IDE, review, fix, marketplace assignment, acceptance, and rejection payloads should be protected with NIP-59 gift-wrap. The outer visible event is a gift-wrap event (`1059`) addressed to the recipient; the sealed rumor inside carries the Drydock event such as kind `25910`.

Use gift-wrap when event content includes source code, diagnostics, assignment details, reviewer decisions, or payment-sensitive metadata. Public discovery data, such as NIP-89 reviewer profiles (`31990`), may remain unwrapped when the reviewer intends it to be discoverable.

Routing tags on encrypted envelopes must be sufficient for relay delivery without leaking private payload details. Prefer `p` tags for recipients and avoid sensitive filenames, repository names, or finding text in outer tags.

## NIP-11 Relay Capability Probing

On startup, Drydock probes each configured relay using NIP-11 (`GET` with `Accept: application/nostr+json`). This is non-blocking and log-only:

- Logs relay name, software, and supported NIPs
- Warns if a relay requires authentication (`auth_required`) but no signer is configured
- Warns if a relay requires payment (`payment_required`)

No relay is skipped based on NIP-11 results — the probe is informational.

## NIP-42 AUTH

When a signer is configured, Drydock registers an `AuthRequiredHandler` on the Nostr pool. If a relay sends an AUTH challenge, the handler signs the auth event automatically.

The `SubscribeManyNotifyClosed` channel reports whether auth was handled:

```json
{"level":"INFO","msg":"relay required auth and was re-authenticated","relay":"wss://...","reason":"auth-required:"}
```

## Review Comment Structure (kind 1111)

Drydock publishes two types of kind 1111 events per review:

### Summary Comment

One per review. Contains the review summary, all findings in a table, and a metadata footer.

### Detail Comments

One per finding at or above the configured severity floor (default: `high`). Contains the full finding with evidence, explanation, and suggestion.

### Tag Layout

Both summary and detail events use the same tag structure:

| Tag | Value | Purpose |
|-----|-------|---------|
| `E` | root event ID | Root of the thread (patch or PR event) |
| `K` | root event kind | Kind of the root event |
| `e` | parent event ID | Direct parent (usually same as root for patches) |
| `k` | parent event kind | Kind of the parent event |
| `A` | `30617:<repo_id>` | Repository pointer (addressable event reference) |
| `P` | root author pubkey | Pubkey of the root event author |
| `p` | parent author pubkey | Pubkey of the parent event author |
| `expiration` | Unix timestamp | Event TTL — default 90 days, 7 days for superseded reviews |

### Metadata Footer

Every review comment includes a plaintext footer:

```text
---
model: qwen2.5-coder-32b-instruct-q4_k_m
context-hash: a1b2c3...
patch-event-id: abc123...
repo-id: npub1...:reponame
review-mode: automated
confidence: 0.85
context-layers-used: patch, modified-files, symbols
context-layers-dropped: commit-history, project-docs
```

The `context-layers-dropped` field is always present (even when empty) for auditability.

The `model` field names the model that **actually served** the reviewer
request: preferentially the identifier reported in the endpoint's
chat-completion response for that specific run, falling back to the served-model
registry (seeded by a startup `/v1/models` probe) and then the configured
model name. Internal route aliases (`coder32b`, `llm70b`, `coder14b`) are
never published when better information exists.

## Comment Scope Derivation

Drydock determines how to thread review comments based on the patch event's kind:

**Kind 1617 (patch)**: The patch event is both the root and parent. The review comment is a direct reply.

**Kind 1619 (PR update)**: The `E` tag points to the root PR event (kind 1618). The PR update itself is the parent. This threads the review under the PR conversation.

**Kind 1618 (PR)**: Same as patch — the PR event is both root and parent.

## Publishing Guard

The publisher includes an explicit check that prevents accidentally emitting status events:

```go
if summaryEvent.Kind == 1631 || summaryEvent.Kind == 1632 {
    return "", errors.New("publisher must not emit status events 1631/1632")
}
```

Drydock only publishes kind 1111 (comment) events. It never sets status (applied, closed) on behalf of repository maintainers.

## Signing

Two signing methods are supported:

| Method | Config | Key Location | Use Case |
|--------|--------|-------------|----------|
| NIP-46 Bunker | `DRYDOCK_SIGNER_BUNKER_URL` | Remote bunker device/service | Production |
| Local nsec | `DRYDOCK_SIGNER_NSEC` | Plaintext in environment | Development |

The bunker signer uses `fiatjaf.com/nostr/keyer` and supports both `bunker://` URLs and NIP-05 identifiers. On connection, the signer validates by calling `GetPublicKey`.

See [Deployment](deployment.md#signing-configuration) for setup instructions.

## High-Water-Mark Persistence

The listener tracks the most recent event timestamp in the `listener_state` SQLite table. On restart, it uses this timestamp (minus a 30-second overlap for clock skew) as the `since` filter for relay subscriptions.

Only timestamps within `DRYDOCK_LISTENER_MAX_FUTURE_SKEW` may advance the
cursor. If an older deployment persisted an implausible future cursor, Drydock
resets it to the configured lookback boundary on startup and resumes from
there.

This means:
- No events are missed across restarts
- A small window of events may be re-delivered (deduplicated by `ingested_events` primary key)
- If the high-water-mark is older than the configured lookback, the lookback window takes precedence
