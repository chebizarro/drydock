# Nostr Event Kinds

This document is the Drydock reference for Nostr event kinds and tag conventions.

## Comprehensive Kind Reference

| Kind | Standard | Name | Drydock Use |
|------|----------|------|-------------|
| 0 | NIP-01 | Profile metadata | Published/refreshed at startup for the Drydock identity |
| 5 | NIP-09 | Deletion | Deletes the operator's monitored-repositories list when its `a` tag names that exact list |
| 13 | NIP-59 | Seal | Sender-signed encrypted seal inside gift wraps; never published directly |
| 14 | NIP-17 | Private direct message | Plaintext unsigned rumor inside a seal; never published directly |
| 1059 | NIP-59 | Gift wrap | Ephemerally signed encrypted envelope for private Drydock traffic |
| 1111 | NIP-22 | Comment | Published code review comments and thread replies |
| 1617 | NIP-34 | Patch | Reactive review candidate when its repository is monitored |
| 1618 | NIP-34 | Pull request | Reactive PR-tip review candidate; diff is computed from git |
| 1619 | NIP-34 | Pull request update | Reactive PR-update candidate and comment-thread parent |
| 1621 | NIP-34 | Pull request revision | Stored for PR history; not directly reviewed |
| 1630 | NIP-34 | Status: open | Root status tracking; eligible when the repository is monitored |
| 1631 | NIP-34 | Status: applied/merged | Root status tracking; not reviewed unless an authorized on-demand order uses `force` |
| 1632 | NIP-34 | Status: closed | Root status tracking; not reviewed unless an authorized on-demand order uses `force` |
| 1633 | NIP-34 | Status: draft | Eligible only when repository policy opts in, or an authorized order uses `force` |
| 1985 | NIP-32 | Label | Ingested for context and routing |
| 9735 | NIP-57 | Zap receipt | Payment authorization linked to a patch/PR by `e` tag |
| 25910 | ContextVM | JSON-RPC transport | Review/order, IDE, marketplace, audit, feedback, and progress messages |
| 30001 | NIP-51 | Named list | Operator-authored `drydock:monitored-repositories:v1` list |
| 30078 | NIP-78 | Application data | IDE session state and replaceable app state |
| 30617 | NIP-34 | Repository announcement | Repository metadata, clone URL, relay hints |
| 30618 | NIP-34 | Repository state snapshot | Repository state and staleness gate |
| 31990 | NIP-89 | Handler/reviewer profile | Reviewer discovery and capability advertisement |
| 22242 | NIP-42 | AUTH event | Relay authentication challenge response |
| 24242 | Blossom BUD-01 | Media authorization | Signed HTTP upload authorization; never published to relays |
| 10002 | NIP-65 | Relay list metadata | User relay discovery when available |

Kind `7000` is no longer subscribed to or routed. Legacy NIP-90 kinds `5900`, `6900`, and `7000` are also absent from NIP-document indexing. External feedback publishers must switch to `marketplace/feedback` before deployment.

## Deprecated Drydock and NIP-90 Mappings

| Deprecated Kind | Replaced By | Notes |
|-----------------|-------------|-------|
| 31650 | 30078 | IDE session state is NIP-78 application data |
| 1651, 1652 | 25910 | `review/request` request/response |
| 1653, 1654 | 25910 | `review/apply-fix` request/response |
| 30620 | 31990 | Reviewer profile is a NIP-89 handler profile |
| 1660 | 25910 | `marketplace/assign` request |
| 1661 | 25910 | `marketplace/accept` request |
| 1662 | 25910 | `marketplace/reject` request |
| 1663, 7000 | 25910 | `marketplace/feedback` notification |
| 5900, 6900 | none | Retired NIP-90 request/result kinds; not part of Drydock runtime |

## Reactive Monitoring: NIP-51 Kind 30001

Automatic NIP-34 review is fail-closed and controlled by one parameterized replaceable list:

- kind: `30001`
- author: `DRYDOCK_MONITORED_REPOS_AUTHOR`
- exactly one `d` tag: `drydock:monitored-repositories:v1`
- zero or more `a` tags, each a canonical `30617:<owner-pubkey>:<identifier>` address

