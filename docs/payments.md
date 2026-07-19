# Payments: Free Access, Zap Receipts, and Cashu

> **Status**: Payment gating supports configured free access, maintainer access, daily quota, subscriptions, NIP-57 zap receipts, and one-off Cashu tokens settled through NWC-created Lightning invoices.

## Integration point

Payment authorization runs after Drydock has prepared the canonical repository,
loaded its base-branch `.drydock.yaml`, and loaded the stored patch event. When
`payments.enabled` is true, the service checks these paths in order:

1. Existing payment authorization
2. Repository or operator free-pubkey allowlists
3. Repository owner/maintainer access when enabled
4. Active subscription
5. A qualifying NIP-57 zap receipt
6. An attached Cashu payment
7. Daily free-review quota

A denial is stored as `payment_blocked:<reason>` and is excluded from the
normal failed-review retry sweep.

## Free access

- `payments.free_pubkeys` grants unlimited access for listed patch authors.
- `DRYDOCK_FREE_PUBKEYS` grants the same access operator-wide.
- Repository owners and maintainers are free by default; set
  `payments.free_for_maintainers: false` to disable this exemption.

Both lists accept npub or 64-character hex keys and normalize them to hex.
Access kinds are `free_pubkey` and `free_maintainer`.

## NIP-57 zap receipts

Drydock subscribes to kind `9735` receipts. A receipt is accepted for storage
only when:

- its signature, event ID, and timestamp pass normal ingest validation;
- its `p` tag equals the Drydock signer/service pubkey;
- its `e` tag is a valid event ID identifying the patch or PR event;
- its amount is a positive millisatoshi value from `amount` or a
  checksummed fixed-amount `bolt11` invoice (both must agree when present);
- its author appears in `DRYDOCK_TRUSTED_ZAPPERS`, when that allowlist is set.

`DRYDOCK_TRUSTED_ZAPPERS` is a comma-separated npub/hex allowlist of LNURL
receipt-signing providers. An empty list accepts any valid receipt author and
logs that the weaker accept-any mode is active.

The repository policy performs the final price check because it is loaded
later in the pipeline:

```yaml
payments:
  enabled: true
  price_sats: 100
  accept_zaps: true
```

`accept_zaps` defaults to `true` whenever payments are enabled. A stored
receipt authorizes only its tagged patch when
`amount_msat >= price_sats * 1000`, and the authorization result uses access
kind `zap`.

Receipts may arrive before or after the patch. If a receipt arrives after the
review reached `payment_blocked`, receipt persistence atomically clears that
failure, claims the review, and re-enqueues it. Duplicate receipt event IDs are
idempotent.

## Cashu and NWC

A submitter can attach a one-off Cashu token to the patch:

```json
["cashu", "cashuA..."]
```

Drydock accepts tokens only from configured trusted mints, creates and checks
the settlement invoice through NWC, and persists recovery evidence before
authorizing the review. Relevant operator settings are:

| Variable | Purpose |
|----------|---------|
| `DRYDOCK_NWC_CONNECTION_STRING` | NIP-47 wallet connection used for Lightning invoice operations |
| `DRYDOCK_CASHU_TRUSTED_MINTS` | Comma-separated trusted Cashu mint URLs |
| `DRYDOCK_CASHU_MINT_URL` | Backward-compatible single-mint setting |
| `DRYDOCK_TRUSTED_ZAPPERS` | Optional comma-separated receipt-author allowlist |

Cashu review and subscription authorizations use access kinds
`cashu_review` and `cashu_subscription`; active subscriptions use
`subscription`.

## Repository example

```yaml
payments:
  enabled: true
  price_sats: 100
  accept_zaps: true
  free_reviews_per_day: 1
  free_pubkeys:
    - npub1...
  free_for_maintainers: true
  subscription_price_sats: 2000
  subscription_days: 30
```

Payment policy is always read from the canonical repository base branch, so an
incoming patch cannot reduce its own price or enable another access path.
