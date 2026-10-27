# Security review: maintainer-authorized monitoring

Date: 2026-10-27 · Scope: epic `drydock-zero-restart-monitored-author-config-20261026` (sub-tasks 1–5) · Branches `task/drydock-*-20261027`

This review checks the three properties the epic requires, by reading the code paths and naming the tests that pin them. It also lists what it could **not** verify.

## 1. Neither operator configuration nor payment can manufacture repository-maintainer authority

**Authority is derived in exactly one place.** `monitoring.Registry.authorizedLocked` (`internal/monitoring/registry.go`) grants an author a repository only if the author equals the owner of, or appears in the `maintainers` tag of, the stored kind-30617 announcement for that address (`lookupAuthority` additionally requires the announcement's author to equal the address owner and its `d` to equal the identifier). Every membership source — NIP-51 list entries and ContextVM enrollments — goes through it in `rebuildLocked`.

- Operator list: `operatorCeilingLocked` only **intersects**; a repository named by the operator but by no authorized maintainer is not monitored. Pinned by `TestOperatorListIsCeilingNotGrant`.
- Operator rotation (`monitoring/operator`): `SetOperatorState` replaces the ceiling author only; it cannot add an authorized author. A larger ceiling can at most *expose* existing maintainer opt-ins. Pinned by `TestOperatorControlRotatesPersistsAndRollsBack` (membership empty until the new operator's list arrives; restored only for repositories with a maintainer opt-in).
- Static allowlists: `scope.Matcher` is applied as a second intersection (`TestStaticScopeIsCeiling`).
- Payment: `enrollment.Service.Enroll` calls `authorize` (ceiling → announcement known → `CanMaintainRepository`) **before** `charge`; a paying stranger is rejected with `ErrNotMaintainer` and never reaches the authorizer (`TestMaintainerMayEnrollStrangerMayNot`, `TestEnrollFailsClosedWithoutAnnouncementAndOutsideCeiling`). Payment can only *deny* an already-authorized maintainer (`TestEnrollPaymentPolicy`). Maintainers are deliberately not free for enrollment (`FreeForMaintainers=false`) so payment is a price, never a credential.
- Parameters: identity is `request.Sender`, which the handler requires to equal `request.Event.PubKey`; `repo_addr` and the optional `a` tag must agree; no parameter names an author.
- Announcement replay: `db.ApplyRepositoryAnnouncement` applies NIP-01 replacement ordering so a replayed older announcement cannot resurrect a removed maintainer (`TestApplyRepositoryAnnouncementNewestWinsAndStaleReplayIsIgnored`, `TestAnnouncementReplacementWithdrawsMaintainerAuthority`).
- Failure mode: a failed authority refresh evicts the cached authority rather than keeping it (`TestAnnouncementRefreshFailureFailsClosed`).

## 2. Customer-controlled relays cannot bypass egress policy

All declared relays pass `relayroute.Build` (`internal/relayroute/plan.go`) before the listener sees them:

- Scheme: `wss://` only outside development; loopback, RFC1918/ULA, link-local, unspecified and **zoned** IPv6 literals, `localhost`, `*.local` are refused (`TestBuildProductionPolicyRejectsInsecureAndPrivateHosts`).
- Operator allow/deny host suffixes, deny wins (`TestBuildAllowDenySuffixes`).
- Per-repository cap in declaration order and a global cap preferring shared relays; truncation is recorded (`TestBuildPerRepositoryCapKeepsDeclarationOrderAndSkipsBase`, `TestBuildGlobalCapIsDeterministicAndPrefersSharedRelays`); `0` disables customer relays entirely.
- Filters on customer relays are scoped to the declaring repositories (`#a`) and to their owners' announcements per owner (`TestRouteDirectedFiltersScopeCustomerRelaysToDeclaringRepositories`); no unscoped kinds filter exists.
- Budgets bound what a stranger can make Drydock subscribe to: declared-repository recovery is cut by the operator/static ceilings and capped at 2048; known authors at 256, granting authors first (`TestDeclaredRepositoriesApplyCeilingsAndBudget`, `TestRoutedAuthorsPreferGrantingAuthorsWithinBudget`).

**Residual risk (documented):** a public hostname that resolves to a private address is not detectable without DNS resolution at plan time. Mitigation: `DRYDOCK_REPO_RELAYS_ALLOW`, network-level egress rules.

## 3. Derived state retains signed provenance

Drydock publishes **no** derived kind-30001 list; the signed artefacts *are* the state:

- Lists: the winning revision's raw event is persisted (`monitored_repository_list_state.raw_event`) and re-parsed on load by the same parser the live path uses (`listFromPersisted`), so recovery cannot reinterpret a list (`TestBuildInventoryUsesRegistrySemanticsForLists`).
- Enrollments: `monitoring_enrollments` stores the request event id, its `created_at`, the verified announcement event id, the payment access kind, the expiry, and any revocation event id. Enroll and revoke are ordered commands over one row (newest wins; equal timestamps favour revocation) so a replayed older command cannot undo a newer one (`TestRevocationOrderingBeatsReplayedOlderEnrollment`, `TestEqualTimestampRevocationBeatsEnrollment`). The registry is always a projection of the stored winner (`project`), never of the submitted operation.
- Operator ceiling: `runtime_settings` records `changed_by` and `change_event_id`; the state is persisted before it is published (`TestOperatorControlPersistsBeforePublishing`).

## Other properties checked

- Readiness vs. enrollment: `/readyz` keys on `Snapshot.Loaded`; membership stays fail-closed on `Initialized`/`Contains`. Consumers (`pipeline`, `revieworder`, `depupgrade`) gate on `Contains`, which is expiry-aware.
- Replay protection on all three ContextVM methods: mandatory `expiration` within 15 minutes, service `p` tag, `method` tag, ingest handler-completion dedup by event id, **and** durable per-command idempotency independent of ingest: enroll/unenroll are ordered commands over one row (a redelivered command is a no-op by event id), and `monitoring/operator` persists the last applied command's event id atomically with the state it produced, so a retried rollback cannot swap the operators again (`TestOperatorControlRedeliveredCommandIsIdempotent`). The expiration window bounds *how long* a captured request stays valid; idempotency makes redelivery within the window harmless.
- Live reconfiguration never touches the signer, database handle, Qdrant client, or queued work; only the listener's subscription set is cancelled and reopened (`TestRunResubscribesWhenRoutesChangeWithoutRestart`, `TestRunDoesNotResubscribeWhenRoutingIsUnchanged`, `TestGRASPStyleRepositoryIsReviewedFromItsOwnRelayWithoutRestart`).

## Interoperability status

| Forge | Status |
|---|---|
| GRASP | Covered by the repository-native test above: 30617 is the authority for maintainers and relays; patches delivered only on the declared relay are reviewed; relay replacement and withdrawal apply live. |
| Budabit (Communikeys, Loom CI) | Ordinary NIP-34 authority remains unchanged. Default-off Loom/Hive CI context now follows repository-scoped 5401 anchors, authenticated 5100 job lineage, and 5402/5101 claims; bounded exact-ID/author filters reuse live routing. Offline tests only; no execution/provenance or complete CI-inventory claim. See [interop review](forge-interop-loom-concord-2026-10-27.md). |
| Armada Concord | Default-off, per-repo review notices now use pinned CORD-01/03 channel-stream envelopes: 1059 / 20013 / unsigned 9. Operator-provisioned stream credentials make Drydock a key-holding channel participant, not an address-only non-member poster. Best-effort asynchronous delivery; offline shape/crypto tests, no live relay or Armada rendering acceptance. Rotation/policy sync remain manual. See [interop review](forge-interop-loom-concord-2026-10-27.md). |

## Not verified here

- The live canary (`scripts/monitoring-canary.sh`) was authored but **not executed**; it needs a running deployment, relay and keys. Its checks: container id, PID, signer pubkey, SQLite inode, Qdrant collections and queued-work count identical across enroll, relay replacement and unenroll, with `/readyz` 200 throughout and the `control plane changed; resubscribing` log line proving the live path.
- Payment internals (`payment.AuthorizePatch`) were used as-is; an active review subscription for `(author, repo)` satisfies the enrollment price, which is why a price requires a TTL.
