# ContextVM Integration

Drydock uses JSON-RPC 2.0 envelopes in Nostr kind `25910` for application intent. Nostr provides authenticated identity, relay delivery, and routing tags; ContextVM provides methods, parameters, correlation, results, and errors. The protocol is generic: Loom and other orderers call the same methods without Drydock containing Loom-specific event kinds or code.

Private messages may be carried as NIP-59 gift wraps. The visible outer event is kind `1059`; the verified unsigned inner rumor is kind `25910`. Plain signed kind-25910 events are also supported.

## Requests and Notifications

A **request** has a non-empty JSON-RPC `id`. Drydock returns exactly one response with the same `id` and an `e` tag naming the request event.

A **notification** omits `id`. It is one-way and never produces a JSON-RPC response, including when it is unknown, malformed, unauthorized, rate-limited, or duplicated. Nostr event IDs provide delivery deduplication.

| Method | Shape | Direction | Purpose |
|--------|-------|-----------|---------|
| `review/request` | request | IDE → Drydock | Session-bound inline or stored-patch review |
| `review/apply-fix` | request | IDE → Drydock | Apply an IDE diagnostic fix |
| `review/order` | request | Any authenticated orderer → Drydock | Session-independent stored-patch order |
| `marketplace/assign` | request | Drydock/orderer → Reviewer | Offer an assignment |
| `marketplace/accept` | request | Reviewer → Drydock | Accept an assignment |
| `marketplace/reject` | request | Reviewer → Drydock | Reject an assignment |
| `marketplace/complete` | request | Reviewer → Drydock | Correlate a published review and trigger payout |
| `marketplace/feedback` | notification | Assignment requester → Drydock | Rate a completed review |
| `security/audit` | request | Requester → Drydock | Start a whole-repository audit |
| `security/audit/sarif` | request | Original requester → Drydock | Retrieve authorized SARIF |
| `security/audit/progress` | notification | Drydock → Requester | Report audit progress or terminal state |

## Generic Review Orders

`review/order` invokes the normal review pipeline without an IDE session and without requiring membership in the monitored-repositories list. The target must still pass the static repository/owner security ceiling, canonical repository policy, payment preflight, and force authorization.

### Request

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "order-01HZX...",
    "method": "review/order",
    "params": {
      "patch_event_id": "<stored-kind-1617-1619-event-id>",
      "repo_addr": "30617:<owner-pubkey>:<identifier>",
      "force": false
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["e", "<patch-event-id>"],
    ["a", "30617:<owner-pubkey>:<identifier>"],
    ["method", "review/order"],
    ["expiration", "1714003200"]
  ]
}
```

Rules:

- `id`, `patch_event_id`, exactly one matching `p`, `e`, `method`, and `expiration` tag are required.
- Expiration must be in the future and no more than 15 minutes after event creation.
- `patch_event_id` must already be stored and must contain exactly one canonical kind-30617 `a` tag.
- `repo_addr` and the event `a` tag are optional, but when present they must match each other and the stored patch.
- Address-only orders are rejected because a repository address does not identify a deterministic diff.
- A missing patch or repository announcement returns not found.
- The authenticated sender is the requester. Callers cannot supply another requester identity.

Drydock checks the static repository/owner allowlists before consulting payment. A paid order may bypass **monitoring membership**, but payment never bypasses the static security ceiling. Repository payment policy is loaded from the canonical base branch. An authorized `force` bypasses only root-status gating.

### Accepted Response

```json
{
  "jsonrpc": "2.0",
  "id": "order-01HZX...",
  "result": {
    "accepted": true,
    "order_id": "order-01HZX...",
    "request_event_id": "<original-event-id>",
    "patch_event_id": "<patch-event-id>",
    "repo_addr": "30617:<owner-pubkey>:<identifier>",
    "forced": false,
    "state": "queued"
  }
}
```

Acceptance is durable intake, not completion. `state` is `queued` when the in-memory wake-up was emitted and `retry_pending` when a queue-full condition left the durable task for recovery. The pipeline publishes results through the normal NIP-34 comment flow.

The pair `(requester pubkey, JSON-RPC id)` is the immutable order key. Exact retries return the original receipt. Reusing the key for different parameters, or concurrently ordering a target already claimed by another order, returns conflict. Receipt creation and review claiming are atomic, and restart recovery preserves invocation, requester, order ID, and force metadata.

## IDE Review Requests

`review/request` remains session-bound. Inline mode reviews the supplied diff synchronously. Stored-patch mode validates session ownership and then delegates scope, payment, durable claiming, and queueing to the same review-order service.

| Field | Required | Description |
|-------|----------|-------------|
| `session_id` | yes | Active session owned by the signer |
| `request_id` | optional | Defaults to the JSON-RPC ID; persisted as the turn idempotency key |
| `diff` | initial inline mode | Unified diff; a new inline review must be non-empty |
| `changed_files` | optional | Compatibility hint; the server derives the authoritative changed-file set from the filtered diff |
| `chat_id` | continuation | Opaque ID returned by the initial inline response |
| `expected_version` | continuation | Non-negative version returned by the preceding response |
| `message` | continuation | Non-empty follow-up instruction |
| `patch_event_id` | stored-patch mode | Stored NIP-34 patch/PR |
| `force` | optional | Authorized root-status bypass in stored-patch mode |

Initial inline review responses include `chat_id` and `expected_version`. A continuation reuses the same `review/request` method with those fields plus a new `request_id` and `message`; it does not resend the diff. Exact duplicate turns replay their stored response, while a reused request ID with different content or a stale/future version returns conflict before model execution. Broken and expired chats are rejected before snapshot restore or model execution.

Inline filesystem review also requires the session's absolute `workspace_path` to be an exact canonical root assigned to the signer's lowercase hex pubkey by `DRYDOCK_IDE_WORKSPACE_BINDINGS`. An unauthorized workspace is cleared from the session and cannot be rebound later under the same session ID.

## Marketplace Feedback Notification

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "method": "marketplace/feedback",
    "params": {
      "review_event_id": "<completed-review-event-id>",
      "rating": 5,
      "helpful": true,
      "accurate": true,
      "comment": "Useful review"
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["e", "<completed-review-event-id>"],
    ["method", "marketplace/feedback"],
    ["expiration", "1714003200"]
  ]
}
```

