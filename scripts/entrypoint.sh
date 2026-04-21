#!/usr/bin/env sh
set -eu

MODE="${DRYDOCK_MODE:-listener}"

case "$MODE" in
  listener)
    exec /usr/local/bin/drydock
    ;;
  eval)
    exec /usr/local/bin/drydock-eval
    ;;
  nip-ingest)
    exec /usr/local/bin/drydock
    ;;
  *)
    echo "Unknown DRYDOCK_MODE='$MODE'. Use 'listener', 'eval', or 'nip-ingest'." >&2
    exit 1
    ;;
esac

