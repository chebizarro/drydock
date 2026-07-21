# fp-24 readiness audit — 2026-07-21

This is preparation evidence only. `fp-24` remains blocked by `fp-23` and was
not started or closed by this audit. No production state was mutated.

## Ready prerequisites

- Lemmy (`192.168.40.110`) is reachable over SSH and has Docker 29.5.2.
- Lemmy has 81 GiB free disk and 76 GiB available RAM.
- `http://192.168.40.110:8080` is healthy and serves
  `google_gemma-4-26B-A4B-it-Q4_K_M.gguf`.
- `http://192.168.40.110:8001` is healthy and serves
  `nomic-embed-text-v1.5.Q8_0.gguf`; an OpenAI-compatible embedding request
  succeeded and returned a 768-dimensional vector.
- Lemmy can reach production Chartroom (`192.168.40.104:8087`) and
  `relay.sharegap.net:443`.
- Production Chartroom reports healthy Postgres, Qdrant, and object storage.
- Signet liveness is healthy, its keystore is available, it has an active relay,
  and live signing traffic proves the NIP-46 service is operational.

## Blocking prerequisites

1. Bahia liveness passes but readiness fails with HTTP 503. The reported
   failures are `bootstrap_ready: phase=init` and two stopped required runners
   (`assistant-session-recovery` and `docs-nostr-publisher`). This must be
   classified or repaired before Bahia is accepted as the fp-24 deployment
   control plane.
2. Bahia's server-managed runtime aliases currently include only
   `edge-01-docker` and `btc-01-docker`; there is no Lemmy runtime endpoint.
   Registering a protected `lemmy-docker` alias is required before a Bahia
   direct-runtime deployment can target Lemmy. Raw Docker coordinates must not
   be sent in public Nostr requests.
3. Signet `/ready` currently returns HTTP 503 even though `/health` passes and
   live NIP-46 work succeeds. The readiness semantics must be classified, and a
   dedicated Drydock identity/bunker connection with policies permitting kinds
   `1111` and `4903` must be provisioned and tested.
4. Gemma's GPU was at 100% utilization during the audit. This is not an outage,
   but fp-24 needs a quiet acceptance window or an explicit latency budget so a
   saturated shared model does not produce a false failure.
5. `fp-23` must close with authenticated Chartroom context and paired `4903`
   behavior before fp-24 formally starts.

## Repeatable gate

The read-only gate is `scripts/fp24-readiness.sh`.

```bash
export DRYDOCK_SIGNER_BUNKER_URL='bunker://...'
export DRYDOCK_CHARTROOM_TOKEN='...'
scripts/fp24-readiness.sh preflight
```

The script never prints secret values. It checks live inference/model identity,
Chartroom, Bahia health/readiness, bunker-only signing configuration, and an
authenticated Chartroom query.

After a real review, verify the signed relay pair with:

```bash
scripts/fp24-readiness.sh verify \
  <kind-1111-event-id> <kind-4903-event-id> <patch-event-id> <repo-id>
```

The verifier fetches both events from `wss://relay.sharegap.net`, validates
their signatures, requires kinds `1111` and `4903`, checks common author
identity, and proves the audit's `subject`, `patch_event_id`, and `repo_id`
correlation.

## Deployment acceptance sequence

1. Close `fp-23` and merge its accepted Drydock revision.
2. Make Bahia and Signet readiness either green or explicitly explain and fix
   their readiness contracts; do not waive unexplained 503 responses.
3. Register the server-managed Lemmy runtime endpoint in Bahia using protected
   transport material.
4. Provision the Drydock Signet identity and deliver the bunker URI through the
   approved secret path. Do not place it in Git or Nostr content.
5. Build the accepted Drydock revision and deploy it through Bahia on Lemmy.
6. Confirm the deployed environment has a bunker URI, Chartroom token, and no
   `DRYDOCK_SIGNER_NSEC` or private-key file.
7. Submit a real NIP-34 patch for an approved test repository.
8. Capture the Chartroom context provenance, Drydock logs, kind-`1111` review,
   paired kind-`4903` audit, Bahia runtime observation, and exact source/image
   revision.
9. Run the relay verifier above and record its passing output in fp-24 evidence.
