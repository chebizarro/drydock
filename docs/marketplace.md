# Review Marketplace

The Drydock marketplace connects patch authors with specialized human reviewers. Community members publish standard NIP-89 handler profiles to advertise review capabilities, and marketplace coordination uses ContextVM JSON-RPC over Nostr.

## Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Review Marketplace                            │
│                                                                      │
│  ┌──────────────┐      ┌──────────────┐      ┌──────────────────┐  │
│  │   Reviewer   │      │    Router    │      │   Patch Author   │  │
│  │   Registry   │◄────▶│  (matching)  │◄────▶│    (requester)   │  │
│  └──────────────┘      └──────────────┘      └──────────────────┘  │
│         │                     │                       │             │
│         ▼                     ▼                       ▼             │
│  ┌──────────────┐      ┌──────────────┐      ┌──────────────────┐  │
│  │  Reputation  │      │  Assignment  │      │    Feedback      │  │
│  │    System    │◄─────│   Manager    │─────▶│    & Ratings     │  │
│  └──────────────┘      └──────────────┘      └──────────────────┘  │
└─────────────────────────────────────────────────────────────────────┘
```

## Nostr Event Kinds

| Kind | Name | Publisher | Description |
|------|------|-----------|-------------|
| 31990 | Reviewer Profile | Reviewer | NIP-89 handler profile advertising reviewer capabilities |
| 25910 | ContextVM JSON-RPC | Drydock / Reviewer / Requester | Assignment, accept, reject, complete, and feedback methods |
| 1059 | NIP-59 Gift Wrap | Any private participant | Encrypted envelope for private marketplace requests and notifications |

Deprecated mappings: `30620` is replaced by NIP-89 kind `31990`; `1660`–`1663` and NIP-90 kind `7000` are replaced by ContextVM messages on kind `25910`. Kind `7000` is not subscribed to or routed.

## Reviewer Registration

### NIP-89 Reviewer Profile (kind 31990)

Reviewers publish a NIP-89 handler/reviewer profile. The event is addressable and discoverable by tags:

```json
{
  "kind": 31990,
  "content": {
    "pubkey": "<reviewer-pubkey>",
    "max_concurrent": 3,
    "response_time": "4h"
  },
  "tags": [
    ["d", "drydock-reviewer"],
    ["k", "25910"],
    ["name", "Alice Security"],
    ["about", "Security-focused Go, Rust, and Python reviewer"],
    ["drydock:languages", "go", "rust", "python"],
    ["drydock:domains", "security", "cryptography", "performance"],
    ["drydock:availability", "available"],
    ["drydock:price", "5000"],
    ["drydock:payout", "lnbc..."],
    ["drydock:methods", "marketplace/assign", "marketplace/accept", "marketplace/reject", "marketplace/complete", "marketplace/feedback"]
  ]
}
```

### Profile Fields

| Field/Tag | Type | Description |
|-----------|------|-------------|
| content `pubkey` | string | Reviewer public key; the authenticated event author remains authoritative |
| content `max_concurrent` | int | Maximum simultaneous assignments |
| content `response_time` | string | Typical response time, such as `4h` |
| `name` / `about` | tag | Human-readable identity and summary |
| `drydock:languages` | multi-value tag | Lowercase programming languages |
| `drydock:domains` | multi-value tag | Expertise areas |
| `drydock:availability` | tag | `available`, `busy`, or `unavailable` |
| `drydock:price` | tag | Price per review in satoshis |
| `drydock:payout` | tag | Fresh BOLT11 payout destination |
| `drydock:methods` | multi-value tag | Supported ContextVM marketplace methods |

## Patch Routing

When a patch arrives that needs human review:

1. **Extract Requirements**: Detect languages from changed files
2. **Find Matches**: Query NIP-89 reviewer profiles (`31990`) matching criteria
3. **Score & Rank**: Calculate match scores based on:
   - Language overlap (50% weight)
   - Domain match (30% weight)
   - Availability (10% weight)
   - Response time (10% weight)
4. **Assign**: Send `marketplace/assign` ContextVM requests to top N matches
5. **Notify**: Address assignments to reviewers with `p` tags and NIP-59 gift-wrap when private

### Match Score Calculation

```text
score = (language_overlap × 0.5) + (domain_match × 0.3) + (availability_bonus × 0.1) + (speed_bonus × 0.1)
```

Where:
- `language_overlap`: Jaccard index of reviewer languages vs. patch languages
- `domain_match`: Jaccard index of domains (if specified)
- `availability_bonus`: 1.0 if available, 0.5 if busy, 0 if unavailable
- `speed_bonus`: Based on response_time if fast review requested

## ContextVM Marketplace Methods

Marketplace commands use kind `25910` with JSON-RPC 2.0 payloads.

| Method | Direction | Purpose |
|--------|-----------|---------|
| `marketplace/assign` | Drydock → Reviewer | Offers a patch review assignment |
| `marketplace/accept` | Reviewer → Drydock | Accepts an assignment |
| `marketplace/reject` | Reviewer → Drydock | Declines an assignment with a reason |
| `marketplace/complete` | Reviewer → Drydock | Authenticates a published review event and triggers payout/reconciliation |
| `marketplace/feedback` | Assignment requester → Drydock | One-way authenticated rating notification; no response |

See [ContextVM Integration](contextvm-integration.md) for the shared request, response, and error format.

A `marketplace/complete` request contains `assignment_id` and `review_event_id`. Drydock accepts it only from the assigned reviewer, verifies that the stored signed review event belongs to that reviewer and correlates to the assignment patch/repository, then atomically records completion and allocates one payout. Payout transitions are `pending → submitted → settled|failed`; ambiguous wallet outcomes remain `submitted` for `lookup_invoice` reconciliation and are never resubmitted.

## Assignment Lifecycle

```
    ┌─────────┐         ┌──────────┐         ┌───────────┐
    │ Pending │────────▶│ Accepted │────────▶│ Completed │
    └────┬────┘         └──────────┘         └───────────┘
         │                   │
         │              (rejection)
         │                   │
         ▼                   ▼
    ┌─────────┐         ┌──────────┐
    │ Expired │         │ Rejected │──▶ Reassign
    └─────────┘         └──────────┘
