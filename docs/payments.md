# Payments: NWC and Cashu Integration

> **Status**: Payment gating supports free access, daily quota, subscriptions, and Cashu tokens settled through NWC-created Lightning invoices.

## Motivation

Paid reviews create alignment between submitters and the review service:

- **Rate limiting**: Payment acts as a natural spam filter, preventing abuse from automated patch flooding
- **Sustainability**: Operators running GPU infrastructure can recoup costs
- **Quality signaling**: A willingness to pay indicates the submitter values a thorough review
- **Priority lanes**: Paid reviews can be prioritized over free-tier reviews

## Integration Point

Payment authorization runs in the review pipeline after repository configuration and the stored patch event are loaded. When `payments.enabled` is true, authorization checks existing authorization, configured free access, repository maintainership, subscriptions, attached Cashu payment, and daily free quota before review execution.

## Free Access

Free authorization is evaluated before subscription, Cashu, and daily-quota paths:

1. `payments.free_pubkeys` in the repository's `.drydock.yaml` grants unlimited reviews to listed patch authors. Entries may be npub or 64-character hex and are strictly validated and normalized to hex.
2. `DRYDOCK_FREE_PUBKEYS` grants the same access operator-wide across repositories.
3. The repository announcement owner and `maintainers` are free by default. Set `payments.free_for_maintainers: false` to disable this default.

Configured pubkeys use access kind `free_pubkey`; repository owners and maintainers use `free_maintainer`. These paths do not consume `free_reviews_per_day` quota.

```yaml
payments:
  enabled: true
  price_sats: 100
  free_pubkeys:
    - npub1...
  free_for_maintainers: true
```

```bash
DRYDOCK_FREE_PUBKEYS=npub1...,0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

## Option A: NWC (Nostr Wallet Connect, NIP-47)

### How It Works

1. The patch submitter includes a payment proof — either a zap receipt (kind 9735) referencing the patch event, or a dedicated payment tag
2. Drydock verifies the payment by checking:
   - The zap receipt references the patch event ID
   - The payment amount meets the minimum price
   - The receipt is signed by a trusted wallet service
3. If valid, the patch proceeds to review. If not, it is logged and dropped (or a rejection comment is published)

### Architecture

```
New package: internal/payment/nwc

NWCVerifier
  ├── VerifyPayment(ctx, event) (bool, error)
  │   ├── Look for zap receipt in related events
  │   ├── Verify receipt signature and amount
  │   └── Check against minimum price
  └── Config
      ├── NWCConnectionString (NIP-47 connection)
      └── MinAmountMsats
```

### Configuration

```bash
DRYDOCK_PAYMENT_MODE=nwc
DRYDOCK_NWC_CONNECTION_STRING=nostr+walletconnect://...
DRYDOCK_REVIEW_PRICE_MSATS=1000  # 1 sat minimum
```

### Pros and Cons

| ✅ Pros | ❌ Cons |
|---------|---------|
| Fully on-protocol (Nostr-native) | Requires submitter to have a Lightning wallet |
| Uses existing Nostr infrastructure | Payment confirmation latency |
| Zap receipts are publicly auditable | NWC connection management complexity |
| Works with any NIP-47 wallet | Depends on Lightning network availability |

## Option B: Cashu Ecash (NUT Protocol)

### How It Works

1. The patch submitter attaches a Cashu token in a dedicated tag on the patch event:
   ```json
   ["cashu", "cashuA...base64token..."]
   ```
2. Drydock extracts the token and redeems it against a configured mint
3. If redemption succeeds and the amount meets the minimum, the patch proceeds to review
4. If redemption fails (invalid, already spent, insufficient amount), the patch is dropped

### Architecture

```
New package: internal/payment/cashu

CashuVerifier
  ├── VerifyPayment(ctx, event) (bool, error)
  │   ├── Extract "cashu" tag from event
  │   ├── Decode token
  │   ├── Redeem against mint (HTTP POST to /v1/melt or /v1/swap)
  │   └── Check amount against minimum
  └── Config
      ├── MintURL
      └── MinAmountMsats
```

### Token Redemption Atomicity

The redemption must be atomic with the `BeginReview` database insert. If the token is redeemed but the review slot fails to acquire, the payment is lost. Wrapping both operations in a transaction:

```go
tx := store.BeginTx(ctx)
ok, err := cashuVerifier.Redeem(ctx, token)
if !ok { tx.Rollback(); return nil }
acquired, err := store.BeginReviewTx(tx, eventID, repoID)
if !acquired { tx.Rollback(); return nil }
tx.Commit()
```

### Configuration

```bash
DRYDOCK_PAYMENT_MODE=cashu
DRYDOCK_CASHU_MINT_URL=https://mint.example.com
DRYDOCK_REVIEW_PRICE_MSATS=1000
```

### Pros and Cons

| ✅ Pros | ❌ Cons |
|---------|---------|
| Privacy-preserving (ecash is bearer) | Requires trust in the mint operator |
| No wallet UX required on submitter side | Token expiry management |
| Offline-verifiable token structure | Double-spend prevention requires mint call |
| Low latency (single HTTP call) | Less ecosystem tooling than Lightning |

## Recommended Approach

**Start with Cashu** for the initial implementation:

1. Simpler integration (one HTTP call vs. NWC connection management)
2. Better UX for submitters (token in a tag, no wallet setup)
3. Lower latency (synchronous redemption)
4. Natural fit for the "attach proof to event" pattern

Add NWC as a second option once the payment gate interface is established.

## Common Configuration

Both options share these config fields:

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DRYDOCK_PAYMENT_MODE` | `disabled` \| `nwc` \| `cashu` | `disabled` | Payment verification mode |
| `DRYDOCK_REVIEW_PRICE_MSATS` | integer | `0` | Minimum payment in millisatoshis |
| `DRYDOCK_NWC_CONNECTION_STRING` | string | *(empty)* | NIP-47 wallet connect string (NWC mode) |
| `DRYDOCK_CASHU_MINT_URL` | URL | *(empty)* | Cashu mint URL (Cashu mode) |

## Payment Receipt Publishing

After a successful paid review, Drydock can optionally publish a receipt event as a kind 1111 comment with a payment confirmation footer:

```
---
payment-mode: cashu
payment-amount-msats: 1000
payment-mint: https://mint.example.com
payment-verified: true
```

This makes the payment auditable on-protocol without revealing the token itself.

## Open Questions

- **Dynamic pricing**: Should the price vary based on diff size, repo complexity, or model route? A simple formula: `base_price + (token_count × per_token_rate)`
- **Refunds**: If a review fails (LLM error, repo unavailable), should the token be refunded? Cashu tokens are bearer instruments — refund would require a new token issuance
- **Multi-mint support**: Should Drydock accept tokens from multiple mints? Would require a list of trusted mint URLs
- **Free tier**: Allow a configurable number of free reviews per pubkey per day before requiring payment
- **Subscription model**: NIP-47 supports recurring payments — could enable monthly subscriptions for unlimited reviews
