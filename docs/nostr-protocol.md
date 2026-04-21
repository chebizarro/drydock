# Nostr Protocol Integration

Drydock integrates with the Nostr protocol as both a consumer (listening for NIP-34 code collaboration events) and a producer (publishing structured review comments).

## Subscribed Event Kinds

| Kind | NIP | Name | Drydock Role |
|------|-----|------|-------------|
| 30617 | NIP-34 | Repository announcement | Repository registry — extracts clone URLs, relay hints, and metadata |
| 30618 | NIP-34 | Repository state snapshot | Staleness gate — if a patch's tip commit is already in the latest snapshot, the review is skipped |
| 1617 | NIP-34 | Patch event | **Primary review trigger** — contains unified diff in `content` |
| 1618 | NIP-34 | PR (pull request) event | PR tip review trigger |
| 1619 | NIP-34 | PR update event | Review trigger — uses `E` tag to find root PR for comment threading |
| 1621 | NIP-34 | PR revision | Monitored and stored; not directly reviewed |
| 1111 | NIP-22 | Comment | Review output kind; also ingested for thread cache |
| 1630 | NIP-34 | Status: open | Root status tracking |
| 1631 | NIP-34 | Status: applied/merged | Root status tracking — patches with applied roots are not reviewed |
| 1632 | NIP-34 | Status: closed | Root status tracking — patches with closed roots are not reviewed |
| 1633 | NIP-34 | Status: draft | Root status tracking |
| 1985 | NIP-32 | Label | Monitored and stored |

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

```
---
model: coder32b
context-hash: a1b2c3...
patch-event-id: abc123...
repo-id: npub1...:reponame
review-mode: automated
confidence: 0.85
context-layers-used: patch, modified-files, symbols
context-layers-dropped: commit-history, project-docs
```

The `context-layers-dropped` field is always present (even when empty) for auditability.

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

This means:
- No events are missed across restarts
- A small window of events may be re-delivered (deduplicated by `ingested_events` primary key)
- If the high-water-mark is older than the configured lookback, the lookback window takes precedence
