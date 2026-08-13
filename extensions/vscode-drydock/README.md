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

- `drydock.relays`: Trusted Nostr relays to connect to (default: `[]`)
- `drydock.drydockPubkey`: Drydock service public key
- `drydock.autoReview`: Automatically review on save (default: false)

Store the client key with **Drydock: Store Nostr Private Key**. It is kept in VS Code secret storage rather than a plaintext setting.

## Commands

- **Drydock: Review Uncommitted Changes** - Start a review of current uncommitted changes
- **Drydock: Continue Review** - Send a follow-up against the latest review's frozen snapshot
- **Drydock: Apply Suggested Fix** - Apply a turn-scoped fix from the diagnostics
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
    "workspace_path": "/absolute/path/to/example-repo",
    "repo_id": "",
    "editor": "vscode",
    "version": "0.1.0",
    "languages": ["typescript"]
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
      "request_id": "req-01HZX...",
      "diff": "<unified git diff>",
      "changed_files": ["src/auth.go"],
      "full_review": true
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["session", "session-uuid"],
    ["request", "req-01HZX..."],
    ["method", "review/request"]
  ]
}
```

Initial review responses include an opaque `chat_id` and the next `expected_version`. **Drydock: Continue Review** sends another `review/request` with a new request ID, the stored `chat_id`, the last `expected_version`, and a non-empty `message`; it does not resend the diff. The extension advances its local version only from a correlated, signature-verified Drydock response.

The server applies a 10-minute default deadline to initial and continuation turns. Its operator must bind the extension pubkey to the exact canonical workspace root with `DRYDOCK_IDE_WORKSPACE_BINDINGS`; an unbound workspace cannot start inline filesystem review.

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
      "request_id": "fix-01HZX...",
      "fix_id": "fix-123",
      "file": "src/auth.go"
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["session", "session-uuid"],
    ["request", "fix-01HZX..."],
    ["method", "review/apply-fix"]
  ]
}
```

Drydock responses are also kind `25910` ContextVM events containing a JSON-RPC `result` or `error`. The extension accepts them only when the event signature/pubkey and the `p`, `e`, `session`, and `request` correlation tags match; fix responses additionally require the matching `fix` tag. Fix IDs are scoped to a persisted review turn, so a continuation may return a different ID for a finding at the same location.

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
