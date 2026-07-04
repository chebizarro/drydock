# IDE Integration

Drydock provides code review diagnostics directly in the IDE through Nostr-native events. IDEs publish workspace session state and send review/fix commands as ContextVM JSON-RPC messages.

## Architecture

```
Developer IDE
  └─ Drydock IDE Extension
       • Session state: kind 30078 addressable event
       • Review command: kind 25910 ContextVM JSON-RPC method ide/review
       • Fix command: kind 25910 ContextVM JSON-RPC method ide/applyFix
            │
            ▼
        Nostr relays
            │
            ▼
Drydock Server
  └─ IDE Gateway → Context Builder → Review Engine → Suggested-fix store
```

## Nostr Event Kinds

| Kind | Name | Direction | Description |
|------|------|-----------|-------------|
| 30078 | IDE Session State | IDE → Server | Addressable workspace session state with `d=<session-id>` |
| 25910 | ContextVM Command | IDE ↔ Server | JSON-RPC request/response for IDE review and fix methods |
| 1059 | NIP-59 Gift Wrap | IDE ↔ Server | Optional encrypted wrapper for private ContextVM payloads |

Deprecated IDE-specific kinds `31650` and `1651`–`1654` are historical only and must not be used by live integrations.

## ContextVM Methods

| Method | Direction | Params | Result |
|--------|-----------|--------|--------|
| `ide/review` | IDE → Server | `ReviewRequest` | `ReviewResponse` diagnostics |
| `ide/applyFix` | IDE → Server | `FixRequest` | `FixResponse` with suggested diff |

Responses are kind `25910` JSON-RPC response envelopes and are correlated to requests by the JSON-RPC `id`. Events also carry `p`, `session`, `request`, `method`, and `t=drydock-ide` tags for routing and filtering.

## Protocol Flow

### 1. Session Establishment

The IDE announces session state as an addressable kind `30078` event:

```json
{
  "kind": 30078,
  "content": {
    "session_id": "session-uuid",
    "workspace_path": "/path/to/project",
    "repo_id": "<repo-id>",
    "repo_ref": "repo:<repo-id>",
    "editor": "vscode",
    "version": "1.0.0",
    "languages": ["go", "typescript"]
  },
  "tags": [
    ["d", "session-uuid"],
    ["p", "<drydock-pubkey>"],
    ["t", "drydock-ide"]
  ]
}
```

### 2. Review Request

When the user triggers review, the IDE publishes a kind `25910` ContextVM JSON-RPC request:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "review-request-uuid",
    "method": "ide/review",
    "params": {
      "session_id": "session-uuid",
      "diff": "diff --git ...",
      "changed_files": ["src/auth.go"],
      "full_review": true
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["session", "session-uuid"],
    ["request", "review-request-uuid"],
    ["method", "ide/review"],
    ["t", "drydock-ide"]
  ]
}
```

### 3. Review Response

Drydock responds with a kind `25910` JSON-RPC result using the same `id`:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "review-request-uuid",
    "result": {
      "request_id": "review-request-uuid",
      "session_id": "session-uuid",
      "diagnostics": [
        {
          "file": "src/auth.go",
          "range": {"start_line": 15, "start_column": 4, "end_line": 15, "end_column": 20},
          "severity": 1,
          "message": "Password comparison should avoid timing leaks",
          "source": "drydock",
          "has_fix": true,
          "fix_id": "fix-timing-attack-001",
          "suggested_fix": "@@ ..."
        }
      ],
      "summary": "Found one issue.",
      "review_time_ms": 1234
    }
  },
  "tags": [
    ["e", "<request-event-id>"],
    ["p", "<ide-pubkey>"],
    ["session", "session-uuid"],
    ["request", "review-request-uuid"],
    ["method", "ide/review"],
    ["t", "drydock-ide"]
  ]
}
```

### 4. Fix Request & Response

When the user applies a fix, the IDE asks Drydock to retrieve the stored suggested fix:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "fix-request-uuid",
    "method": "ide/applyFix",
    "params": {
      "session_id": "session-uuid",
      "fix_id": "fix-timing-attack-001",
      "file": "src/auth.go"
    }
  }
}
```

Drydock responds:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "fix-request-uuid",
    "result": {
      "request_id": "fix-request-uuid",
      "session_id": "session-uuid",
      "success": true,
      "diff": "@@ ..."
    }
  }
}
```

## Diagnostic Severity Levels

| Level | LSP Mapping | Description |
|-------|-------------|-------------|
| 1 | Error | Critical security issues, broken code |
| 2 | Warning | Best practice violations, potential bugs |
| 3 | Information | Suggestions, style improvements |
| 4 | Hint | Low-priority recommendations |

## VS Code Extension

### Installation

```bash
cd extensions/vscode-drydock
npm install && npm run compile
```

### Extension Settings

```json
{
  "drydock.relays": ["wss://relay.example"],
  "drydock.drydockPubkey": "npub1drydock...",
  "drydock.autoReview": false
}
```

The private key is stored in VS Code SecretStorage via **Drydock: Store Nostr Private Key**.

## Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `drydock_ide_sessions_active` | Gauge | Currently active IDE sessions |
| `drydock_ide_review_requests_received_total` | Counter | Review requests received |
| `drydock_ide_review_responses_sent_total` | Counter | Review responses sent |
| `drydock_ide_review_errors_total` | Counter | Review processing errors |
| `drydock_ide_fix_requests_received_total` | Counter | Fix requests received |
| `drydock_ide_fix_responses_sent_total` | Counter | Fix responses sent |

## Security and Privacy

- Events are signed and verified before gateway processing.
- Session and command events must be addressed to the Drydock gateway with a matching `p` tag.
- Review requests contain uncommitted diffs. The current VS Code extension publishes kind `25910` payloads directly and warns before using likely-public relays. NIP-59 gift-wrap support exists on the server/listener path (`1059` → inner `25910`) and should be used by clients that support encrypting private ContextVM payloads.
- Fixes are stored server-side with session/author checks and expire after a short TTL.

## Troubleshooting

### No diagnostics appearing
1. Verify `drydock.drydockPubkey` is configured.
2. Verify trusted relay connectivity.
3. Confirm the session state event is published as kind `30078` and commands as kind `25910`.
4. Check Drydock logs for signature, session, or recipient validation failures.

### Connection issues
1. Verify relays are reachable.
2. Check server pubkey is correct.
3. Look for errors in the extension host console.
