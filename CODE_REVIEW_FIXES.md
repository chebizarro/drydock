# Code Review Fixes Applied

## Summary
All identified issues from the code review have been fixed. This document summarizes the changes made.

## Critical Issues Fixed

### 1. Resource Leak in Database Queries
**File**: `internal/db/store.go`
- Added `defer rows.Close()` immediately after error check in `GetPublishRelays`
- Ensures proper cleanup even if errors occur during iteration

### 2. Nil Pointer Dereference in Listener
**File**: `internal/listener/listener.go`
- Added nil check for `ie.Relay` before accessing `ie.Relay.URL`
- Prevents panic when channel is closed or relay is nil

### 3. Race Condition in Git Operations
**File**: `internal/repo/manager.go`
- Added `sync.Map` for repository-level locking
- Implemented `getRepoLock()` helper method
- `ApplyPatchSeries` now acquires mutex before git operations
- Prevents concurrent git operations on the same repository

### 4. Unbounded Goroutines in Meta-Review
**File**: `internal/metareview/service.go`
- Added `golang.org/x/sync/semaphore` for concurrency control
- Added `MaxConcurrent` config field (default: 10)
- `RunAsync` now uses semaphore to limit concurrent executions
- Prevents resource exhaustion from too many concurrent meta-reviews

### 5. Context Handling in Goroutines
**File**: `internal/repo/manager.go`
- Fixed `applySinglePatch` to respect context cancellation
- Added error channel to properly handle write errors
- Goroutine now checks context before writing

### 6. Transaction Safety for Multi-Step Operations
**File**: `internal/db/store.go`
- Wrapped `BeginReview` in transaction for atomicity
- Wrapped `InsertPatchEvent` + `UpsertThreadCache` in transaction
- Prevents inconsistent state if operations fail mid-way

## Configuration Improvements

### 7. Configurable Listener Lookback
**Files**: `internal/config/config.go`, `internal/listener/listener.go`, `cmd/drydock/main.go`
- Added `ListenerLookbackMin` config field
- Added `DRYDOCK_LISTENER_LOOKBACK_MIN` environment variable
- Replaced hardcoded 5-minute lookback with configurable value
- Added `parseIntOrDefault` helper function

## Database Schema Improvements

### 8. Foreign Key Constraints
**File**: `internal/db/schema.go`
- Added foreign key constraint to `patch_event_relays` table
- References `patch_events(event_id)` with `ON DELETE CASCADE`
- Prevents orphaned relay records

## Code Quality Improvements

### 9. Magic String Constants
**File**: `internal/metareview/service.go`
- Added constants for "why missed" reasons:
  - `WhyMissedInsufficientContext`
  - `WhyMissedModelLimitation`
  - `WhyMissedPromptGap`
- Added constants for routing actions:
  - `ActionFlagContextBuilder`
  - `ActionFlagModelRouting`
  - `ActionQueuePromptRefinement`
- Updated `routeFeedback` to use constants

### 10. Signer Validation
**File**: `internal/signing/bunker.go`
- Added validation test calling `GetPublicKey` before returning signer
- Ensures bunker signer is functional before use

### 11. Token Count Bounds Checking
**File**: `internal/contextbuilder/builder.go`
- Added bounds check in `ApproxTokenCounter.Count`
- Prevents overflow for extremely large strings
- Returns capped value (1<<30) if rune count exceeds safe limits

### 12. Code Documentation
**File**: `internal/repo/manager.go`
- Added comment explaining array bounds safety for `cloneURLs[0]`

## Dependencies Added

### 13. golang.org/x/sync
**File**: `go.mod`
- Added `golang.org/x/sync v0.10.0` dependency
- Required for semaphore-based concurrency control

## Testing Recommendations

Before deploying, test the following scenarios:

1. **Concurrent Reviews**: Verify multiple patches on same repo don't conflict
2. **Database Transactions**: Test rollback behavior on errors
3. **Meta-Review Limits**: Verify semaphore prevents resource exhaustion
4. **Context Cancellation**: Test graceful shutdown during git operations
5. **Configuration**: Test environment variable parsing for `DRYDOCK_LISTENER_LOOKBACK_MIN`

## Migration Notes

- The schema change (foreign key constraint) will be applied automatically via `store.Migrate()`
- Existing data is compatible with all changes
- No manual migration steps required
- Run `go mod tidy` to update `go.sum` with new dependency

## Environment Variables

New configuration option:
```bash
DRYDOCK_LISTENER_LOOKBACK_MIN=5  # Minutes to look back for events (default: 5)
```

All fixes maintain backward compatibility while improving reliability and maintainability.


