# Deploying Drydock on Your Own Infrastructure

This guide takes you from an empty host to a Drydock instance that reviews NIP-34 patches with local models. Follow the steps in order. Each one ends with a check you can run, so a human or an agent can confirm it before moving on.

Related documents:

- [Local Models](local-models.md): choosing, serving and sizing the LLMs. Read it before step 1.
- [Configuration Reference](configuration.md): every environment variable.
- [Per-Repository Configuration](repo-config.md): `.drydock.yaml` options for reviewed repositories.

## What you are deploying

| Component | Required? | Where it runs | Purpose |
|-----------|-----------|---------------|---------|
| **drydock-core** | yes | Docker container or native binary | Listens to relays, prepares patches, runs reviews, publishes results |
| **LLM server(s)** | yes | GPU host: llama.cpp, Ollama or vLLM | OpenAI-compatible `/v1/chat/completions` for every review role |
| **Nostr relays** | yes | Yours or existing ones | Where patches arrive and reviews are published |
| **Signer** | yes, to publish | NIP-46 bunker (recommended) or an nsec file | Signs reviews as Drydock's identity |
| **Monitored-repositories list** | yes, for reviews | A Nostr event you publish | Chooses which repositories Drydock reviews automatically |
| **Qdrant + embedding server** | development: optional; production: required | Compose stack + GPU/CPU host | Retrieval of NIP specs, project docs, few-shot examples, code chunks |
| LSP bridge | optional | Compose profile `lsp` | Type-aware symbol analysis |
| Dependency-upgrade runner | optional | Compose profile `depupgrade` | Sandboxed dependency-upgrade proposals |

LLM servers are **not** part of the Compose stack. You run them separately and point Drydock at them.

## Development or production mode

`DRYDOCK_ENV` (or `DRYDOCK_PRODUCTION=true`) selects the mode. Drydock validates its configuration at startup and exits on any error, logging every problem it found.

| | Development (`DRYDOCK_ENV=development`) | Production (`DRYDOCK_ENV=production`) |
|---|---|---|
| LLM endpoints | Built-in defaults if unset; `http://` and localhost allowed | planner, coder32b, llm70b, coder14b and meta must be set explicitly, with an API key |
| Endpoint transport | Anything | Every configured LLM, Qdrant, embedding and registry URL must be `https://` and not loopback |
| Relays | Defaults to public relays | Must be explicit; the built-in public relays are rejected |
| `DRYDOCK_MONITORED_REPOS_AUTHOR` | Optional operator ceiling | Optional operator ceiling (warning when unset) |
| Signer | Optional (listen-only without one) | Required (bunker or nsec) |
| Qdrant and embeddings | Optional | URLs, models and API keys required |

