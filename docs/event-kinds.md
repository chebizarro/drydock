# Nostr Event Kinds

This document is the Drydock reference for Nostr event kinds and tag conventions.

## Comprehensive Kind Reference

| Kind | Standard | Name | Drydock Use |
|------|----------|------|-------------|
| 1059 | NIP-59 | Gift wrap | Encrypted envelope for private Drydock events |
| 1111 | NIP-22 | Comment | Published code review comments and thread replies |
| 1617 | NIP-34 | Patch | Primary automated review trigger |
| 1618 | NIP-34 | Pull request | PR tip review trigger |
| 1619 | NIP-34 | Pull request update | PR update review trigger and comment-thread parent |
| 1621 | NIP-34 | Pull request revision | Monitored and stored for PR history |
| 1630 | NIP-34 | Status: open | Root status tracking |
| 1631 | NIP-34 | Status: applied/merged | Root status tracking; suppresses duplicate review |
| 1632 | NIP-34 | Status: closed | Root status tracking; suppresses duplicate review |
| 1633 | NIP-34 | Status: draft | Root status tracking |
| 1985 | NIP-32 | Label | Labels ingested for context and routing |
| 7000 | NIP-90 | Job feedback | Marketplace feedback and completion feedback |
| 25910 | ContextVM | JSON-RPC transport | Review, fix, assignment, accept, and reject methods |
| 30078 | NIP-78 | Application data | IDE session state and replaceable app state |
| 30617 | NIP-34 | Repository announcement | Repository metadata, clone URL, relay hints |
| 30618 | NIP-34 | Repository state snapshot | Repository state and staleness gate |
| 31990 | NIP-89 | Handler/reviewer profile | Reviewer discovery and capability advertisement |
| 22242 | NIP-42 | AUTH event | Relay authentication challenge response |
| 10002 | NIP-65 | Relay list metadata | User relay discovery when available |

## Deprecated Drydock Kinds

These project-specific kinds are no longer part of the Nostr-native strategy:

| Deprecated Kind | Replaced By | Notes |
|-----------------|-------------|-------|
| 31650 | 30078 | IDE session state is NIP-78 application data |
| 1651 | 25910 | `review/request` ContextVM method |
| 1652 | 25910 | `review/request` response |
| 1653 | 25910 | `review/apply-fix` ContextVM method |
| 1654 | 25910 | `review/apply-fix` response |
| 30620 | 31990 | Reviewer profile is a NIP-89 handler profile |
| 1660 | 25910 | `marketplace/assign` ContextVM method |
| 1661 | 25910 | `marketplace/accept` ContextVM method |
| 1662 | 25910 | `marketplace/reject` ContextVM method |
| 1663 | 7000 | Marketplace feedback uses NIP-90 feedback |

## Standard NIP Kinds Used

### NIP-22 Comment (`1111`)

Drydock publishes review summaries and detail comments as kind `1111`. Comments are threaded to patches and PRs with root and parent tags.

### NIP-34 Repository and Patch Events (`1617`, `1618`, `1619`, `1621`, `1630`-`1633`, `30617`, `30618`)

Drydock consumes NIP-34 repository, patch, pull request, update, revision, and status events. Patch and PR events are the review inputs; status events decide whether work should be skipped.

### NIP-78 Application Data (`30078`)

IDE session state is represented as replaceable application data. Use a stable `d` tag such as `drydock:ide-session:<session-id>`.

### ContextVM JSON-RPC (`25910`)

Application commands use ContextVM JSON-RPC envelopes in kind `25910`. Supported methods are:

- `review/request`
- `review/apply-fix`
- `marketplace/assign`
- `marketplace/accept`
- `marketplace/reject`

See [ContextVM Integration](contextvm-integration.md).

### NIP-89 Handler/Reviewer Profile (`31990`)

Marketplace reviewers publish NIP-89 profiles to advertise languages, domains, availability, pricing, and supported review outputs.

### NIP-90 Feedback (`7000`)

Marketplace feedback uses kind `7000` with tags for status, rating, reviewer, and related review events.

### NIP-59 Gift Wrap (`1059`)

Private Drydock payloads should be gift-wrapped. This includes source code snippets, diagnostics, fix requests, assignments, and reviewer decisions.

## Tag Conventions

| Tag | Applies To | Purpose |
|-----|------------|---------|
| `p` | All routed events | Recipient or referenced participant pubkey |
| `e` | Comments, ContextVM, feedback | Related event ID such as request, patch, assignment, or review |
| `E` | NIP-22 comments | Root event ID for comment thread |
| `k` | NIP-22 comments | Parent event kind |
| `K` | NIP-22 comments | Root event kind |
| `a` | Addressable references | Repository, profile, or session address |
| `A` | NIP-22 comments | Root repository addressable reference |
| `d` | Replaceable/addressable events | Stable identifier for kind `30078` and `31990` |
| `t` | Most events | Topic routing such as `drydock`, `review`, `marketplace`, language, or domain |
| `method` | Kind `25910` | ContextVM JSON-RPC method name |
| `status` | Kind `7000` | NIP-90 feedback status such as `success` or `error` |
| `rating` | Kind `7000` | Marketplace rating value, typically `1` through `5` |
| `expiration` | Ephemeral/private workflows | Unix timestamp for relay discard eligibility |
| `client` | Kind `30078` | IDE client identifier and version |

## Addressable References

Use addressable references for replaceable or parameterized events:

```text
<kind>:<pubkey>:<d-tag>
```

Examples:

```text
30078:<ide-pubkey>:drydock:ide-session:session-uuid
31990:<reviewer-pubkey>:drydock-reviewer
30617:<repo-owner-pubkey>:<repo-id>
```

## Subscription Examples

Review request inbox:

```json
{"kinds": [25910], "#p": ["<drydock-pubkey>"], "#method": ["review/request"]}
```

Marketplace assignment inbox:

```json
{"kinds": [25910], "#p": ["<reviewer-pubkey>"], "#method": ["marketplace/assign"]}
```

Reviewer discovery:

```json
{"kinds": [31990], "#t": ["code-reviewer"]}
```

NIP-34 patch review stream:

```json
{"kinds": [1617, 1618, 1619], "since": 1714000000}
```

## Encryption Guidance

Public discovery events, such as reviewer profiles, can remain unencrypted. Private workflow messages should use NIP-59 gift-wrap and avoid leaking sensitive details in outer tags. Use `p` tags for recipient routing and keep sensitive filenames, snippets, findings, and assignment details inside the encrypted payload.
