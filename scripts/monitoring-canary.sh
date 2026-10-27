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
# Requirements: nak, curl, jq, sqlite3 (and docker in Docker mode).
# Run from the deployment host.
# Keep a kind-25910 response subscription addressed to the maintainer open
# before running (e.g. nak req --stream -k 25910 -p <maintainer-hex>
# -a <drydock-hex> <relay>). Ephemeral relays may reject unobserved replies.
# Use a DISPOSABLE repository: relay replacement republishes its announcement
# (all tags preserved except `relays`) and restores the original afterwards,
# but the intermediate revision is visible to every consumer.
#
# Environment:
#   CANARY_MODE            docker (default) or native
#   CANARY_PID             running Drydock PID (required in native mode)
#   CANARY_LOG_FILE        Drydock JSON log file (required in native mode)
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

MODE="${CANARY_MODE:-docker}"
CONTAINER="${CANARY_CONTAINER:-drydock}"
HEALTH="${CANARY_HEALTH_URL:-http://127.0.0.1:9090}"
DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing tool: $1" >&2; exit 1; }; }
for t in nak curl jq sqlite3; do need "$t"; done
case "$MODE" in
  docker) need docker ;;
  native)
    [[ "${CANARY_PID:-}" =~ ^[1-9][0-9]*$ ]] || { echo "set CANARY_PID to the running Drydock PID" >&2; exit 1; }
    [[ -r "${CANARY_LOG_FILE:-}" ]] || { echo "set CANARY_LOG_FILE to a readable log file" >&2; exit 1; }
    ;;
  *) echo "CANARY_MODE must be docker or native" >&2; exit 1 ;;
esac
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
logs() {
  if [[ "$MODE" == native ]]; then cat "$CANARY_LOG_FILE";
  else docker logs "$(cid)" 2>&1; fi
}

# --- baseline -------------------------------------------------------------
sql() { sqlite3 -readonly "$CANARY_DB_PATH" "$1" || fail "sqlite query failed: $1"; }