Use development mode for a single trusted host where the model server is on the same machine or private network. Use production mode when endpoints cross untrusted networks or you want the startup guards. Production mode needs TLS on your model servers; see [Production TLS](#production-tls).

## Prerequisites

On the **Drydock host**:

- Docker with Compose v2.24 or newer (`docker compose version`). For a native build instead: Go 1.26+ with CGO and a C compiler, plus `git` and `ripgrep` on `PATH`. Install [betterleaks](https://github.com/betterleaks/betterleaks) if any repository enables secret scanning; the Docker image includes it.
- Outbound access to your relays and to the git hosts in the repositories' clone URLs.
- On first start, outbound HTTPS to `openaipublic.blob.core.windows.net` to download the tokenizer file, which is then cached. For air-gapped hosts see [Tokenizer cache](#tokenizer-cache).
- [`nak`](https://github.com/fiatjaf/nak) for publishing the monitored-repositories list and, optionally, running a bunker.

On the **GPU host** (it can be the same machine): a GPU and model server as described in [Local Models](local-models.md).

## Step 1: Serve the models

Follow [Local Models](local-models.md) to start a chat server and, if you want retrieval, an embedding server. The recommended starting point is one llama.cpp server running **qwen3.8-27b at Q4_K_M with thinking off**, used for every role.

Check that it answers from the Drydock host. Use the address the Drydock container will use. From inside Compose that is `host.docker.internal`, which the server reaches only if it listens on a non-loopback address.

```bash
# Add  -H "Authorization: Bearer $DRYDOCK_LLM_API_KEY"  to both if the server has an API key
curl -fsS http://GPU_HOST:8080/v1/models
curl -fsS http://GPU_HOST:8080/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"qwen3.8-27b","messages":[{"role":"user","content":"Reply with OK"}],"max_tokens":8}'
```

Both must succeed: `-f` makes curl fail on HTTP errors, including a wrong API key. The model `id` returned by `/v1/models` must match the `DRYDOCK_*_MODEL` values you configure. Drydock logs `configured model name is not served by its endpoint` at startup when it doesn't.

## Step 2: Create Drydock's Nostr identity and signer

Drydock signs its reviews with its own key. You also need an **operator key**: your own identity, which publishes the list of repositories to review. They can be the same key, but separate keys are easier to rotate.

```bash
nak key generate                      # Drydock's secret key (hex); store it in your secret manager
nak key public <hex-secret>           # its public key
nak encode npub <hex-public>          # npub form, to share
```

Choose how Drydock signs, in order of preference:

1. **A NIP-46 bunker.** The key stays in the signer, and Drydock receives only a `bunker://` URL. Use any NIP-46 signer you trust, or run one with `nak`:

   ```bash
   # On a host you trust (keeps running; --persist remembers authorized clients)
   nak bunker --sec <drydock-hex-secret> --persist -s <random-connection-secret> wss://your-relay.example
   ```

   The command prints the `bunker://<signer-pubkey>?relay=...&secret=...` URL. Set it as `DRYDOCK_SIGNER_BUNKER_URL`.
2. **An nsec file.** The container runs as uid 1000, so the file must be readable by that uid. Compose automatically merges a `docker-compose.override.yml` next to `docker-compose.yml`, which is the place for the mount:

   ```bash
   mkdir -p secrets && printf '%s\n' '<nsec-or-hex-secret>' > secrets/drydock-nsec
   sudo chown 1000:1000 secrets/drydock-nsec && chmod 400 secrets/drydock-nsec
   cat > docker-compose.override.yml <<'YAML'
   services:
     drydock-core:
       volumes:
         - ./secrets/drydock-nsec:/run/secrets/drydock-nsec:ro
   YAML
   ```

   Then set `DRYDOCK_SIGNER_NSEC_FILE=/run/secrets/drydock-nsec`, the path **inside** the container. For native runs, use the host path, owned by the service user. If the file can't be read, Drydock only logs `failed to read signer nsec file` and continues with no signer, so check for that line. Avoid putting the key itself in `.env` via `DRYDOCK_SIGNER_NSEC`.

If both are set, the bunker wins. Production mode refuses to start without a signer. Development mode starts listen-only, ingesting events without publishing.

## Step 3: Choose what Drydock reviews

Drydock reviews NIP-34 patches (kind 1617) and pull requests (kinds 1618/1619) for repositories whose **maintainers have opted in**. A repository is monitored when its kind-30617 owner, or a pubkey in its `maintainers` tag, publishes a NIP-51 kind 30001 list that names Drydock's pubkey in a `p` tag and the repository in an `a` tag — or sends an authenticated `monitoring/enroll` request. Authority follows the latest announcement, so dropping a maintainer withdraws their enrollments. Until the announcement is known and an opt-in exists, nothing is reviewed: that's fail-closed.

A maintainer opts a repository in with:

```bash
nak event -k 30001 -d drydock:monitored-repositories:v1 \
  -t p=<drydock-hex-pubkey> \
  -t a=30617:<owner-hex>:<repo-identifier> \
  --sec <maintainer-secret> wss://your-relay.example
```

Optionally, you as the operator can add a **ceiling**: set `DRYDOCK_MONITORED_REPOS_AUTHOR` to your key and publish the same kind of list (no `p` tag needed) naming the repositories you are willing to review. Only repositories in both the maintainer opt-ins and your list are monitored; a deleted or malformed ceiling list monitors nothing. The ceiling can be rotated live with `monitoring/operator` (see the [Configuration Reference](configuration.md#live-operator-ceiling)). `DRYDOCK_REPO_ALLOWLIST` and `DRYDOCK_REPO_OWNER_ALLOWLIST` remain static hard limits.

To find a repository's address: `nak req -k 30617 -a <owner-hex-pubkey> wss://relay.example | jq -r '"30617:" + .pubkey + ":" + (.tags[] | select(.[0]=="d") | .[1])'`.

**Check:** the startup log line `loaded monitored repository registry` shows `repository_count` > 0 once an opt-in (and, if configured, your ceiling list) has arrived.

## Step 4: Configure

```bash
git clone https://git.sharegap.net/cascadia/drydock.git
cd drydock
cp .env.example .env              # development
# or: cp .env.production.example .env
```

Edit `.env`. At minimum set:

| Variable | Value |
|----------|-------|
| `DRYDOCK_RELAYS` | Your relays, comma-separated (`wss://...`) |
| `DRYDOCK_MONITORED_REPOS_AUTHOR` | Operator npub or hex (step 3) |
| `DRYDOCK_SIGNER_BUNKER_URL` or `DRYDOCK_SIGNER_NSEC_FILE` | Step 2. Keep the bunker URL in single quotes (`'bunker://...&secret=...'`); unquoted `&` breaks native runs |
| `DRYDOCK_*_BASE_URL`, `DRYDOCK_*_MODEL` (planner, coder32b, llm70b, coder14b, meta) | Your model server and served model name (step 1) |
| `DRYDOCK_LLM_API_KEY` | The server's API key, if it has one (required in production) |
| `LSP_BRIDGE_AUTH_TOKEN`, `DEP_RUNNER_AUTH_TOKEN` | Long random strings. Compose interpolates them even when their profiles are off |

Recommended for local GPUs, and already set in `.env.example`:

- `DRYDOCK_AGENTIC_REVIEW_FALLBACK=true` uses the single-shot review path. The default agentic path sends 100k–1M prompt tokens per review across many turns. Enable it only after measuring it on your hardware (see [Local Models](local-models.md#agentic-or-single-shot-review)).
- `DRYDOCK_LLM_TIMEOUT` gives slow GPUs enough time per call. Size it as described in [Local Models](local-models.md#timeouts-and-token-caps).
- `DRYDOCK_PIPELINE_WORKERS` should match the number of requests your model server can serve in parallel.

`.env` holds secrets. Keep it out of version control (it is gitignored) and readable only by the service user.

**Check:** `docker compose config --quiet` exits 0. For native runs, use `set -a; . ./.env; set +a` and check that it raises no shell errors.

## Step 5: Run

### Docker Compose (recommended)

```bash
docker compose up -d --build                       # drydock-core + qdrant
docker compose --profile lsp up -d --build         # optional: + LSP bridge
docker compose logs -f drydock-core
```

Compose passes every variable in `.env` to drydock-core. It publishes the management listener (health, readiness, metrics, dashboard) on host loopback port 8081, and Qdrant on loopback 6333.

To reach a model server on the Docker host, use `http://host.docker.internal:<port>/v1` in `.env`. The compose file maps that name to the host gateway. The server must listen on the host's bridge-reachable address, not only `127.0.0.1`.

### Native binary

```bash
CGO_ENABLED=1 go build -o bin/drydock ./cmd/drydock
CGO_ENABLED=1 go build -o bin/drydock-eval ./cmd/drydock-eval
set -a; . ./.env; set +a
# .env.example uses container paths and hostnames; replace them for a native process:
export DRYDOCK_DATABASE_URL='file:./data/drydock.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)'
export DRYDOCK_REPO_CACHE_DIR=./data/repos TIKTOKEN_CACHE_DIR=./data/tiktoken-cache
export DRYDOCK_EVAL_DATASET_PATH="$PWD/eval/heldout-sample.json"
export DRYDOCK_HEALTH_ADDR=127.0.0.1:8081
mkdir -p data/repos
./bin/drydock
```

Also replace `host.docker.internal` in the `DRYDOCK_*_BASE_URL` values with the model server's address (for example `127.0.0.1`). For retrieval, `qdrant` resolves only inside Compose: run Qdrant with `docker compose up -d qdrant` (published on host loopback) and set `DRYDOCK_QDRANT_URL=http://127.0.0.1:6333`, or leave retrieval off.

To run it as a service, a minimal systemd unit:

```ini
# /etc/systemd/system/drydock.service
[Unit]
Description=Drydock code review
After=network-online.target
Wants=network-online.target

[Service]
User=drydock
WorkingDirectory=/opt/drydock
EnvironmentFile=/opt/drydock/.env
ExecStart=/opt/drydock/bin/drydock
Restart=on-failure
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

systemd's `EnvironmentFile` accepts the quoted values used in `.env.example`. The template's paths point at `/data/...` (the container layout): create that directory for the `drydock` user, or edit `DRYDOCK_DATABASE_URL`, `DRYDOCK_REPO_CACHE_DIR` and `TIKTOKEN_CACHE_DIR`.

## Step 6: Verify

1. **Startup validation passed.** The logs contain `configuration validation passed`. Any `configuration warning` lines explain degraded features. See [Troubleshooting](#troubleshooting) for errors.
2. **Readiness.** `curl -fsS http://127.0.0.1:8081/readyz` returns `200` once the database is open and, with `DRYDOCK_MONITORED_REPOS_AUTHOR` set, the monitored list has loaded.
3. **Signer and list.** The logs show `NIP-46 bunker signer ready` (or `local nsec signer ready`), and `loaded monitored repository registry` with your `repository_count`.
4. **Models reachable and named correctly.** There should be no `LLM endpoint ... not reachable` warnings and no `configured model name is not served by its endpoint` warnings. `could not verify served models for LLM endpoint` means the model list couldn't be fetched at all: wrong URL, server down, or rejected credentials. The startup reachability check counts any HTTP status below 500, including `401`, as reachable, so rely on the authenticated `curl` checks from step 1, and on step 5, to confirm keys work.
5. **Model wiring end to end.** Run the evaluation harness, which sends a small labelled dataset through the real planner → reviewer → walkthrough path:

   ```bash
   make eval                                   # Docker: one-off container with DRYDOCK_MODE=eval
   # or natively: ./bin/drydock-eval
   ```

   It prints recall and false-positive metrics as JSON. A run that completes proves the endpoints, model names and JSON output work. To compare models properly, use `drydock-bench` ([Evaluation](eval.md#model-benchmarking-drydock-bench)).
6. **A real review.** Open a small patch (kind 1617) or PR on a monitored repository. Drydock publishes a kind-1111 review comment on it, signed by its key, to `DRYDOCK_WRITE_RELAYS` (or `DRYDOCK_RELAYS`). Follow it in `docker compose logs -f drydock-core`.

## Production TLS

Production mode requires every configured endpoint to use `https://` on a non-loopback host, and requires API keys. For self-hosted services:

- **llama.cpp** serves TLS and checks keys itself:

  ```bash
  llama-server ... --host 0.0.0.0 --port 8443 \
    --ssl-cert-file /etc/llama/tls.crt --ssl-key-file /etc/llama/tls.key \
    --api-key-file /etc/llama/api-keys
  ```

  Then set `DRYDOCK_*_BASE_URL=https://gpu-host.example:8443/v1` and `DRYDOCK_LLM_API_KEY` to one of the keys.
- **Ollama and vLLM**: run them behind a TLS reverse proxy (Caddy, nginx) that also enforces a bearer token.
- **Qdrant**: the bundled `qdrant` service is plain HTTP with no API key, which suits development only. For production, start the stack with the overlay `docker-compose.production.yml`. It gives Qdrant your API key (`DRYDOCK_QDRANT_API_KEY`) and TLS, and points Drydock at a CA bundle:

  ```bash
  docker compose -f docker-compose.yml -f docker-compose.production.yml up -d --build
  ```

  It expects `./tls/qdrant/tls.crt` and `tls.key`, a certificate whose subject alternative name is `qdrant`, plus `./tls/ca-bundle.pem`. Set `DRYDOCK_QDRANT_URL=https://qdrant:6333`. Alternatively, use an external Qdrant that already serves HTTPS with an API key.
- **A private CA.** Drydock is a Go program, and `SSL_CERT_FILE` **replaces** its system root store. The bundle must therefore contain the public roots (for relays, git hosts and public endpoints) **plus** your CA:

  ```bash
  mkdir -p tls && cat /etc/ssl/certs/ca-certificates.crt my-private-ca.pem > tls/ca-bundle.pem
  ```

  The overlay mounts it and sets `SSL_CERT_FILE`. Certificates for your model servers must name the host Drydock uses in the URL.

The production hardening checklist:

- [ ] `DRYDOCK_ENV=production`, explicit private relays, a bunker signer, and secrets loaded from a secret manager.
- [ ] `DRYDOCK_DASHBOARD_BEARER_TOKEN` is set if the management listener is reachable beyond loopback. `/healthz`, `/readyz` and `/metrics` stay unauthenticated, for probes.
- [ ] Backups of the `drydock_data` and `qdrant_storage` volumes (see [Operations](#operations)).
- [ ] `DRYDOCK_LLM_TIMEOUT` and `DRYDOCK_LLM_MAX_TOKENS` are sized for your hardware.
- [ ] One instance per database. SQLite is single-writer, so never run two Drydocks on one database file.
- [ ] The restart policy matches your platform (`restart: unless-stopped` in Compose).
- [ ] Alerts on `drydock_review_apply_failure_ratio`, `drydock_review_model_none_publication_ratio` and `drydock_meta_review_failure_ratio` from `/metrics`.

## Operations

### Volumes and backups

| Volume | Mount | Contents |
|--------|-------|----------|
| `drydock_data` | `/data` | SQLite database (`drydock.db` plus `-wal`/`-shm`), repository cache, review snapshots, tokenizer cache |
| `qdrant_storage` | `/qdrant/storage` | Vector collections |

Stop the stack, archive both volumes, then start it again:

```bash
docker compose stop
docker run --rm -v drydock_drydock_data:/data -v "$PWD":/backup alpine tar czf /backup/drydock-data.tgz -C /data .
docker run --rm -v drydock_qdrant_storage:/q -v "$PWD":/backup alpine tar czf /backup/qdrant.tgz -C /q .
docker compose start
```

Compose prefixes volume names with the project name (`drydock_` when the checkout directory is `drydock`). Check with `docker volume ls`.

### Upgrades

`git pull`, then `docker compose up -d --build`. The database schema migrates automatically at startup. Read the commit log for configuration changes; new settings come with defaults, and startup validation reports anything missing.

Upgrading from a release where `DRYDOCK_MONITORED_REPOS_AUTHOR` granted membership: run the [monitoring inventory](configuration.md#migration-from-the-single-trusted-author) first. Repositories in your list without a maintainer opt-in stop being reviewed until a maintainer publishes one; nothing else changes and nothing is expanded.

### Tokenizer cache

Drydock counts prompt tokens with the `cl100k_base` encoding. It downloads the encoding file once and caches it in `TIKTOKEN_CACHE_DIR` (Compose default `/data/tiktoken-cache`, on the data volume). It is needed in **every** mode: startup exits with `failed to initialize authoritative agentic tokenizer` if it can't load, even with `DRYDOCK_AGENTIC_REVIEW_FALLBACK=true`. For an air-gapped host, start Drydock once on a connected machine with the same `TIKTOKEN_CACHE_DIR`, then copy that directory over.

### Capacity

- `DRYDOCK_PIPELINE_WORKERS` sets the number of concurrent reviews. Each review makes about 3 sequential LLM calls (planner, reviewer, walkthrough) on the single-shot path. Match workers to the parallel slots your model servers have (llama-server `-np`).
- `DRYDOCK_REPO_CACHE_MAX_SIZE_MB` and `DRYDOCK_REPO_CACHE_MAX_COUNT` bound the repository cache; repositories range from 10 MB to several GB.
- See [Scaling](scaling.md) for multi-server layouts.

### Management listener

`DRYDOCK_HEALTH_ADDR` serves `/healthz` (liveness), `/readyz` (readiness), `/metrics` (Prometheus), `/dashboard/` and `/api/*`. The dashboard and API are unauthenticated unless `DRYDOCK_DASHBOARD_BEARER_TOKEN` is set, so keep the listener on loopback, or set the token:

```bash
curl -H "Authorization: Bearer $DRYDOCK_DASHBOARD_BEARER_TOKEN" http://127.0.0.1:8081/api/stats
```

## Troubleshooting

Startup validation messages and what to do about them:

| Message (in the logs) | Fix |
|---|---|
| `no relays configured` | Set `DRYDOCK_RELAYS` (or `DRYDOCK_READ_RELAYS`) |
| `production mode requires DRYDOCK_RELAYS or both DRYDOCK_READ_RELAYS and DRYDOCK_WRITE_RELAYS ...` / `must not use built-in public relay default` | Configure your own relays explicitly |
| `production mode requires DRYDOCK_MONITORED_REPOS_AUTHOR` / `must be a valid hex or npub public key` | Step 3 |
| `no signer configured` (error in production) | Step 2 |
| `production mode requires DRYDOCK_<ROLE>_BASE_URL to be explicitly configured` | Set every core role's URL and model, even if they are identical |
| `production mode requires an API key for <role> LLM endpoint` | Set `DRYDOCK_LLM_API_KEY` or the per-role key |
| `production mode requires ... to use https://` / `must not use localhost/loopback URL` | [Production TLS](#production-tls), or run in development mode |
| `production mode requires DRYDOCK_QDRANT_URL / _API_KEY / DRYDOCK_EMBED_* ...` | Production needs retrieval configured |
| `DRYDOCK_LLM_TIMEOUT must be a positive Go duration ...` | Use a unit, e.g. `300s`, at most `30m` |
| `database validation failed` | The `DRYDOCK_DATABASE_URL` directory must exist and be writable by the service user (uid 1000 in the image) |
| `required variable LSP_BRIDGE_AUTH_TOKEN is missing a value` (Compose) | Set both sidecar tokens in `.env`, even if the profiles are off |

Runtime problems:

| Symptom | Likely cause |
|---|---|
| `/readyz` stays `503` | The monitored list hasn't loaded: it isn't published to Drydock's read relays, or it has the wrong `d` tag or author |
| Reviews never start | The repository isn't in the monitored list, or there is no signer (the logs say `pipeline runner disabled`) |
| `LLM endpoint ... not reachable` from the container | The model server listens only on `127.0.0.1`, or `host.docker.internal` isn't routable. Bind the server to the bridge-reachable address |
| Reviews fail with context-length errors | The served context is smaller than Drydock's prompt. Lower `DRYDOCK_CONTEXT_TOKEN_BUDGET` (e.g. `24000` for a 32k server) or raise the server's context; see [Local Models](local-models.md#context-size) |
| Reviews time out | Raise `DRYDOCK_LLM_TIMEOUT`, turn thinking off, or use a faster model; see [Local Models](local-models.md#timeouts-and-token-caps) |
| Many JSON repair calls in the logs (`structured llm output invalid`) | The model is weak at strict JSON. Use a recommended model from [Local Models](local-models.md) |
| `failed to initialize authoritative agentic tokenizer` at startup | No outbound access to download the tokenizer and no cached copy; see [Tokenizer cache](#tokenizer-cache) |
