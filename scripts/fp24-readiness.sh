#!/usr/bin/env bash
set -euo pipefail

# Read-only readiness and relay-evidence verifier for fp-24.
# Secrets are consumed from the environment and are never printed.

LEMMY_LLM_URL="${LEMMY_LLM_URL:-http://192.168.40.110:8080}"
LEMMY_EMBED_URL="${LEMMY_EMBED_URL:-http://192.168.40.110:8001}"
CHARTROOM_URL="${CHARTROOM_URL:-http://192.168.40.104:8087}"
BAHIA_URL="${BAHIA_URL:-http://192.168.40.104:8080}"
NOSTR_RELAY="${NOSTR_RELAY:-wss://relay.sharegap.net}"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "FAIL missing required command: $1" >&2
    exit 1
  }
}

check_health() {
  local name="$1"
  local url="$2"
  curl --fail --silent --show-error --max-time 10 "$url" >/dev/null
  echo "PASS $name $url"
}

check_model() {
  local name="$1"
  local url="$2"
  local pattern="$3"
  local response
  response="$(curl --fail --silent --show-error --max-time 10 "$url/v1/models")"
  jq -e --arg pattern "$pattern" '
    [(.models[]?.name // empty), (.data[]?.id // empty)]
    | any(test($pattern; "i"))
  ' <<<"$response" >/dev/null
  echo "PASS $name model matches $pattern"
}

check_no_nsec() {
  if [[ -n "${DRYDOCK_SIGNER_NSEC:-}" || -n "${DRYDOCK_SIGNER_NSEC_FILE:-}" ]]; then
    echo "FAIL local Drydock signing secret is configured" >&2
    exit 1
  fi
  if [[ -z "${DRYDOCK_SIGNER_BUNKER_URL:-}" ]]; then
    echo "FAIL DRYDOCK_SIGNER_BUNKER_URL is not configured" >&2
    exit 1
  fi
  echo "PASS signer configuration is bunker-only"
}

check_chartroom_search() {
  if [[ -z "${DRYDOCK_CHARTROOM_TOKEN:-}" ]]; then
    echo "SKIP authenticated Chartroom search (DRYDOCK_CHARTROOM_TOKEN is unset)"
    return
  fi
  local body response
  body='{"query":"NIP-46 remote signing","mode":"hybrid","limit":3}'
  response="$(curl --fail --silent --show-error --max-time 20 \
    -H "Authorization: Bearer ${DRYDOCK_CHARTROOM_TOKEN}" \
    -H 'Content-Type: application/json' \
    --data "$body" "$CHARTROOM_URL/search")"
  jq -e '[(.results // .hits // .data // [])[]] | length > 0' <<<"$response" >/dev/null
  echo "PASS authenticated Chartroom search returned context"
}

fetch_event() {
  local id="$1"
  nak req --id "$id" --limit 1 "$NOSTR_RELAY" | jq -c 'select(.id != null)' | head -n 1
}

verify_relay_pair() {
  local review_id="$1"
  local audit_id="$2"
  local patch_id="${3:-}"
  local repo_id="${4:-}"
  local review audit

  review="$(fetch_event "$review_id")"
  audit="$(fetch_event "$audit_id")"
  [[ -n "$review" ]] || { echo "FAIL review event not found" >&2; exit 1; }
  [[ -n "$audit" ]] || { echo "FAIL audit event not found" >&2; exit 1; }

  nak verify <<<"$review"
  nak verify <<<"$audit"
  jq -e --arg id "$review_id" '.id == $id and .kind == 1111' <<<"$review" >/dev/null
  jq -e --arg id "$audit_id" --arg review "$review_id" '
    .id == $id and .kind == 4903
    and any(.tags[]; .[0] == "type" and .[1] == "review-published")
    and any(.tags[]; .[0] == "subject" and .[1] == $review)
  ' <<<"$audit" >/dev/null
  jq -e --arg pubkey "$(jq -r .pubkey <<<"$review")" '.pubkey == $pubkey' <<<"$audit" >/dev/null

  if [[ -n "$patch_id" ]]; then
    jq -e --arg patch "$patch_id" 'any(.tags[]; .[0] == "patch_event_id" and .[1] == $patch)' <<<"$audit" >/dev/null
  fi
  if [[ -n "$repo_id" ]]; then
    jq -e --arg repo "$repo_id" 'any(.tags[]; .[0] == "repo_id" and .[1] == $repo)' <<<"$audit" >/dev/null
  fi
  echo "PASS signed kind-1111 and paired kind-4903 relay evidence"
}

usage() {
  echo "usage: $0 preflight | verify <review-event-id> <audit-event-id> [patch-event-id] [repo-id]" >&2
  exit 2
}

need curl
need jq

case "${1:-preflight}" in
  preflight)
    check_health "Lemmy LLM" "$LEMMY_LLM_URL/health"
    check_health "Lemmy embeddings" "$LEMMY_EMBED_URL/health"
    check_model "Lemmy LLM" "$LEMMY_LLM_URL" 'gemma'
    check_model "Lemmy embeddings" "$LEMMY_EMBED_URL" 'nomic'
    check_health "Chartroom" "$CHARTROOM_URL/health"
    check_health "Bahia liveness" "$BAHIA_URL/health"
    check_health "Bahia readiness" "$BAHIA_URL/ready"
    check_no_nsec
    check_chartroom_search
    ;;
  verify)
    [[ $# -ge 3 ]] || usage
    need nak
    verify_relay_pair "$2" "$3" "${4:-}" "${5:-}"
    ;;
  *) usage ;;
esac
