# Codebase Chat

Drydock provides codebase Q&A through Nostr encrypted direct messages (DMs). Users can ask questions about repositories by sending DMs to the Drydock bot, receiving AI-powered answers with relevant code context.

## How It Works

```
┌─────────────────┐                    ┌─────────────────────────────┐
│     User        │  Gift wrap (1059)  │        Drydock              │
│  (Nostr Client) │ ─────────────────► │                             │
└─────────────────┘                    │  ┌─────────────────────┐   │
                                       │  │   DM Handler        │   │
                                       │  │  (NIP-17/NIP-59)     │   │
                                       │  └──────────┬──────────┘   │
                                       │             │              │
                                       │             ▼              │
                                       │  ┌─────────────────────┐   │
                                       │  │   Code Indexer      │   │
                                       │  │  (tree-sitter +     │   │
                                       │  │   embeddings)       │   │
                                       │  └──────────┬──────────┘   │
                                       │             │              │
                                       │             ▼              │
                                       │  ┌─────────────────────┐   │
                                       │  │   LLM (RAG-enhanced)│   │
                                       │  └──────────┬──────────┘   │
                                       │             │              │
┌─────────────────┐   DM response      │             ▼              │
│     User        │ ◄───────────────── │  ┌─────────────────────┐   │
│  (Nostr Client) │                    │  │   Response Publisher│   │
└─────────────────┘                    │  └─────────────────────┘   │
                                       └─────────────────────────────┘
```

1. **Receive DM**: User sends a NIP-17 direct message inside a NIP-59 kind-1059 gift wrap
2. **Open & Parse**: The listener verifies and decrypts the gift wrap and seal, then passes the plaintext kind-14 rumor to the handler
3. **Index Repository**: If new repo, clone and index with tree-sitter + embeddings
4. **RAG Retrieval**: Find relevant code snippets based on the question
5. **LLM Response**: Generate answer with code context
6. **Encrypt & Reply**: Send encrypted DM response back to user

## Protocol

### Supported Event Kinds

| Kind | NIP | Description |
|------|-----|-------------|
| 13 | NIP-17/NIP-59 | Sender-signed, NIP-44-encrypted seal inside the gift wrap |
| 14 | NIP-17 | Plaintext unsigned direct-message rumor inside the seal |
| 1059 | NIP-59 | Ephemerally signed NIP-44 gift wrap published to relays |

### Message Format

Users can send natural language questions. To specify a repository:

```
@repo:github.com/user/project

How does the authentication flow work?
```

Or reference it inline:

```
In the drydock repository, explain the context builder's priority layers.
```

Once a repository context is established, follow-up questions maintain it:

```
User: @repo:github.com/example/app What frameworks does this use?
Drydock: This project uses React with TypeScript, Express.js backend...

User: How is state management handled?
Drydock: (continues with same repo context) State is managed using Redux...
```

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DRYDOCK_CODECHAT_RATE_LIMIT_REQUESTS` | `20` | Maximum requests per sender in the configured window |
| `DRYDOCK_CODECHAT_RATE_LIMIT_WINDOW` | `1h` | Per-sender rate-limit window |

Codebase chat is registered automatically when Drydock has a signer with encrypt/decrypt support plus configured Qdrant and embedding clients. There is no separate enable flag. Conversation length is currently a code constant of 10 turns, the prompt loads the latest 5 stored turns, and repository availability comes from the indexed `code_chunks` collection rather than a codechat-specific allowlist.

### Keyer Configuration

The handler uses a `nostr.Keyer` interface to sign the seal and perform the NIP-44 encryption required by NIP-59:

```go
type Keyer interface {
    GetPublicKey(ctx context.Context) (nostr.PubKey, error)
    SignEvent(ctx context.Context, evt *nostr.Event) error
    Encrypt(ctx context.Context, plaintext string, recipient nostr.PubKey) (string, error)
    Decrypt(ctx context.Context, ciphertext string, sender nostr.PubKey) (string, error)
}
```

Drydock supports multiple signer backends:
- **Bunker**: NIP-46 remote signing with encryption support
- **Local nsec**: Direct private key (development only)
- **Socket**: Unix socket-based signer

## Database Schema

Conversation turns are persisted for context:

```sql
CREATE TABLE codechat_turns (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sender_pubkey TEXT NOT NULL,
  event_id TEXT NOT NULL UNIQUE,
  repo_id TEXT NOT NULL DEFAULT '',
  question TEXT NOT NULL,
  response TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  created_at INTEGER NOT NULL
);
```

## Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `drydock_codechat_dms_received_total` | Counter | DMs received for codebase chat |
| `drydock_codechat_responses_sent_total` | Counter | Responses published |
| `drydock_codechat_rate_limited_total` | Counter | Requests dropped (rate limit) |
| `drydock_codechat_errors_total` | Counter | Processing errors |

## Security Considerations

1. **Encryption**: DMs use the complete NIP-17/NIP-59 rumor, seal, and gift-wrap construction; only the outer kind-1059 wrapper is published
2. **Private Key Handling**: Use bunker signing; never expose nsec in production
3. **Repository Access**: Chat retrieves only repositories already present in the operator's `code_chunks` index; normal repository admission and indexing policy controls what becomes available
4. **Rate Limiting**: The SQLite-backed limiter denies on backend failure and enforces the configured per-sender request window
5. **Conversation Isolation**: Each user's history is separate

## Example Conversation

```
User (DM to npub1drydock...):
@repo:github.com/lightning/lnd
How does the payment lifecycle work?

Drydock (DM response):
The payment lifecycle in LND follows these stages:

1. **Invoice Creation** (`invoices/invoices.go`):
   ```go
   func (i *InvoiceRegistry) AddInvoice(invoice *Invoice) error
   ```

2. **HTLC Forwarding** (`htlcswitch/switch.go`):
   The switch forwards HTLCs through the channel graph...

3. **Settlement** (`contractcourt/chain_watcher.go`):
   Once the preimage is revealed...

Would you like me to explain any specific stage in more detail?
```

## Troubleshooting

### "Repository not found"
- Ensure the repository URL is correct and publicly accessible
- Confirm the repository has been reviewed/indexed and is present in the `code_chunks` collection

### "Rate limited"
- Users are limited to configured requests per hour/day
- Wait or contact the operator for increased limits

### No response received
- Check that Drydock has the user's pubkey (they must have sent a DM first)
- Verify relay connectivity
- Check logs for decryption errors
