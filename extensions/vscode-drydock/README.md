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

Review and fix requests are kind 25910 events whose content is a JSON-RPC 2.0 request:

```json
{"jsonrpc":"2.0","id":"<uuid>","method":"review/request","params":{"session_id":"<session-id>","request_id":"<uuid>","diff":"<unified diff>","changed_files":["path/to/file"],"full_review":true}}
```

Fix requests use method `review/apply-fix` with `session_id`, `request_id`, `fix_id`, and `file` params. Drydock responses are also kind 25910 ContextVM events containing a JSON-RPC `result` or `error`.

## Development

```bash
npm install
npm run compile
# Press F5 in VS Code to run the extension in development mode
```

## License

MIT
