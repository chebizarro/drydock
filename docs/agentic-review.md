# Agentic Review Operations

Drydock uses one agentic review service for stored-patch pipeline reviews, inline IDE reviews, and full security audits. The service freezes an immutable snapshot, discovers context through capability-filtered tools, verifies the exact package budget, runs an evidence-backed reviewer loop, and optionally persists a versioned session.

## Runtime Flow

```
entry point
    │
    ▼
agenticreview.Service.Prepare
    │
    ├─ freeze workspace snapshot
    ├─ seed patch + changed files
    ├─ discovery tool loop
    └─ selection.finalize exact-token gate
             │
             ▼
agenticreview.Service.ReviewPrepared / StartSession / Continue
             │
             ├─ engine planner + checklist
             ├─ iterative reviewer tools
             ├─ review.submit validation
             └─ consensus + scope + severity + walkthrough
```

Pipeline and audit use pinned Git snapshots. IDE reviews copy and hash the mutable workspace before any tool call. A finalized package and its artifact hashes are immutable for the life of a prepared review or session.

## Tool Contracts

All handlers live in `internal/agenttools`. Internal loops call the registry directly; `internal/mcpserver` adapts the same definitions to MCP. Tools never accept a role, capability, live workspace root, or snapshot identifier from model arguments.

| Tool | Contract |
|------|----------|
| `repo.file_tree` | List files within the bound snapshot and allowlist |
| `code.structure` | Extract declarations from one frozen source file |
| `code.search` | Search frozen text using the bounded search facade |
| `code.read` | Read an in-scope relative path and optional line range |
| `code.references` | Resolve definitions/references through the configured frozen-snapshot LSP facade |
| `tests.search` | Search only test files in the frozen snapshot |
| `git.read` | Closed read-only action set: diff, show, log, blame |
| `context.layer` | Run one named deterministic context provider through its shared facade |
| `security.trace` | Run taint or security-surface analysis across an authorized audit snapshot |
| `selection.add` | Add optional full-file, line-range, or codemap artifacts |
| `selection.remove` | Remove optional artifacts; patch and changed files are mandatory |
| `selection.status` | Report artifacts, exact budget, headroom, and finalized state |
| `selection.finalize` | Re-verify hashes, render, exact-count, and freeze the package |
| `review.submit` | Submit schema-valid coverage and evidence-backed P0/P1/P2 findings |

Every invocation has a server-generated tool-call ID, a bound scope, a result-size limit, and replay protection. Absolute paths, parent traversal, NULs, symlinks, paths outside the allowlist, and fallback to the live workspace fail closed. The deterministic fallback uses the same repository-relative path normalization and rejects symlink components before reading modified files, symbols, or project docs.

## Capability Matrix

Legend: R = repository read tools, W = snapshot-wide security tools, S = selection tools, F = finalization, V = review submission.

| Role | R | W | S | F | V | Intended use |
|------|---|---|---|---|---|--------------|
| `context_discovery` | yes | no | yes | yes | no | Patch/IDE discovery |
| `security_auditor_discovery` | yes | yes | yes | yes | no | Full-snapshot audit discovery |
| `code_reviewer` | yes | no | no | no | yes | Patch/IDE iterative reviewer |
| `security_auditor` | yes | yes | no | no | yes | Full-snapshot iterative reviewer |
| `external_readonly` | yes | no | no | no | no | MCP clients |

Both `tools/list` and dispatch enforce the matrix. Availability is narrower still: selection tools require a server-created selection controller, and `review.submit` requires a server-created review submitter. HTTP MCP accepts only `external_readonly` until an external reviewer-to-session binding is specified.

## Finding and Session Contracts

Agentic findings use canonical priority with a total compatibility mapping:

| Priority | Legacy severity |
|----------|-----------------|
| P0 | critical |
| P1 | high |
| P2 | medium |

Each finding cites at least one successful current-run evidence tool-call ID. Patch and IDE findings must target the authoritative changed-file set. Security-audit findings may target any valid path and line in the frozen snapshot.

A continuation requires `chat_id`, owner, request ID, `expected_version`, and a non-empty message. The store applies request-ID idempotency and optimistic versioning. Duplicate identical requests replay the stored result; reused IDs with different payloads and stale/future versions fail without rerunning the model.

## Failure Modes