snapshot() {
  local c pid signer inode qdrant queued
  if [[ "$MODE" == native ]]; then
    c=native
    pid="$CANARY_PID"
    kill -0 "$pid" 2>/dev/null || fail "Drydock PID $pid is not running"
  else
    c="$(cid)"; [[ -n "$c" ]] || fail "container $CONTAINER not found"
    pid="$(docker inspect -f '{{.State.Pid}}' "$c")"
  fi
  signer="$(logs | grep -o '"signer_pubkey":"[0-9a-f]*"' | tail -1 | cut -d'"' -f4)"
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

log_marker() { logs | wc -l | tr -d ' '; }
# Wait for a log line containing both patterns, strictly after line $3.
await_log() {
  local p1="$1" p2="$2" since="$3" i
  for ((i = 0; i < 60; i++)); do
    ready || fail "/readyz regressed while awaiting $p1"
    if logs | tail -n +"$((since + 1))" | grep -F "$p1" | grep -F "$p2" >/dev/null; then return 0; fi
    sleep 1
  done
  return 1
}
await_resubscribe() { await_log 'control plane changed; resubscribing' '' "$1"; }
assert_processed() {
  if logs | tail -n +"$(($2 + 1))" | grep -F 'failed to process event' | grep -F "$1" >/dev/null; then
    fail "request $1 failed during processing; check the Drydock log (including response publication)"
  fi
}

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
log "durable enrollment: $row"
await_resubscribe "$m" || fail "no live resubscription after enrollment"
assert_processed "$enroll_id" "$m"
ready || fail "/readyz regressed after enrollment"
after_enroll="$(snapshot)"
[[ "$before" == "$after_enroll" ]] || fail "identity drift after enrollment: $before vs $after_enroll"
log "enrollment $enroll_id applied with zero restart; identities unchanged"

if [[ -n "${CANARY_NEW_RELAY:-}" ]]; then
  [[ -n "${CANARY_OWNER_SEC:-}" ]] || fail "CANARY_NEW_RELAY requires CANARY_OWNER_SEC"
  [[ "$(nak key public "$CANARY_OWNER_SEC")" == "$OWNER_HEX" ]] || fail "CANARY_OWNER_SEC does not own $CANARY_REPO_ADDR"
  original="$(nak req -k 30617 -a "$OWNER_HEX" -d "$IDENT" --limit 1 "$CANARY_RELAY" | head -1)"
  [[ -n "$original" ]] || fail "current announcement not found on $CANARY_RELAY"
  # Strictly newer timestamps avoid NIP-01's event-id tie-break if both
  # replacements land in the same second.
  announcement_time="$(printf '%s' "$original" | jq -r '.created_at')"
  # Re-sign the current announcement with only the relays tag replaced.
  republish() { # $1 = jq tags expression
    local now
    now="$(date +%s)"
    announcement_time="$((announcement_time + 1))"
    if (( now > announcement_time )); then announcement_time="$now"; fi
    local out
    out="$(printf '%s' "$original" | jq -c --argjson at "$announcement_time" "del(.id,.sig,.pubkey) | .created_at = \$at | .tags |= ($1)" \
      | nak event --sec "$CANARY_OWNER_SEC" "$CANARY_RELAY")" || fail "republish announcement"
    announcement_id="$(printf '%s' "$out" | jq -r '.id')"
    [[ ${#announcement_id} -eq 64 ]] || fail "could not read announcement event id"
  }
  assert_announcement() {
    local stored
    stored="$(sql "SELECT announcement_event_id FROM repositories WHERE repo_id='$OWNER_HEX:$IDENT'")"
    [[ "$stored" == "$announcement_id" ]] || fail "durable announcement mismatch: got '$stored', want '$announcement_id'"
    log "announcement $announcement_id stored"
  }
  m="$(log_marker)"
  log "replacing relay set -> $CANARY_NEW_RELAY (announcement otherwise preserved)"
  republish "(map(select(.[0] != \"relays\")) + [[\"relays\", \"$CANARY_NEW_RELAY\"]])"
  await_resubscribe "$m" || fail "no live resubscription after relay replacement"
  assert_announcement
  ready || fail "/readyz regressed after relay replacement"
  after_relay="$(snapshot)"
  [[ "$before" == "$after_relay" ]] || fail "identity drift after relay replacement"
  log "relay replacement applied with zero restart; restoring original relays"
  m="$(log_marker)"
  republish "."
  await_resubscribe "$m" || fail "no live resubscription after restoring the announcement"
  assert_announcement
  ready || fail "/readyz regressed after restoring the announcement"
fi

m="$(log_marker)"
log "unenrolling"
revoke_id="$(contextvm monitoring/unenroll "{\"repo_addr\":\"$CANARY_REPO_ADDR\"}")"
[[ ${#revoke_id} -eq 64 ]] || fail "could not read unenroll event id"
await_resubscribe "$m" || fail "no live resubscription after unenrollment"
assert_processed "$revoke_id" "$m"
row="$(enrollment_row)"
[[ "$row" == "$revoke_id|"*"|$revoke_id" && "$row" != *"|0|"* ]] || fail "durable revocation mismatch: got '$row', want request '$revoke_id' with revoked_at != 0"
log "durable revocation: $row"
ready || fail "/readyz regressed after unenrollment"
after_unenroll="$(snapshot)"
[[ "$before" == "$after_unenroll" ]] || fail "identity drift after unenrollment"
log "final: $after_unenroll"
log "CANARY PASSED: enroll ($enroll_id) / unenroll ($revoke_id)${CANARY_NEW_RELAY:+ and relay replacement} with zero restart ($MODE); process, signer, db and in-flight work unchanged${CANARY_QDRANT_URL:+; qdrant collections unchanged}"
