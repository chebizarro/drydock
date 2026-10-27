#!/usr/bin/env bash
# Live canary for maintainer-authorized monitoring.
#
# Proves, against a running Drydock, that enroll/unenroll and a relay
# replacement take effect with zero restart and leave every unrelated identity
# untouched: container and process identity, signer pubkey, SQLite database
# file, Qdrant collections, listener subscription, and queued review work.
# Every step is verified against the durable record (the request event id in
# monitoring_enrollments) and the log line carrying that id, never by a
# generic "something resubscribed" signal.
#
# Requirements: docker, nak, curl, jq, sqlite3. Run from the deployment host.
# Use a DISPOSABLE repository: relay replacement republishes its announcement
# (all tags preserved except `relays`) and restores the original afterwards,
# but the intermediate revision is visible to every consumer.
#
# Environment:
#   CANARY_CONTAINER       docker compose service/container name (default: drydock)
#   CANARY_HEALTH_URL      base URL of the management listener (default: http://127.0.0.1:9090)
#   CANARY_RELAY           relay Drydock reads and the forge publishes to (wss://...)
#   CANARY_DRYDOCK_PUBKEY  Drydock service pubkey (hex)
#   CANARY_MAINTAINER_SEC  secret (nsec/hex) of an owner or maintainer of CANARY_REPO_ADDR
#   CANARY_REPO_ADDR       30617:<owner-hex>:<identifier> whose announcement names the maintainer
#   CANARY_DB_PATH         host path to the SQLite database (inode, enrollment rows, queued work)
#   CANARY_QDRANT_URL      optional; Qdrant base URL to snapshot collection names
#   CANARY_NEW_RELAY       optional; exercise live relay replacement with this relay
#   CANARY_OWNER_SEC       secret of the repository owner (only for relay replacement)
#
# Usage: scripts/monitoring-canary.sh [--dry-run]
set -euo pipefail

CONTAINER="${CANARY_CONTAINER:-drydock}"
HEALTH="${CANARY_HEALTH_URL:-http://127.0.0.1:9090}"
DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing tool: $1" >&2; exit 1; }; }
for t in docker nak curl jq sqlite3; do need "$t"; done
for v in CANARY_RELAY CANARY_DRYDOCK_PUBKEY CANARY_MAINTAINER_SEC CANARY_REPO_ADDR CANARY_DB_PATH; do
  [[ -n "${!v:-}" ]] || { echo "set $v" >&2; exit 1; }
done