```

### Assignment Request (`marketplace/assign` on kind 25910)

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "assign-abc123def456",
    "method": "marketplace/assign",
    "params": {
      "assignment_id": "abc123def456",
      "patch_event_id": "...",
      "repo_id": "github.com/user/project",
      "languages": ["go", "rust"],
      "price_sats": 5000,
      "deadline": 1714003200
    }
  },
  "tags": [
    ["p", "<reviewer-pubkey>"],
    ["method", "marketplace/assign"]
  ]
}
```

### Acceptance Request (`marketplace/accept` on kind 25910)

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "accept-abc123def456",
    "method": "marketplace/accept",
    "params": {
      "assignment_id": "abc123def456",
      "estimated_time": "2h"
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["e", "<assignment-event-id>"],
    ["t", "drydock"],
    ["method", "marketplace/accept"]
  ]
}
```

### Rejection Request (`marketplace/reject` on kind 25910)

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "id": "reject-abc123def456",
    "method": "marketplace/reject",
    "params": {
      "assignment_id": "abc123def456",
      "reason": "Outside my expertise area"
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["e", "<assignment-event-id>"],
    ["t", "drydock"],
    ["method", "marketplace/reject"]
  ]
}
```

## Reputation System

Reputation scores drive routing priority and trust:

### Score Components

| Component | Weight | Range | Description |
|-----------|--------|-------|-------------|
| Acceptance Rate | 40% | 0-1 | Accepted / Total Assignments |
| Average Rating | 40% | 1-5 → 0-1 | Mean feedback rating |
| Volume Bonus | 20% | 0-1 | Diminishing returns on review count |

### Calculation

```text
acceptance_rate = accepted_assignments / total_assignments
rating_normalized = average_rating / 5.0
volume_bonus = 1 - (1 / (1 + completed_reviews / 10))

overall_score = (acceptance_rate × 0.4) + (rating_normalized × 0.4) + (volume_bonus × 0.2)
```

### Feedback Notification (`marketplace/feedback` on kind 25910)

After completion, the assignment requester may rate the review with a one-way ContextVM notification:

```json
{
  "kind": 25910,
  "content": {
    "jsonrpc": "2.0",
    "method": "marketplace/feedback",
    "params": {
      "review_event_id": "<review-event-id>",
      "rating": 5,
      "helpful": true,
      "accurate": true,
      "comment": "Excellent review; it found a critical issue"
    }
  },
  "tags": [
    ["p", "<drydock-pubkey>"],
    ["e", "<review-event-id>"],
    ["method", "marketplace/feedback"],
    ["expiration", "1714003200"]
  ]
}
```

The JSON-RPC `id` is omitted, so Drydock sends no response. The signed event sender must be the durable assignment requester; the reviewer is derived from the completed assignment and cannot be supplied by the caller. The `e` and `method` tags must match the payload. Ratings are 1–5 and comments are limited to 4096 bytes.

