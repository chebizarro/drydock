# IDE Integration

Drydock provides real-time code review diagnostics directly in your IDE through a Nostr-native protocol. Developers get instant feedback as they edit, with actionable inline diagnostics and one-click fixes.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                           Developer IDE                              │
│  ┌───────────────────────────────────────────────────────────────┐  │
│  │                    VS Code / Neovim                           │  │
│  │  ┌─────────────┐  ┌─────────────────┐  ┌──────────────────┐  │  │
│  │  │  Editor     │  │  Diagnostics    │  │  Code Actions    │  │  │
│  │  │  (source)   │  │  (squiggles)    │  │  (quick fixes)   │  │  │
│  │  └─────────────┘  └─────────────────┘  └──────────────────┘  │  │
│  │                            ▲                    ▲             │  │
│  │                            │                    │             │  │
│  │  ┌─────────────────────────┴────────────────────┴──────────┐ │  │
│  │  │              Drydock IDE Extension                       │ │  │
│  │  │  • Session state (kind 30078 / NIP-78)                  │ │  │
│  │  │  • Review RPC (kind 25910 / ContextVM)                  │ │  │
│  │  │  • Fix RPC (kind 25910 / ContextVM)                     │ │  │
│  │  └────────────────────────┬────────────────────────────────┘ │  │
│  └───────────────────────────┼────────────────────────────────────┘  │
└──────────────────────────────┼──────────────────────────────────────┘
                               │ Nostr + NIP-59 encryption
                               ▼
┌─────────────────────────────────────────────────────────────────────┐
│                        Drydock Server                                │
│  ┌────────────────┐  ┌────────────────┐  ┌─────────────────────┐   │
│  │  IDE Gateway   │  │  Review Engine │  │  Auto-Fix Generator │   │
│  │  (handler.go)  │──▶│  (LLM review)  │──▶│  (patch creation)  │   │
│  └────────────────┘  └────────────────┘  └─────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

## Nostr Event Kinds

| Kind | Name | Direction | Description |
|------|------|-----------|-------------|
| 30078 | IDE Session | IDE → Relay | NIP-78 application data for replaceable workspace session state |
| 25910 | ContextVM JSON-RPC | IDE ↔ Server | Review requests/responses and fix requests/responses |
| 1059 | NIP-59 Gift Wrap | IDE ↔ Server | Encrypted envelope for private session, review, and fix payloads |

Deprecated mappings: `31650` is replaced by `30078`; `1651`, `1652`, `1653`, and `1654` are replaced by ContextVM JSON-RPC messages on kind `25910`.

## Protocol Flow

### 1. Session Establishment

When the IDE extension activates, it publishes replaceable NIP-78 application data:

```json
{
  "kind": 30078,
  "content": {
    "session_id": "session-uuid",
    "workspace_path": "/path/to/project",
    "repo_id": "<owner-pubkey>:project",
    "editor": "vscode",
    "version": "0.1.0",
    "languages": ["go"]
  },
  "tags": [
    ["d", "drydock:ide-session:session-uuid"],
    ["p", "<drydock-pubkey>"],
    ["type", "ide-session"],
    ["schema", "drydock.ide-session.v1"],
    ["client", "vscode-drydock/0.1.0"]
  ]
}
```

If the session data contains private repository information, publish it inside a NIP-59 gift-wrap addressed to Drydock. For inline filesystem review, `workspace_path` must be absolute and must resolve to an exact canonical root assigned to the session author's lowercase hex pubkey in `DRYDOCK_IDE_WORKSPACE_BINDINGS`. Unauthorized paths are cleared, and an established session cannot later rebind to another workspace.

### 2. Review Request

When the user saves a file or triggers manual review, the IDE sends a ContextVM JSON-RPC request on kind `25910`:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "review-01HZX...",
    "method": "review/request",
    "params": {
      "session_id": "session-uuid",
      "request_id": "review-01HZX...",
      "diff": "--- a/src/auth.go\n+++ b/src/auth.go\n@@ -15 +15 @@\n-old\n+new",
      "changed_files": ["src/auth.go"],
      "full_review": true
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["session", "session-uuid"],
    ["request", "review-01HZX..."],
    ["method", "review/request"]
  ]
}
```

### 3. Review Response

Drydock responds on kind `25910` with the same JSON-RPC `id`. Responses are routed by `p` and correlated by the request-event `e` tag; they do not carry a `method` tag:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "review-01HZX...",
    "result": {
      "diagnostics": [
        {
          "file": "src/auth.go",
          "range": {"start_line": 14, "start_column": 4, "end_line": 14, "end_column": 20},
          "severity": 1,
          "message": "Password compared in constant time to prevent timing attacks",
          "source": "drydock",
          "has_fix": true,
          "fix_id": "fix-timing-attack-001"
        }
      ],
      "summary": "Found one issue.",
      "review_time_ms": 4210,
      "chat_id": "9f4c...32-hex-characters...",
      "expected_version": 0
    }
  },
  "tags": [
    ["e", "<request-event-id>"],
    ["p", "<user-pubkey>"],
    ["session", "session-uuid"],
    ["request", "review-01HZX..."]
  ]
}
```

### 4. Continue a Review

The response's `chat_id` identifies the persisted review session and `expected_version` is the version the next turn must echo. A follow-up uses the same `review/request` method with a new request/JSON-RPC ID:

```json
{
  "session_id": "session-uuid",
  "request_id": "review-follow-up-01",
  "chat_id": "9f4c...32-hex-characters...",
  "expected_version": 0,
  "message": "Check the error path in the changed function."
}
```

