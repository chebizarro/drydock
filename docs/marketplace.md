# Review Marketplace

The Drydock marketplace connects patch authors with specialized human reviewers. Community members can register as reviewers with expertise in specific languages and domains, building reputation through quality reviews.

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
| 30620 | Reviewer Profile | Reviewer | Addressable profile with expertise, availability, pricing |
| 1660 | Review Assignment | Drydock | Assigns patch to reviewer |
| 1661 | Assignment Acceptance | Reviewer | Reviewer accepts the assignment |
| 1662 | Assignment Rejection | Reviewer | Reviewer declines (with reason) |
| 1663 | Review Feedback | Author | Author rates the completed review |

## Reviewer Registration

### Profile Event (kind 30620)

Reviewers publish an addressable profile:

```json
{
  "kind": 30620,
  "content": {
    "display_name": "Alice Security",
    "languages": ["go", "rust", "python"],
    "domains": ["security", "cryptography", "performance"],
    "availability": "available",
    "price_per_review": 5000,
    "max_concurrent": 3,
    "response_time": "4h"
  },
  "tags": [
    ["d", "reviewer-profile"],
    ["t", "code-reviewer"],
    ["t", "security-expert"]
  ]
}
```

### Profile Fields

| Field | Type | Description |
|-------|------|-------------|
| `display_name` | string | Human-readable name |
| `languages` | string[] | Programming languages (lowercase) |
| `domains` | string[] | Expertise areas (security, performance, api-design, etc.) |
| `availability` | string | `available`, `busy`, or `unavailable` |
| `price_per_review` | int | Price in satoshis (0 = free) |
| `max_concurrent` | int | Maximum simultaneous assignments |
| `response_time` | string | Typical response time (e.g., "2h", "24h") |

## Patch Routing

When a patch arrives that needs human review:

1. **Extract Requirements**: Detect languages from changed files
2. **Find Matches**: Query registry for reviewers matching criteria
3. **Score & Rank**: Calculate match scores based on:
   - Language overlap (50% weight)
   - Domain match (30% weight)
   - Availability (10% weight)
   - Response time (10% weight)
4. **Assign**: Create assignments for top N matches
5. **Notify**: Publish assignment events tagging reviewers

### Match Score Calculation

```
score = (language_overlap × 0.5) + (domain_match × 0.3) + (availability_bonus × 0.1) + (speed_bonus × 0.1)
```

Where:
- `language_overlap`: Jaccard index of reviewer languages vs. patch languages
- `domain_match`: Jaccard index of domains (if specified)
- `availability_bonus`: 1.0 if available, 0.5 if busy, 0 if unavailable
- `speed_bonus`: Based on response_time if fast review requested

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

### Assignment Event (kind 1660)

```json
{
  "kind": 1660,
  "content": {
    "assignment_id": "abc123def456",
    "patch_event_id": "...",
    "repo_id": "github.com/user/project",
    "languages": ["go", "rust"],
    "price_sats": 5000,
    "deadline": 1714003200
  },
  "tags": [
    ["p", "<reviewer-pubkey>"],
    ["e", "<patch-event-id>"],
    ["a", "30617:<repo-naddr>"],
    ["expiration", "1714003200"]
  ]
}
```

### Acceptance Event (kind 1661)

```json
{
  "kind": 1661,
  "content": {
    "assignment_id": "abc123def456",
    "estimated_time": "2h"
  },
  "tags": [
    ["e", "<assignment-event-id>"]
  ]
}
```

### Rejection Event (kind 1662)

```json
{
  "kind": 1662,
  "content": {
    "assignment_id": "abc123def456",
    "reason": "Outside my expertise area"
  },
  "tags": [
    ["e", "<assignment-event-id>"]
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

```
acceptance_rate = accepted_assignments / total_assignments
rating_normalized = average_rating / 5.0
volume_bonus = 1 - (1 / (1 + completed_reviews / 10))

overall_score = (acceptance_rate × 0.4) + (rating_normalized × 0.4) + (volume_bonus × 0.2)
```

### Feedback Event (kind 1663)

After a review is completed, the patch author can rate it:

```json
{
  "kind": 1663,
  "content": {
    "review_event_id": "...",
    "reviewer_pubkey": "...",
    "rating": 5,
    "helpful": true,
    "accurate": true,
    "comment": "Excellent review, found a critical security issue"
  },
  "tags": [
    ["e", "<review-event-id>"],
    ["p", "<reviewer-pubkey>"]
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
2. **Set Realistic Availability**: Update when you're busy to avoid assignment expiry
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