| Failure | Behavior | Operator action |
|---------|----------|-----------------|
| Discovery turn/tool/token/context exhaustion | Run deterministic builder against a materialization of the same snapshot, then apply the same exact gate | Inspect stop-reason and fallback metrics; reduce optional context or raise a justified limit |
| Deterministic fallback also exceeds budget | Fail the review; never review a partial package | Reduce provider output or increase package budget after capacity review |
| Tokenizer cannot load authoritative encoding | Agentic startup/configuration fails; approximate counting is not accepted | Restore tokenizer assets/build support before enabling agentic mode |
| `selection.finalize` over budget | Return a tool error; selection remains mutable so the model can prune and retry | Inspect utilization and finalization-failure metrics |
| Reviewer loop exhausts without `review.submit` | Drop that ensemble member; fail only if every member fails | Inspect per-member trace and model/tool limits |
| Cancellation or deadline | Record stop reason `cancelled` (`StopCancelled`) and return no partial review; a reserved session turn is recorded failed where applicable | Retry with a fresh request ID after the caller is healthy |
| Version or idempotency conflict | Reject before model execution | Reload the latest version; never recycle a request ID for different text |
| Broken or expired session | Reject the continuation before snapshot restore or model execution | Start a new review; do not retry the broken/expired `chat_id` |
| Snapshot/patch/manifest hash mismatch | Fail closed and mark a persisted session broken | Preserve evidence, expire the session, and follow snapshot cleanup |
| History exceeds budget | Fail the continuation; code context is never silently removed | Shorten conversation or raise the history budget deliberately |
| MCP authentication or scope resolution failure | Reject before tools are listed or dispatched | Verify bearer token and server-created session binding |

## Operator Configuration

### Agentic loops and budgets

| Environment variable | Default | Purpose |
|----------------------|---------|---------|
| `DRYDOCK_AGENTIC_REVIEW_FALLBACK` | `false` | Explicitly use legacy deterministic context plus single-shot review |
| `DRYDOCK_AGENTIC_DISCOVERY_BASE_URL` | coder32b endpoint | Discovery model endpoint |
| `DRYDOCK_AGENTIC_DISCOVERY_MODEL` | coder32b model | Discovery model name |
| `DRYDOCK_AGENTIC_DISCOVERY_API_KEY` | coder32b key | Discovery endpoint credential |
| `DRYDOCK_AGENTIC_DISCOVERY_MAX_TURNS` | `24` | Discovery turn limit |
| `DRYDOCK_AGENTIC_DISCOVERY_MAX_TOOL_CALLS` | `96` | Discovery tool-call limit |
| `DRYDOCK_AGENTIC_DISCOVERY_MAX_CUMULATIVE_TOKENS` | `256000` | Discovery cumulative-token limit |
| `DRYDOCK_AGENTIC_REVIEWER_MAX_TURNS` | `20` | Reviewer turn limit |
| `DRYDOCK_AGENTIC_REVIEWER_MAX_TOOL_CALLS` | `96` | Reviewer tool-call limit |
| `DRYDOCK_AGENTIC_REVIEWER_MAX_CUMULATIVE_TOKENS` | `384000` | Reviewer cumulative-token limit |
| `DRYDOCK_AGENTIC_MAX_TOOL_RESULT_BYTES` | `16384` | Per-tool result byte ceiling |
| `DRYDOCK_AGENTIC_MAX_MODEL_CONTEXT` | `256000` | Serialized request preflight ceiling |
| `DRYDOCK_AGENTIC_PACKAGE_TOKEN_BUDGET` | `64000` | Final package budget |
| `DRYDOCK_AGENTIC_TOKEN_HEADROOM` | `0.10` | Fraction reserved for tokenizer/model skew |
| `DRYDOCK_AGENTIC_HISTORY_TOKEN_BUDGET` | `64000` | Compacted conversation-history budget |
| `DRYDOCK_IDE_AGENTIC_TIMEOUT` | `10m` | End-to-end IDE review/continuation deadline |
| `DRYDOCK_IDE_WORKSPACE_BINDINGS` | none | Comma-separated `lowercase-pubkey=/absolute/workspace` bindings for inline snapshots; repeat a pubkey for multiple roots; empty disables inline filesystem review |

All integer limits must be positive. Headroom must be at least 0 and less than 1.

### Snapshots and sessions

| Environment variable | Default | Purpose |
|----------------------|---------|---------|
| `DRYDOCK_REVIEW_SNAPSHOT_STORAGE_PATH` | `<DRYDOCK_REPO_CACHE_DIR>/review-snapshots` (`repos/review-snapshots` by default) | Snapshot descriptors and mutable copies |
| `DRYDOCK_REVIEW_SNAPSHOT_TTL` | `24h` | Unleased snapshot lifetime |
| `DRYDOCK_REVIEW_SNAPSHOT_LEASE_TTL` | `24h` | Default lease lifetime |
| `DRYDOCK_REVIEW_SESSION_LIFETIME` | `24h` | Review-session lifetime |
| `DRYDOCK_REVIEW_SNAPSHOT_GC_INTERVAL` | `15m` | Session expiry and snapshot GC cadence |

The snapshot lease TTL must be at least the session lifetime.

### MCP HTTP

| Environment variable | Default | Purpose |
|----------------------|---------|---------|
| `DRYDOCK_MCP_HTTP_ENABLED` | `false` | Enable authenticated Streamable HTTP |
| `DRYDOCK_MCP_HTTP_ADDR` | `127.0.0.1:8090` | Listen address |
| `DRYDOCK_MCP_HTTP_BEARER_TOKEN` | none | Required bearer token |
| `DRYDOCK_MCP_HTTP_SESSION_ID` | none | Required server-created 32-character review session ID |
| `DRYDOCK_MCP_MAX_REQUEST_BODY_BYTES` | `4194304` | HTTP request-body ceiling |
| `DRYDOCK_MCP_SHUTDOWN_TIMEOUT` | `30s` | Graceful HTTP shutdown deadline |

