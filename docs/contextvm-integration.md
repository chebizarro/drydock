# ContextVM Integration

Drydock uses ContextVM as its application command protocol over Nostr. ContextVM messages are JSON-RPC 2.0 envelopes carried in Nostr kind `25910` events. Nostr provides relay delivery, signatures, identity, and routing tags; ContextVM provides method names, request IDs, responses, and errors.

## Event Kind

| Kind | Name | Purpose |
|------|------|---------|
| 25910 | ContextVM JSON-RPC | Application commands for review, fixes, and marketplace coordination |

Private ContextVM payloads should be transported through NIP-59 gift-wrap. In that case, the visible outer event is kind `1059` and the wrapped inner event is kind `25910`.

## Supported Methods

| Method | Direction | Purpose |
|--------|-----------|---------|
| `review/request` | IDE or requester → Drydock | Request diagnostics for a file, selection, patch, or review target |
| `review/apply-fix` | IDE or requester → Drydock | Request an auto-fix patch for a diagnostic or finding |
| `marketplace/assign` | Drydock → Reviewer | Offer a review assignment |
| `marketplace/accept` | Reviewer → Drydock | Accept a review assignment |
| `marketplace/reject` | Reviewer → Drydock | Reject a review assignment with a reason |

## JSON-RPC Request Format

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
    ["p", "<recipient-pubkey>"],
    ["t", "drydock"],
    ["method", "review/request"]
  ]
}
```

Rules:

- `content.jsonrpc` MUST be `"2.0"`.
- `content.id` MUST be present for requests that expect a response.
- `content.method` MUST be one of the supported methods.
- `content.params` MUST be an object.
- The event SHOULD include a `p` tag for the recipient and a `method` tag for subscription routing.

## JSON-RPC Response Format

Successful responses use `result` and the same `id` as the request:

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

Error responses use `error` and the same `id` when available:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "req-01HZX...",
    "error": {
      "code": -32602,
      "message": "Invalid params",
      "data": {
        "field": "file",
        "reason": "required"
      }
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

## Method Parameters

### `review/request`

Common params:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `session_id` | string | optional | IDE or requester session ID |
| `file` | string | optional | Relative file path under review |
| `content` | string | optional | File or selection content when needed |
| `selection` | object | optional | Source range for selection-based review |
| `patch_event_id` | string | optional | NIP-34 patch or PR event ID |
| `trigger` | string | optional | `save`, `manual`, `patch`, or other caller trigger |

### `review/apply-fix`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `session_id` | string | optional | IDE or requester session ID |
| `fix_id` | string | required | Fix identifier returned by review diagnostics |
| `file` | string | optional | Relative file path to patch |
| `diagnostic_id` | string | optional | Diagnostic or finding identifier |

### `marketplace/assign`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `assignment_id` | string | required | Stable assignment identifier |
| `patch_event_id` | string | required | Patch or PR event to review |
| `repo_id` | string | required | Repository identifier |
| `languages` | string[] | optional | Languages detected in the patch |
| `domains` | string[] | optional | Requested review domains |
| `price_sats` | integer | optional | Offered price in sats |
| `deadline` | integer | optional | Unix timestamp deadline |

### `marketplace/accept`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `assignment_id` | string | required | Assignment being accepted |
| `estimated_time` | string | optional | Reviewer's estimated turnaround |

### `marketplace/reject`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `assignment_id` | string | required | Assignment being rejected |
| `reason` | string | optional | Human-readable rejection reason |

## Error Codes

Drydock uses JSON-RPC standard codes plus application-specific negative codes.

| Code | Name | Description |
|------|------|-------------|
| -32700 | Parse error | `content` is not valid JSON |
| -32600 | Invalid request | JSON-RPC envelope is malformed |
| -32601 | Method not found | Unsupported ContextVM method |
| -32602 | Invalid params | Missing or invalid method parameters |
| -32603 | Internal error | Drydock failed while processing the request |
| -32001 | Unauthorized | Sender is not allowed to invoke the method |
| -32002 | Not found | Referenced session, fix, assignment, or patch was not found |
| -32003 | Conflict | Request conflicts with current assignment or session state |
| -32004 | Expired | Request or assignment has expired |
| -32005 | Rate limited | Sender exceeded allowed request rate |

## Tag Conventions

| Tag | Purpose |
|-----|---------|
| `p` | Recipient pubkey for routing |
| `e` | Related request, assignment, patch, or response event |
| `a` | Addressable session, repository, or profile reference |
| `t` | Topic tags such as `drydock`, `review`, or `marketplace` |
| `method` | ContextVM method name for efficient subscriptions |
| `expiration` | Unix timestamp after which relays may discard the event |

## Subscription Guidance

Participants subscribe by recipient and method, for example:

```json
{"kinds": [25910], "#p": ["<my-pubkey>"], "#method": ["review/request"]}
```

Use event IDs for deduplication and JSON-RPC IDs for command correlation. Do not poll relays for responses; keep subscriptions open and react to matching events.
