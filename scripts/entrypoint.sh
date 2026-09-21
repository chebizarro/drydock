#!/usr/bin/env sh
set -eu

MODE="${DRYDOCK_MODE:-listener}"

case "$MODE" in
  listener|drift-guard)
    # main.go re-reads DRYDOCK_MODE and dispatches to runDriftGuard, which
    # parses os.Args[1:]. Forward "$@" so `drydock <subcommand> ...` is not
    # silently discarded.
    exec /usr/local/bin/drydock "$@"
    ;;
  eval)
    exec /usr/local/bin/drydock-eval "$@"
    ;;
  *)
    echo "Unknown DRYDOCK_MODE='$MODE'. Use 'listener', 'eval', or 'drift-guard'." >&2
    exit 1
    ;;
esac

