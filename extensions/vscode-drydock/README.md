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
- Nostr private key (stored with VS Code SecretStorage via the command below)
- Access to trusted/private Nostr relays

## Configuration

- `drydock.relays`: Trusted/private Nostr relays to connect to (default: `[]`; publishing source diffs to likely-public relays requires confirmation)
- Private key: run **Drydock: Store Nostr Private Key** to store your nsec/hex key in VS Code SecretStorage
- `drydock.drydockPubkey`: Drydock service public key
- `drydock.autoReview`: Automatically review on save (default: false)

## Commands

- **Drydock: Review Uncommitted Changes** - Review your current uncommitted changes
- **Drydock: Apply Suggested Fix** - Apply a fix from the diagnostics
- **Drydock: Clear Diagnostics** - Clear all Drydock diagnostics
- **Drydock: Store Nostr Private Key** - Store or clear your Nostr private key securely

## Protocol

This extension communicates with Drydock using custom Nostr event kinds:

| Kind | Description |
|------|-------------|
| 31650 | IDE workspace session announcement |
| 1651 | Review request (uncommitted diff) |
| 1652 | Review response (diagnostics) |
| 1653 | Fix apply request |
| 1654 | Fix apply response |

## Development

```bash
npm install
npm run compile
# Press F5 in VS Code to run the extension in development mode
```

## License

MIT