Drydock accepts only the configured author's newest valid replacement. Equal timestamps use the lower event ID as the winner. Foreign authors, wrong `d` tags, malformed repository addresses, and older revisions do not change the active snapshot. A valid empty list disables reactive review. A kind-5 event from the same author whose `a` tag names the exact list address deletes it; a newer valid list recreates it.

The control-plane subscription has no `since` bound, and the winning revision is persisted for restart recovery. Until an authoritative list, empty list, or deletion has loaded, reactive review remains disabled and readiness fails in configured deployments.

## ContextVM Kind 25910

Requests include a non-empty JSON-RPC `id` and receive a correlated response. Notifications omit `id`, produce no response, and are deduplicated by Nostr event ID.

Request methods include:

- `review/request` and `review/apply-fix` for IDE sessions
- `review/order` for session-independent stored-patch orders
- `marketplace/assign`, `marketplace/accept`, `marketplace/reject`, and `marketplace/complete`
- `security/audit` and `security/audit/sarif`

Notification methods include:

- `marketplace/feedback`
- `security/audit/progress`

See [ContextVM Integration](contextvm-integration.md).

## Other Standard Kinds

### NIP-22 Comment (1111)

Drydock publishes review summaries and detail comments as kind `1111`, threaded to patches and PRs with root and parent tags.

### NIP-34 Repository and Patch Events

Drydock stores repository, patch, pull-request, revision, status, and snapshot events. Kinds `1617`, `1618`, and `1619` are reactive candidates only when their unique repository `a` tag is present in the active monitored list and passes the static security ceiling. On-demand `review/order` uses a stored patch and does not require list membership.

### NIP-57 Zap Receipt (9735)

Receipts must address the service with `p`, identify the patch/PR with `e`, and carry a valid positive amount. Qualifying receipts authorize payment-gated reviews and can re-enqueue payment-blocked work.

### NIP-78 Application Data (30078)

IDE session state is replaceable application data with a stable `d` tag such as `drydock:ide-session:<session-id>`.

### NIP-89 Handler Profile (31990)

Marketplace reviewers publish NIP-89 profiles to advertise languages, domains, availability, pricing, and supported outputs.

### NIP-17/NIP-59 Privacy (13, 14, 1059)

Private ContextVM requests and notifications may be gift-wrapped. The visible kind-1059 wrapper exposes only recipient routing; the authenticated unsigned inner rumor is kind `25910`. Plain signed kind-25910 messages remain supported for non-sensitive workflows.

## Tag Conventions

| Tag | Applies To | Purpose |
|-----|------------|---------|
| `p` | Routed events | Recipient or referenced participant pubkey |
| `e` | Comments and ContextVM | Related request, patch, assignment, or review event |
| `E` | NIP-22 comments | Root event ID |
| `k` / `K` | NIP-22 comments | Parent/root event kinds |
| `a` | Addressable references | Repository, session, or monitored-list address |
| `A` | NIP-22 comments | Root repository address |
| `d` | Replaceable/addressable events | Stable identifier, including the monitored-list identity |
| `t` | Discoverable events | Topic routing |
| `method` | Kind 25910 | ContextVM method name |
| `expiration` | ContextVM/private workflows | Unix timestamp for relay discard eligibility |
| `client` | Kind 30078 | IDE client identifier and version |
| `amount` / `bolt11` | Kind 9735 | Paid amount and fixed-amount invoice |

## Subscription Examples

Monitored-repositories control plane (intentionally no `since`):

```json
{"kinds":[30001],"authors":["<operator-pubkey>"],"#d":["drydock:monitored-repositories:v1"]}
{"kinds":[5],"authors":["<operator-pubkey>"],"#a":["30001:<operator-pubkey>:drydock:monitored-repositories:v1"]}
```

ContextVM inbox:

```json
{"kinds":[25910],"#p":["<drydock-pubkey>"],"#method":["review/order","marketplace/feedback","security/audit"]}
```

Recipient-scoped gift wraps:

```json
{"kinds":[1059],"#p":["<drydock-pubkey>"]}
```

NIP-34 intake remains broad so newly monitored repositories take effect immediately; membership is enforced atomically at admission and rechecked for reactive tasks before publication.
