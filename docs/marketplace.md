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
| 25910 | ContextVM JSON-RPC | Drydock / Reviewer | Assignment, accept, and reject methods |
| 7000 | Review Feedback | Author / Drydock | NIP-90 feedback for completed review quality and marketplace outcomes |
| 1059 | NIP-59 Gift Wrap | Any private participant | Encrypted envelope for private marketplace commands |

Deprecated mappings: `30620` is replaced by NIP-89 kind `31990`; `1660`, `1661`, and `1662` are replaced by ContextVM messages on kind `25910`; `1663` is replaced by NIP-90 feedback kind `7000`.

## Reviewer Registration

### NIP-89 Reviewer Profile (kind 31990)

Reviewers publish a NIP-89 handler/reviewer profile. The event is addressable and discoverable by tags:

```json
{
  "kind": 31990,
  "content": {
    "name": "Alice Security",
    "display_name": "Alice Security",
    "about": "Security-focused Go, Rust, and Python reviewer",
    "picture": "https://example.com/alice.png",
    "nip90": true,
    "drydock": {
      "languages": ["go", "rust", "python"],
      "domains": ["security", "cryptography", "performance"],
      "availability": "available",
      "price_per_review": 5000,
      "payout_destination": "lnbc...",
      "max_concurrent": 3,
      "response_time": "4h"
    }
  },
  "tags": [
    ["d", "drydock-reviewer"],
    ["k", "1111"],
    ["k", "7000"],
    ["t", "code-reviewer"],
    ["t", "security-expert"],
    ["t", "go"],
    ["t", "rust"],
    ["t", "python"]
  ]
}
```

### Profile Fields

| Field | Type | Description |
|-------|------|-------------|
| `name` / `display_name` | string | Human-readable reviewer name |
| `about` | string | Public reviewer summary |
| `drydock.languages` | string[] | Programming languages (lowercase) |
| `drydock.domains` | string[] | Expertise areas (security, performance, api-design, etc.) |
| `drydock.availability` | string | `available`, `busy`, or `unavailable` |
| `drydock.price_per_review` | int | Price in satoshis (0 = free) |
| `drydock.payout_destination` | string | Fresh BOLT11 invoice used for reviewer payout |
| `drydock.max_concurrent` | int | Maximum simultaneous assignments |
| `drydock.response_time` | string | Typical response time (e.g., "2h", "24h") |

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
    ["e", "<patch-event-id>"],
    ["a", "30617:<repo-naddr>"],
    ["t", "drydock"],
    ["method", "marketplace/assign"],
    ["expiration", "1714003200"]
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

### Feedback Event (kind 7000)

After a review is completed, the patch author can rate it with NIP-90 feedback:

```json
{
  "kind": 7000,
  "content": "Excellent review, found a critical security issue",
  "tags": [
    ["status", "success"],
    ["e", "<review-event-id>"],
    ["p", "<reviewer-pubkey>"],
    ["rating", "5"],
    ["helpful", "true"],
    ["accurate", "true"],
    ["t", "drydock"],
    ["t", "review-feedback"]
  ]
}
```

## Configuration

### Server Environment Variables

```bash
# Enable marketplace
DRYDOCK_MARKETPLACE_ENABLED=true

# Default relays for marketplace events
DRYDOCK_MARKETPLACE_RELAYS="wss://relay.damus.io,wss://nos.lol"

# Maximum reviewers to assign per patch
DRYDOCK_MARKETPLACE_MAX_REVIEWERS=2

# Assignment timeout (time to accept/reject)
DRYDOCK_MARKETPLACE_ASSIGNMENT_TIMEOUT=2h

# Default review deadline
DRYDOCK_MARKETPLACE_DEFAULT_DEADLINE=24h

# Minimum reputation to receive assignments
DRYDOCK_MARKETPLACE_MIN_REPUTATION=0.3
```

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
- **Escrow Payments**: Hold funds until review completion
- **Reviewer Verification**: On-chain attestations of expertise
- **Dispute Resolution**: Mechanism for contested reviews