HTTP scope is resolved only after authentication. Do not expose the listener publicly without TLS termination and secret-management controls.

## MCP Operation

For local read-only stdio use:

```
drydock-mcp \
  -target /path/to/repository \
  -ref HEAD \
  -allow-paths . \
  -snapshot-storage /var/lib/drydock/mcp-snapshots
```

The process freezes the target before starting transport and always binds `external_readonly`. Selection and submission handlers require server-owned state and are intentionally unavailable in the standalone command.

For HTTP, configure the environment variables above on `drydock-core`. The bearer authorizer binds each authenticated connection to the configured server-created session and rejects non-external roles. Rotate the bearer token by restarting the listener with the new secret.

## Metrics and Logs

| Metric | Labels | Meaning |
|--------|--------|---------|
| `drydock_agentic_loop_turns_total` | — | Discovery and reviewer model turns |
| `drydock_agentic_tool_calls_total` | `tool`, `outcome` | Canonical agent-tool calls and outcomes |
| `drydock_agentic_budget_utilization_ratio` | `budget` | Utilization for turns, tool calls, cumulative tokens, and context package |
| `drydock_agentic_finalization_failures_total` | `reason` | Exact-package finalization failures |
| `drydock_agentic_loop_exhaustion_fallbacks_total` | — | Discovery exhaustion runs that invoked deterministic fallback |
| `drydock_agentic_session_conflicts_total` | `type` | Version, idempotency, and active-turn conflicts |
| `drydock_agentic_snapshot_corruption_total` | — | Frozen-snapshot integrity failures |
| `drydock_agentic_stop_reasons_total` | `reason` | Loop stop reasons, including `cancelled` |

Ensemble status additionally reports required, successful, failed, and degraded members with per-member traces. Alert on sustained finalization failures, any snapshot corruption, all-member ensemble failure, conflict spikes, repeated tokenizer startup failure, and p95 IDE duration approaching `DRYDOCK_IDE_AGENTIC_TIMEOUT`.

## Snapshot Cleanup

Normal cleanup is automatic and ordered:

1. The lifecycle loop marks expired sessions `expired`.
2. It releases their snapshot leases and decrements snapshot references.
3. `workspacesnapshot.Manager.GC` removes only expired, unleased snapshots.
4. The loop repeats at `DRYDOCK_REVIEW_SNAPSHOT_GC_INTERVAL`.

For operator cleanup:

1. Stop new review intake and disable MCP HTTP.
2. Back up the SQLite database and snapshot storage directory.
3. Query `review_sessions` for active sessions and `review_snapshots` for nonzero `ref_count`. Do not remove referenced storage.
4. Let `drydock-core` run for at least one GC interval so it expires sessions before collecting snapshots.
5. Verify expired sessions have zero references and their storage paths no longer exist.
6. If orphaned files remain, stop every Drydock process, confirm no active session references their snapshot IDs, then remove only those unreferenced paths.
7. Restart and watch snapshot-corruption and lifecycle logs.

Never delete the storage root while active sessions exist. A resumable session must never point at a collected snapshot.

## Rollout Sequence

Advance one stage at a time. Hold or roll back when its gate fails.

| Stage | Enablement | Exit gate | Rollback |
|-------|------------|-----------|----------|
| 1. Shadow discovery | Run discovery and compare package hashes/coverage without changing reviewer input | Stable finalization rate, bounded utilization, no path/snapshot violations | Stop shadow execution |
| 2. Agentic context + single-shot reviewer | Use finalized agentic package with legacy reviewer | Quality at least baseline; fallback and budget rates within target | Set `DRYDOCK_AGENTIC_REVIEW_FALLBACK=true` |
| 3. Iterative reviewer | Enable read tools and mandatory `review.submit` | Better verified-finding precision; acceptable exhaustion/all-member-fail rate | Return to single-shot reviewer |
| 4. Sessions | Enable IDE `chat_id` continuations and lifecycle GC | Low conflict rate; replay, restart recovery, expiry, and broken-state drills pass | Disable new sessions; preserve existing leases until expiry |
| 5. MCP | Enable stdio, then authenticated HTTP read-only access | Auth failures fail closed; role-filtered tool lists and scope isolation verified | Disable HTTP and revoke bearer token |
| 6. Remove fallback | Delete legacy path only after every prior stage meets its gate for the agreed observation window | No recent fallback dependence; rollback release tested | Redeploy the last release containing fallback |

Record the observation window, target thresholds, decision owner, and rollback release for every environment. MCP never precedes sessions because HTTP scope binding depends on a server-created session. External reviewer submission remains disabled until its server-side authorization contract is designed and reviewed.
