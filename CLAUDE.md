# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

For Nostr protocol guidance, follow [AGENTS.md](AGENTS.md). Drydock uses an operator-authored NIP-51 kind-30001 list for reactive repository monitoring, kind `30078` for IDE session state, kind `25910` for ContextVM requests and notifications (including `review/order` and `marketplace/feedback`), kind `31990` for reviewer profiles, and NIP-59 gift-wrap (`1059`) for private payloads. NIP-90 kinds `5900`, `6900`, and `7000` are retired.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->


## Build & Test

Use the Makefile targets — they carry flags the bare `go` commands do not.

```bash
make ci          # vet + build + test (the gate CI runs)
make build       # CGO_ENABLED=1 go build ./...
make test        # CGO_ENABLED=1 go test -count=1 ./...
make test-nocgo  # CGO_ENABLED=0 path for internal/symbols
```

**`CGO_ENABLED=1` is required.** Tree-sitter symbol extraction is cgo-backed;
building without it silently falls back to the regex path that `cmd/drydock`
warns about at startup, so a plain `go test ./...` can pass while exercising
different code.

Running the race detector needs an extra flag because of a known upstream bug
(`fiatjaf.com/nostr` `writeJSONString` uses uintptr arithmetic that trips
`checkptr`; tracked as DRYDOCK-2h1):

```bash
go test -race -gcflags=all=-d=checkptr=0 ./...   # race detection stays enabled
```

Docker: `make up` / `make down` / `make logs`. The LSP bridge is a separate,
much larger image behind a compose profile: `docker compose --profile lsp up`.

## Architecture Overview

A single Go binary (`cmd/drydock`) that reviews code over Nostr. The flow is:

`internal/listener` subscribes to relays → `internal/ingest` routes events by
kind → `internal/pipeline` runs the review → `internal/publisher` signs and
publishes results back to relays. `internal/db` (SQLite via modernc) is the
only persistence layer. `cmd/drydock/main.go` is the single composition root —
every interface in the codebase gets its one production implementation wired
there.

Review work itself splits into `internal/reviewengine` (prompt assembly, LLM
client, structured-output parsing, ensembles), `internal/agenticreview` (the
tool-calling reviewer loop), and `internal/contextbuilder` (assembles the code
context bundle within a token budget). Static analysis lives in
`internal/securityscan`, `internal/nostrscan`, and `internal/betterleaks`;
`internal/securityverify` adversarially re-checks candidate findings before
publication.

Other entrypoints: `cmd/drydock-mcp` (MCP server), `cmd/lsp-bridge` (the
language-server sidecar), `cmd/drydock-eval` (offline evaluation).

## Conventions & Patterns

- **Event kinds** come from `internal/eventkind` — never write a numeric kind
  literal. **Relay URLs** come from config, never from a literal in service
  code.
- **Errors** wrap with `%w` and a `"verb noun: "` prefix; compare sentinels
  with `errors.Is`/`errors.As`, never `==` or a message substring.
  `internal/db/store_monitoring.go` and `store_review_orders.go` are the
  reference files for the persistence house style.
- **Paths from untrusted input** (patches, requests) must go through the
  package's confinement helper — `contextbuilder.readRepositoryFile` or
  `workspacesnapshot.normalizePath` — never a bare `filepath.Join`.
- **LLM responses** are parsed via `llmutil.ExtractJSON`; `response_format:
  json_object` is advisory on self-hosted endpoints, so fenced replies happen.
- **Patch/diff parsing** uses `github.com/bluekeyes/go-gitdiff` via
  `contextbuilder`'s patch analysis, which is the authoritative answer to
  "which files changed". Do not hand-roll a diff scanner.
- Prefer deleting over wrapping. A single-implementation interface that exists
  only so a test can mock it is not worth its declaration.