Feedback is rate-limited per sender. The first valid feedback write wins; duplicate relay delivery or a competing later event is idempotent and does not update reputation twice. Transient storage failures remain eligible for relay redelivery. NIP-90 kind `7000` is retired and ignored after the clean cut.

## Configuration

### Server Environment Variables

Marketplace feedback uses the global relay configuration and these per-sender limits:

```bash
DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_REQUESTS=100
DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_WINDOW=24h
```

Marketplace routing is composed whenever the signed runtime is enabled; there is no separate `DRYDOCK_MARKETPLACE_ENABLED` switch. Assignment count, timeout, deadline, and minimum reputation currently use the defaults wired by `cmd/drydock`, not environment variables.

### Routing Criteria

Patch authors can specify preferences via tags:

```json
{
  "kind": 1617,
  "tags": [
    ["drydock-review", "marketplace"],
    ["drydock-domains", "security,performance"],
    ["drydock-max-price", "10000"],
    ["drydock-prefer", "<specific-reviewer-pubkey>"]
  ]
}
```

## Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `drydock_marketplace_routing_attempts_total` | Counter | Patch routing attempts |
| `drydock_marketplace_routing_successes_total` | Counter | Successful routings |
| `drydock_marketplace_no_reviewers_found_total` | Counter | No matching reviewers |
| `drydock_marketplace_assignments_created_total` | Counter | Assignments created |
| `drydock_marketplace_assignments_accepted_total` | Counter | Assignments accepted |
| `drydock_marketplace_assignments_rejected_total` | Counter | Assignments rejected |
| `drydock_marketplace_assignments_expired_total` | Counter | Assignments expired |
| `drydock_marketplace_reviewers_active` | Gauge | Active reviewers |
| `drydock_marketplace_feedback_received_total` | Counter | Feedback events |
| `drydock_marketplace_reputation_updates_total` | Counter | Reputation recalculations |

## Database Schema

```sql
-- Reviewer profiles
CREATE TABLE reviewer_profiles (
  pubkey TEXT PRIMARY KEY,
  display_name TEXT,
  languages TEXT,        -- JSON array
  domains TEXT,          -- JSON array
  availability TEXT,
  price_per_review INTEGER,
  max_concurrent INTEGER,
  event_id TEXT,
  created_at INTEGER,
  updated_at INTEGER
);

-- Reputation scores
CREATE TABLE reviewer_reputations (
  pubkey TEXT PRIMARY KEY,
  overall_score REAL,
  total_reviews INTEGER,
  accepted_reviews INTEGER,
  rejected_reviews INTEGER,
  average_rating REAL,
  acceptance_rate REAL,
  last_review_at INTEGER,
  updated_at INTEGER
);

-- Review assignments
CREATE TABLE review_assignments (
  id INTEGER PRIMARY KEY,
  patch_event_id TEXT,
  repo_id TEXT,
  reviewer_pubkey TEXT,
  requester_pubkey TEXT,
  status TEXT,           -- pending, accepted, rejected, completed, expired
  priority INTEGER,
  price_sats INTEGER,
  assignment_event_id TEXT UNIQUE,
  expires_at INTEGER,
  created_at INTEGER,
  updated_at INTEGER
);

-- Review feedback
CREATE TABLE review_feedback (
  id INTEGER PRIMARY KEY,
  assignment_id INTEGER,
  reviewer_pubkey TEXT,
  rater_pubkey TEXT,
  rating INTEGER,
  comment TEXT,
  event_id TEXT UNIQUE,
  created_at INTEGER
);
```

## Best Practices

### For Reviewers

1. **Be Specific**: List exact languages and domains you're expert in
2. **Set Realistic Availability**: Update your NIP-89 profile when you're busy to avoid assignment expiry
3. **Respond Quickly**: Fast acceptance improves your reputation
4. **Provide Quality Reviews**: Ratings affect future assignments

### For Operators

1. **Monitor Expiry Rate**: High expiry may indicate insufficient reviewers
2. **Balance Pricing**: Consider price caps for accessibility
3. **Encourage Feedback**: Reputation system works best with feedback data
4. **Curate Reviewers**: Consider approval process for new reviewers

## Future Enhancements

- **Web-of-Trust Integration**: Use NIP-02 follows for trust scoring
- **Escrow Policy Extensions**: Add configurable dispute windows and partial-release rules beyond the current completion-triggered payout flow
- **Reviewer Verification**: On-chain attestations of expertise
- **Dispute Resolution**: Mechanism for contested reviews