log() { printf '[canary %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() { echo "CANARY FAILED: $*" >&2; exit 2; }

# Address parts. Identifiers may contain colons: owner is field 2, identifier
# is everything after the second colon.
[[ "$CANARY_REPO_ADDR" == 30617:* ]] || fail "CANARY_REPO_ADDR must start with 30617:"
OWNER_HEX="$(printf '%s' "$CANARY_REPO_ADDR" | cut -d: -f2)"
IDENT="$(printf '%s' "$CANARY_REPO_ADDR" | cut -d: -f3-)"
[[ ${#OWNER_HEX} -eq 64 && -n "$IDENT" ]] || fail "malformed CANARY_REPO_ADDR"
MAINTAINER_HEX="$(nak key public "$CANARY_MAINTAINER_SEC")"

cid() { docker compose ps -q "$CONTAINER" 2>/dev/null || docker ps -qf "name=$CONTAINER"; }

# --- baseline -------------------------------------------------------------
sql() { sqlite3 -readonly "$CANARY_DB_PATH" "$1" || fail "sqlite query failed: $1"; }

snapshot() {
  local c pid signer inode qdrant queued
  c="$(cid)"; [[ -n "$c" ]] || fail "container $CONTAINER not found"
  pid="$(docker inspect -f '{{.State.Pid}}' "$c")"
  signer="$(docker logs "$c" 2>&1 | grep -o '"signer_pubkey":"[0-9a-f]*"' | tail -1 | cut -d'"' -f4)"
  [[ -n "$signer" ]] || fail "signer pubkey not found in logs"
  inode="$(stat -c %i "$CANARY_DB_PATH" 2>/dev/null || stat -f %i "$CANARY_DB_PATH")"
  qdrant=""
  if [[ -n "${CANARY_QDRANT_URL:-}" ]]; then
    qdrant="$(curl -fsS "$CANARY_QDRANT_URL/collections" | jq -c '[.result.collections[].name]|sort')" || fail "qdrant unreachable"
  fi
  # Identities of in-flight work, not a count: a lost item replaced by a new one
  # would otherwise cancel out.
  queued="$(sql "SELECT group_concat(patch_event_id || '/' || repo_id, ',') FROM (SELECT patch_event_id, repo_id FROM review_log WHERE status IN ('pending','reviewing') ORDER BY 1,2)")"
  printf '%s|%s|%s|%s|%s|%s\n' "$c" "$pid" "$signer" "$inode" "$qdrant" "$queued"
}

ready() { curl -fsS "$HEALTH/readyz" >/dev/null; }

# ContextVM request with the mandatory envelope. Prints the request event id.
contextvm() {
  local method="$1" params="$2" exp id out
  exp="$(( $(date +%s) + 300 ))"
  id="canary-$(date +%s%N)"
  out="$(nak event -k 25910 \
    -t "p=$CANARY_DRYDOCK_PUBKEY" -t "method=$method" -t "expiration=$exp" \
    -c "{\"jsonrpc\":\"2.0\",\"id\":\"$id\",\"method\":\"$method\",\"params\":$params}" \
    --sec "$CANARY_MAINTAINER_SEC" "$CANARY_RELAY")" || fail "publish $method"
  printf '%s' "$out" | jq -r '.id' | head -1
}

log_marker() { docker logs "$(cid)" 2>&1 | wc -l | tr -d ' '; }
# Wait for a log line containing both patterns, strictly after line $3.
await_log() {
  local p1="$1" p2="$2" since="$3" i
  for i in $(seq 1 60); do
    if docker logs "$(cid)" 2>&1 | tail -n +"$((since + 1))" | grep -F "$p1" | grep -Fq "$p2"; then return 0; fi
    sleep 1
  done
  return 1
}
await_resubscribe() { await_log 'control plane changed; resubscribing' '' "$1"; }

enrollment_row() {
  sql "SELECT request_event_id || '|' || revoked_at || '|' || revoke_event_id FROM monitoring_enrollments WHERE repository_addr='$CANARY_REPO_ADDR' AND author_pubkey='$MAINTAINER_HEX'"
}

if [[ $DRY_RUN -eq 1 ]]; then
  cat <<EOF
Plan:
  repo=$CANARY_REPO_ADDR owner=$OWNER_HEX maintainer=$MAINTAINER_HEX
  1. snapshot container/pid/signer/db-inode/qdrant/in-flight work; require /readyz 200
  2. monitoring/enroll; verify monitoring_enrollments.request_event_id == published id and revoked_at=0;
     await log "monitoring enrollment processed" with that id, then "control plane changed; resubscribing"
  3. snapshot again; all identities equal; /readyz 200
  4. relay replacement: ${CANARY_NEW_RELAY:-not configured} (re-signs the current 30617 with only the relays tag changed, then restores it)
  5. monitoring/unenroll; verify revoke_event_id == published id; await resubscription; snapshot equal
EOF
  exit 0
fi

ready || fail "/readyz not OK before start"
before="$(snapshot)"; log "baseline: $before"

m="$(log_marker)"
log "enrolling $CANARY_REPO_ADDR as $MAINTAINER_HEX"
enroll_id="$(contextvm monitoring/enroll "{\"repo_addr\":\"$CANARY_REPO_ADDR\"}")"
[[ ${#enroll_id} -eq 64 ]] || fail "could not read enroll event id"
await_log 'monitoring enrollment processed' "$enroll_id" "$m" || fail "enrollment $enroll_id not processed"
row="$(enrollment_row)"
[[ "$row" == "$enroll_id|0|" ]] || fail "durable enrollment mismatch: got '$row', want '$enroll_id|0|'"
await_resubscribe "$m" || fail "no live resubscription after enrollment"
ready || fail "/readyz regressed after enrollment"
after_enroll="$(snapshot)"
[[ "$before" == "$after_enroll" ]] || fail "identity drift after enrollment: $before vs $after_enroll"
log "enrollment $enroll_id applied with zero restart; identities unchanged"

if [[ -n "${CANARY_NEW_RELAY:-}" ]]; then
  [[ -n "${CANARY_OWNER_SEC:-}" ]] || fail "CANARY_NEW_RELAY requires CANARY_OWNER_SEC"
  [[ "$(nak key public "$CANARY_OWNER_SEC")" == "$OWNER_HEX" ]] || fail "CANARY_OWNER_SEC does not own $CANARY_REPO_ADDR"
  original="$(nak req -k 30617 -a "$OWNER_HEX" -d "$IDENT" --limit 1 "$CANARY_RELAY" | head -1)"
  [[ -n "$original" ]] || fail "current announcement not found on $CANARY_RELAY"
  # Re-sign the current announcement with only the relays tag replaced.
  republish() { # $1 = jq tags expression
    printf '%s' "$original" | jq -c "del(.id,.sig,.pubkey) | .created_at = (now|floor) | .tags |= ($1)" \
      | nak event --sec "$CANARY_OWNER_SEC" "$CANARY_RELAY" >/dev/null || fail "republish announcement"
  }
  m="$(log_marker)"
  log "replacing relay set -> $CANARY_NEW_RELAY (announcement otherwise preserved)"
  republish "(map(select(.[0] != \"relays\")) + [[\"relays\", \"$CANARY_NEW_RELAY\"]])"
  await_resubscribe "$m" || fail "no live resubscription after relay replacement"
  after_relay="$(snapshot)"
  [[ "$before" == "$after_relay" ]] || fail "identity drift after relay replacement"
  log "relay replacement applied with zero restart; restoring original relays"
  m="$(log_marker)"
  republish "."
  await_resubscribe "$m" || fail "no live resubscription after restoring the announcement"
fi

m="$(log_marker)"
log "unenrolling"
revoke_id="$(contextvm monitoring/unenroll "{\"repo_addr\":\"$CANARY_REPO_ADDR\"}")"
[[ ${#revoke_id} -eq 64 ]] || fail "could not read unenroll event id"
await_resubscribe "$m" || fail "no live resubscription after unenrollment"
row="$(enrollment_row)"
[[ "$row" == "$revoke_id|"* && "$row" != *"|0|"* ]] || fail "durable revocation mismatch: got '$row', want request '$revoke_id' with revoked_at != 0"
ready || fail "/readyz regressed after unenrollment"
after_unenroll="$(snapshot)"
[[ "$before" == "$after_unenroll" ]] || fail "identity drift after unenrollment"
log "CANARY PASSED: enroll ($enroll_id) / unenroll ($revoke_id)${CANARY_NEW_RELAY:+ and relay replacement} with zero restart; container, pid, signer, db, qdrant and in-flight work unchanged"