The notification must omit `id`; `method` and `e` tags must match the payload. Expiration is optional, but when present it must be current and within the 15-minute lifetime. Comments are limited to 4096 bytes and ratings to 1–5.

Drydock derives the rater from the authenticated sender and the reviewer from the completed assignment. Only the assignment requester may rate it. Feedback is rate-limited per sender. The first valid write wins; later deliveries or competing feedback events are idempotent and do not recalculate reputation twice. Invalid or unauthorized notifications are handled without a response, while transient storage failures remain retryable by relay redelivery.

This notification is the clean replacement for NIP-90 kind `7000`. Kind `7000` is ignored.

## Security Audit Progress Notification

```json
{
  "jsonrpc": "2.0",
  "method": "security/audit/progress",
  "params": {
    "audit_id": 42,
    "request_event_id": "<security-audit-request-event-id>",
    "phase": "published",
    "status": "success",
    "occurred_at": 1714003000
  }
}
```

Drydock addresses the notification to the audit requester and adds an `e` tag for the original request. Status is `processing`, `success`, or `error`; success and error are terminal. Consumers deduplicate by event ID, never regress a terminal state to processing, and order otherwise by `occurred_at` then event ID. Progress delivery is advisory: durable audit state and SARIF remain authoritative.

## Errors

| Code | Name | Meaning |
|------|------|---------|
| -32700 | Parse error | Invalid JSON |
| -32600 | Invalid request | Malformed envelope or request missing an ID |
| -32601 | Method not found | Unsupported request method |
| -32602 | Invalid params | Invalid fields or target consistency |
| -32603 | Internal error | Transient/internal processing failure |
| -32001 | Unauthorized | Static ceiling, sender, or force authorization failed |
| -32002 | Not found | Patch, repository, session, assignment, or artifact missing |
| -32003 | Conflict | Immutable idempotency or active-target conflict |
| -32004 | Expired | Request expired |
| -32005 | Rate limited | Sender exceeded its configured window |
| -32006 | Payment required | Payment preflight denied; data includes safe reason/retryability |

## Subscription and Privacy Guidance

Subscribe by recipient and method:

```json
{"kinds":[25910],"#p":["<my-pubkey>"],"#method":["review/order","marketplace/feedback","security/audit/progress"]}
```

Gift-wrap subscriptions are recipient-scoped:

```json
{"kinds":[1059],"#p":["<my-pubkey>"]}
```

Use gift wrapping for source, diagnostics, assignments, payment-sensitive metadata, and private security results. Keep only recipient routing on the outer wrapper. Use Nostr event IDs for deduplication, JSON-RPC IDs for request correlation, and persistent subscriptions rather than polling.
