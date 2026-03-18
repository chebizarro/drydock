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
  *)
    echo "Unknown DRYDOCK_MODE='$MODE'. Use 'listener' or 'eval'." >&2
    exit 1
    ;;
esac

