# Drydock VS Code Extension

AI-powered code review via Nostr — review uncommitted changes, see inline diagnostics, and apply fixes with one click.

## Features

- **Review Uncommitted Changes**: Get instant AI feedback on your uncommitted code changes
- **Inline Diagnostics**: See findings directly in your editor as warnings/errors
- **One-Click Fixes**: Apply suggested fixes with a single click
- **Decentralized**: Communicates with Drydock via Nostr — no centralized API

## Requirements

- VS Code 1.80.0 or later
- Git repository
- Nostr private key (for authentication)
- Access to Nostr relays

## Configuration

- `drydock.relays`: Nostr relays to connect to (default: `["wss://relay.damus.io", "wss://nos.lol"]`)
- `drydock.privateKey`: Your Nostr private key (nsec or hex)
- `drydock.drydockPubkey`: Drydock service public key
- `drydock.autoReview`: Automatically review on save (default: false)

## Commands

- **Drydock: Review Uncommitted Changes** - Review your current uncommitted changes
- **Drydock: Apply Suggested Fix** - Apply a fix from the diagnostics
- **Drydock: Clear Diagnostics** - Clear all Drydock diagnostics

## Protocol

This extension communicates with Drydock using Nostr-native event kinds:

| Kind | Description |
|------|-------------|
| 30078 | IDE workspace session announcement (NIP-78 app data, `d=drydock:ide-session:<session-id>`) |
| 25910 | ContextVM JSON-RPC review requests, fix requests, and responses |

Deprecated project-specific kinds `31650` and `1651`-`1654` are no longer used. Session state now uses kind `30078`; review and fix request/response traffic now uses ContextVM JSON-RPC in kind `25910`.

### Session announcement

```json
{
  "kind": 30078,
  "content": {
    "session_id": "session-uuid",
    "workspace": "example-repo",
    "client": "vscode-drydock/0.1.0"
  },
  "tags": [
    ["d", "drydock:ide-session:session-uuid"],
    ["p", "<drydock-pubkey>"],
    ["t", "drydock"],
    ["client", "vscode-drydock", "0.1.0"]
  ]
}
```

### Review request

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
      "content": "<file or selection content>",
      "selection": {"start": 10, "end": 25},
      "trigger": "manual"
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["a", "30078:<ide-pubkey>:drydock:ide-session:session-uuid"],
    ["t", "drydock"],
    ["t", "review"],
    ["method", "review/request"]
  ]
}
```

### Apply-fix request

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "fix-01HZX...",
    "method": "review/apply-fix",
    "params": {
      "session_id": "session-uuid",
      "fix_id": "fix-123",
      "file": "src/auth.go",
      "diagnostic_id": "finding-456"
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["a", "30078:<ide-pubkey>:drydock:ide-session:session-uuid"],
    ["t", "drydock"],
    ["t", "review"],
    ["method", "review/apply-fix"]
  ]
}
```

Drydock responses are also kind `25910` ContextVM events containing a JSON-RPC `result` or `error`, tagged with `p` for the requester and `e` for the request event ID.

Private payloads, including source snippets, diagnostics, and fix requests, should be transported with NIP-59 gift-wrap. In that case the visible outer event is kind `1059` and the wrapped inner event is kind `25910`.

See [`docs/ide-integration.md`](../../docs/ide-integration.md) for full protocol details.

## Development

```bash
npm install
npm run compile
# Press F5 in VS Code to run the extension in development mode
```

## License

MIT