Do not resend the diff. Exact duplicate turns replay the stored response. Reusing a request ID with different content, or sending a stale/future version, returns conflict without running the model. Broken or expired sessions must be replaced with a new initial review. Initial reviews and continuations share the `DRYDOCK_IDE_AGENTIC_TIMEOUT` deadline (`10m` by default).

### 5. Fix Request & Response

User clicks "Quick Fix" in the IDE:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "fix-01HZY...",
    "method": "review/apply-fix",
    "params": {
      "session_id": "session-uuid",
      "request_id": "fix-01HZY...",
      "fix_id": "fix-timing-attack-001",
      "file": "src/auth.go"
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["session", "session-uuid"],
    ["request", "fix-01HZY..."],
    ["method", "review/apply-fix"]
  ]
}
```

Fix IDs are deterministic for the persisted `(chat_id, version, finding)` turn, so a later continuation produces distinct IDs even for a finding at the same location. Suggested fixes expire after 15 minutes.

Drydock replies:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "fix-01HZY...",
    "result": {
      "fix_id": "fix-timing-attack-001",
      "patch": "--- a/src/auth.go\n+++ b/src/auth.go\n@@ -15 +15 @@\n-  if password == storedHash {\n+  if subtle.ConstantTimeCompare([]byte(password), []byte(storedHash)) == 1 {"
    }
  },
  "tags": [
    ["p", "<user-pubkey>"],
    ["e", "<fix-request-event-id>"],
    ["session", "session-uuid"],
    ["request", "fix-01HZY..."],
    ["fix", "fix-timing-attack-001"]
  ]
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
# From VS Code marketplace
code --install-extension drydock.vscode-drydock

# Or run from source
cd extensions/vscode-drydock
npm install
npm run compile
# Press F5 in VS Code to launch the Extension Development Host
```

### Extension Settings

```json
{
  "drydock.drydockPubkey": "npub1drydock...",
  "drydock.relays": ["wss://trusted-relay.example"],
  "drydock.autoReview": false
}
```

### Features

- **Review Diagnostics**: Squiggly underlines appear after a manual review, or after save when `drydock.autoReview` is enabled
- **Hover Information**: VS Code shows diagnostic explanations on hover
- **Review Continuations**: Run **Drydock: Continue Review** to send a follow-up against the frozen snapshot using `chat_id`/`expected_version`
- **One-Click Fixes**: Apply the turn-scoped suggested fix returned for a diagnostic
- **Problems Panel**: All diagnostics appear in VS Code's Problems panel

## Server Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DRYDOCK_IDE_AGENTIC_TIMEOUT` | `10m` | End-to-end deadline for an initial inline agentic review or continuation |
| `DRYDOCK_IDE_WORKSPACE_BINDINGS` | *(empty)* | Comma-separated `lowercase-64-hex-pubkey=/absolute/workspace` bindings; repeat a pubkey to approve multiple exact roots. Empty disables inline filesystem review. |

Example:

```bash
DRYDOCK_IDE_AGENTIC_TIMEOUT=10m
DRYDOCK_IDE_WORKSPACE_BINDINGS='0123...64-hex...cdef=/srv/workspaces/project'
```

## Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `drydock_ide_sessions_active` | Gauge | Currently active IDE sessions |
| `drydock_ide_review_requests_received_total` | Counter | Review requests received |
| `drydock_ide_review_responses_sent_total` | Counter | Review responses sent |
| `drydock_ide_review_errors_total` | Counter | Review processing errors |
| `drydock_ide_fix_requests_received_total` | Counter | Fix requests received |
| `drydock_ide_fix_responses_sent_total` | Counter | Fix responses sent |

## Integration Tests

Integration suites are gated behind the `integration` build tag.

```bash
# Full pipeline integration tests
go test -tags=integration ./internal/pipeline/...

# IDE gateway review→fix integration tests
go test -tags=integration ./internal/idegateway/...
```

## Security

1. **Encryption**: Private IDE ↔ Server payloads use NIP-59 gift-wrap; the inner event carries the kind `25910` ContextVM request or response.
2. **Session Isolation**: Each session is owned by the signing pubkey; continuations also bind `chat_id`, optimistic version, and idempotent request ID to that owner.
3. **Workspace Authorization**: Inline review can freeze only an operator-approved exact canonical root for that pubkey; symlink aliases and session workspace rebinding are rejected.
4. **Snapshot Isolation**: Continuations read the immutable copied snapshot, not later live-workspace mutations. Broken/expired sessions short-circuit before model execution.
5. **Code Privacy**: Source code should be transmitted encrypted and is processed locally.
6. **Operator-Controlled Inference**: Drydock sends code only to the OpenAI-compatible endpoints the operator configured; use local endpoints when code must remain on operator infrastructure.

## Troubleshooting

### No diagnostics appearing
1. Verify `drydock.drydockPubkey` and at least one trusted `drydock.relays` entry
2. Verify relay connectivity in the output panel
3. Confirm the extension's pubkey and workspace are present in `DRYDOCK_IDE_WORKSPACE_BINDINGS`
4. Check the Drydock logs for an unauthorized workspace or session error

### Slow diagnostics
1. Check server-side discovery/reviewer latency and agentic budget metrics
2. Consider disabling `drydock.autoReview` and triggering reviews manually
3. Compare end-to-end duration with `DRYDOCK_IDE_AGENTIC_TIMEOUT`

### Connection issues
1. Verify relays are reachable
2. Check server pubkey is correct
3. Look for errors in extension output channel
