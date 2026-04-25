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
│  │  │  • Session management (kind 31650)                       │ │  │
│  │  │  • Review requests (kind 1651)                           │ │  │
│  │  │  • Fix applications (kind 1653)                          │ │  │
│  │  └────────────────────────┬────────────────────────────────┘ │  │
│  └───────────────────────────┼────────────────────────────────────┘  │
└──────────────────────────────┼──────────────────────────────────────┘
                               │ Nostr (encrypted)
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
| 31650 | IDE Session | IDE → Server | Establishes workspace session (replaceable) |
| 1651 | Review Request | IDE → Server | Request review for file/selection |
| 1652 | Review Response | Server → IDE | Diagnostics from review |
| 1653 | Fix Request | IDE → Server | Request auto-fix for a diagnostic |
| 1654 | Fix Response | Server → IDE | Generated fix patch |

## Protocol Flow

### 1. Session Establishment

When the IDE extension activates:

```json
{
  "kind": 31650,
  "content": {
    "workspace": "/path/to/project",
    "repo_url": "github.com/user/project",
    "capabilities": ["review", "fix", "explain"]
  },
  "tags": [
    ["d", "session-uuid"],
    ["client", "vscode-drydock/1.0.0"]
  ]
}
```

### 2. Review Request

When the user saves a file or triggers manual review:

```json
{
  "kind": 1651,
  "content": {
    "file": "src/auth.go",
    "content": "package auth\n\nfunc Login(user, pass string) {...}",
    "selection": {"start": 10, "end": 25},
    "trigger": "save"
  },
  "tags": [
    ["e", "<session-event-id>"],
    ["p", "<drydock-pubkey>"]
  ]
}
```

### 3. Review Response

Drydock responds with diagnostics:

```json
{
  "kind": 1652,
  "content": {
    "diagnostics": [
      {
        "file": "src/auth.go",
        "range": {"start": {"line": 15, "character": 4}, "end": {"line": 15, "character": 20}},
        "severity": 1,
        "message": "Password compared in constant time to prevent timing attacks",
        "source": "drydock",
        "has_fix": true,
        "fix_id": "fix-timing-attack-001"
      }
    ]
  },
  "tags": [
    ["e", "<request-event-id>"],
    ["p", "<user-pubkey>"]
  ]
}
```

### 4. Fix Request & Response

User clicks "Quick Fix" in the IDE:

```json
// Request (kind 1653)
{
  "content": {
    "fix_id": "fix-timing-attack-001",
    "file": "src/auth.go"
  }
}

// Response (kind 1654)
{
  "content": {
    "fix_id": "fix-timing-attack-001",
    "patch": "--- a/src/auth.go\n+++ b/src/auth.go\n@@ -15 +15 @@\n-  if password == storedHash {\n+  if subtle.ConstantTimeCompare([]byte(password), []byte(storedHash)) == 1 {"
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
# From VS Code marketplace
code --install-extension drydock.vscode-drydock

# Or build from source
cd extensions/vscode-drydock
npm install && npm run package
code --install-extension drydock-*.vsix
```

### Extension Settings

```json
{
  "drydock.enabled": true,
  "drydock.serverPubkey": "npub1drydock...",
  "drydock.relays": ["wss://relay.damus.io", "wss://nos.lol"],
  "drydock.reviewOnSave": true,
  "drydock.reviewDelay": 500,
  "drydock.showInlineHints": true
}
```

### Features

- **Real-time Diagnostics**: Squiggly underlines appear as you type
- **Hover Information**: Hover over issues to see full explanations
- **Quick Fixes**: Click the lightbulb or use `Cmd+.` to apply fixes
- **Problems Panel**: All diagnostics appear in VS Code's Problems panel
- **Status Bar**: Shows connection status and active session

## Neovim Integration

For Neovim users, integrate via the LSP client:

```lua
-- In your Neovim config
require('lspconfig').drydock.setup({
  cmd = {'drydock-lsp-bridge'},
  filetypes = {'go', 'python', 'typescript', 'rust'},
  settings = {
    drydock = {
      serverPubkey = 'npub1drydock...',
      relays = {'wss://relay.damus.io'}
    }
  }
})
```

## Server Configuration

### Environment Variables

```bash
# Enable IDE gateway
DRYDOCK_IDE_ENABLED=true

# Session timeout (inactive sessions are cleaned up)
DRYDOCK_IDE_SESSION_TIMEOUT=30m

# Maximum concurrent sessions per user
DRYDOCK_IDE_MAX_SESSIONS_PER_USER=3

# Review debounce (avoid overwhelming with rapid saves)
DRYDOCK_IDE_REVIEW_DEBOUNCE=2s
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

1. **Encryption**: All IDE ↔ Server communication uses NIP-04/NIP-44 encryption
2. **Session Isolation**: Each user's sessions are separate and authenticated
3. **Code Privacy**: Source code is transmitted encrypted, processed locally
4. **No Cloud**: All LLM inference runs on your infrastructure

## Troubleshooting

### No diagnostics appearing
1. Check extension is enabled: `drydock.enabled: true`
2. Verify relay connectivity in output panel
3. Ensure Drydock server has IDE gateway enabled
4. Check file type is supported

### Slow diagnostics
1. Increase `drydock.reviewDelay` setting
2. Check server-side LLM latency
3. Consider disabling `reviewOnSave` for large files

### Connection issues
1. Verify relays are reachable
2. Check server pubkey is correct
3. Look for errors in extension output channel
