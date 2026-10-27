# Live monitoring canary — 2026-10-27

## Result and boundary

**PASS, local native process, two real loopback WebSocket relays.** Final run:
09:53:43–09:53:52 UTC (Drydock JSON timestamps are Brisbane, UTC+10).
Maintainer ContextVM enroll/unenroll and owner announcement relay replacement
and restoration all applied without restarting Drydock. No Go product changes
were needed. This is not hosted, production, or release acceptance.

Source: branch `task/drydock-canary-live-run-20261027`, base
`1b199ccd7bde47d8b402d38cb4a6572dedc0eb3a`, with this commit's script changes.
Host: Darwin 27.0.0 arm64; Go 1.26.2; Bash 3.2.57; nak 0.19.7;
SQLite CLI 3.54.0; Python 3.14.8; ShellCheck 0.11.0.
Docker was available (`docker info` exited 0), but this run deliberately tested
the new native mode. No Docker stack was started.

- PID **16459**, process start time **Tue Oct 27 19:48:35 2026**, executable
  `./.canary-run/drydock`: identical `ps` output before/after.
- Signer **6a90d6bea3000dfeac93194f3801f7f67951dd2f75028549703aa5736fc5c50b** unchanged.
- SQLite inode **407682749** unchanged. No database replacement or service restart.
- `/readyz`: **84/84 HTTP 200**, zero exceptions/non-200 responses, sampled
  approximately every 100 ms during the final run. All four components
  (`database`, `monitored_repositories`, `listener`, `pipeline`) reported `ok`.
  This is sampled evidence, not a guarantee about every instant between probes.
- Initial durable enrollment was revoked; final durable enrollment is revoked.
  Each successful command's event ID was checked against the live SQLite row.
- Owner's replacement and restoration event IDs were checked against
  `repositories.announcement_event_id`. EOSE-complete `nak req` readback after
  the run matched SQLite's winner. The restored event preserved the original
  owner, kind, tags and content; only timestamp, event ID and signature changed.
- In-flight `review_log` identities were the **empty set** throughout. Retention
  of nonempty/actively executing review work was **not exercised**. No patch
  was published and no LLM review was executed.
- Qdrant/retrieval was off; collection retention was **not exercised**.
  Docker execution was **not exercised** (only its dry-run path).
- No operator ceiling was configured; this tests maintainer authority, not
  operator rotation/paid enrollment/expiry/restart recovery.

## Commands and environment

All files and processes were created from the assigned worktree:

```text
/Users/bizarro/Documents/Projects/.repoprompt-worktrees/drydock/rp-agent-73e12dd6-task-drydock-canary-live-run-202-455dcaa7
```

The following are the setup/run commands, grouped for reproduction. Generated
private-key values are intentionally not retained. Fresh keys, PIDs, inodes and
event IDs will differ. No `nak` installation was needed. Run from a fresh shell
in this worktree; reserve ports 19547, 19548 and 19090 first.

```bash
make build
CGO_ENABLED=1 go build -o .canary-run/drydock ./cmd/drydock
mkdir -p .canary-run
chmod 700 .canary-run
umask 077
{
  printf 'export OWNER_SEC=%q\n' "$(nak key generate)"
  printf 'export CANARY_MAINTAINER_SEC=%q\n' "$(nak key generate)"
  printf 'export DRYDOCK_SIGNER_NSEC=%q\n' "$(nak key generate | nak encode nsec)"
} > .canary-run/keys.env
set -a; . .canary-run/keys.env; set +a
# Actual values: OWNER_SEC=<redacted>, CANARY_MAINTAINER_SEC=<redacted>,
# DRYDOCK_SIGNER_NSEC=<redacted>.

nak serve --hostname 127.0.0.1 --port 19547 >.canary-run/base-relay.log 2>&1 &
echo $! >.canary-run/base-relay.pid
nak serve --hostname 127.0.0.1 --port 19548 >.canary-run/repo-relay.log 2>&1 &
echo $! >.canary-run/repo-relay.pid

export DRYDOCK_ENV=development
export DRYDOCK_RELAYS=ws://127.0.0.1:19547
export DRYDOCK_HEALTH_ADDR=127.0.0.1:19090
export DRYDOCK_DATABASE_URL="file:$PWD/.canary-run/drydock.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)"
export DRYDOCK_REPO_CACHE_DIR="$PWD/.canary-run/repos"
export TIKTOKEN_CACHE_DIR="$PWD/.canary-run/tiktoken-cache"
./.canary-run/drydock >.canary-run/drydock.log 2>&1 &
echo $! >.canary-run/drydock.pid
# In a separate step, after the startup log shows the health server and
# Nostr listener starting, require this check before publishing anything:
curl -fsS http://127.0.0.1:19090/readyz

export CANARY_MODE=native
export CANARY_PID="$(cat .canary-run/drydock.pid)"
export CANARY_LOG_FILE="$PWD/.canary-run/drydock.log"
export CANARY_RELAY=ws://127.0.0.1:19547
export CANARY_HEALTH_URL=http://127.0.0.1:19090
export CANARY_OWNER_SEC="$OWNER_SEC"
export CANARY_DRYDOCK_PUBKEY="$(nak key public "$DRYDOCK_SIGNER_NSEC")"
export CANARY_REPO_ADDR="30617:$(nak key public "$OWNER_SEC"):monitoring-canary-20261027"
export CANARY_DB_PATH="$PWD/.canary-run/drydock.db"
# Move off the repository-only relay onto the existing base relay, then back.
export CANARY_NEW_RELAY=ws://127.0.0.1:19547

nak event -k 30617 -d monitoring-canary-20261027 \
  -t "maintainers=$(nak key public "$CANARY_MAINTAINER_SEC")" \
  -t relays=ws://127.0.0.1:19548 \
  -t clone=https://example.invalid/canary.git \
  -c 'Disposable local monitoring canary' \
  --sec "$OWNER_SEC" "$CANARY_RELAY" >.canary-run/original-announcement.json

# Required for nak serve's ephemeral-response delivery. Keep this subscription
# open, and confirm its scoped REQ in base-relay.log before running the canary.
nak req --verbose --stream -k 25910 \
  -p "$(nak key public "$CANARY_MAINTAINER_SEC")" \
  -a "$CANARY_DRYDOCK_PUBKEY" "$CANARY_RELAY" \
  >.canary-run/responses-final.jsonl 2>.canary-run/response-subscription-final.log &
echo $! >.canary-run/responses.pid
```

No other Drydock environment overrides were required. Model endpoints used the
unreachable development defaults (`127.0.0.1:11434/v1`, `:11435/v1`, `:11436/v1`);
validation warned but passed. No Qdrant URL, operator ceiling, static allowlist,
paid enrollment policy, or monitoring admin was set. Development mode allowed
loopback `ws://` repository relay routing. The owner key and maintainer key were
different; the maintainer pubkey was
`0e494b4708e0615da9e1074bc2013aac26f6d3ed7c9ca85d11dda33b92186569`.

The final observed run used this temporary Python harness, saved as
`.canary-run/run-observed.py`, then `python3 .canary-run/run-observed.py`.
The timer is only a health sampler, not event-delivery/completion logic.
The canary observes durable state/log effects; `nak req` history ends at EOSE.

```python
import datetime
import json
import subprocess
import threading
import urllib.request

stop = threading.Event()

def monitor():
    with open('.canary-run/readiness.jsonl', 'w') as f:
        while not stop.is_set():
            row = {'time': datetime.datetime.now(datetime.timezone.utc).isoformat()}
            try:
                with urllib.request.urlopen('http://127.0.0.1:19090/readyz', timeout=2) as r:
                    row.update(status=r.status, body=json.load(r))
            except Exception as e:
                row['error'] = str(e)
            f.write(json.dumps(row) + '\n')
            f.flush()
            stop.wait(0.1)

t = threading.Thread(target=monitor)
t.start()
try:
    with open('.canary-run/canary-final.log', 'w') as f:
        p = subprocess.Popen(['bash', 'scripts/monitoring-canary.sh'], stdout=subprocess.PIPE,
                             stderr=subprocess.STDOUT, text=True)
        for line in p.stdout:
            print(line, end='', flush=True)
            f.write(line)
            f.flush()
        code = p.wait()
finally:
    stop.set()
    t.join()
rows = [json.loads(line) for line in open('.canary-run/readiness.jsonl')]
summary = {'samples': len(rows), 'non_200': [r for r in rows if r.get('status') != 200],
           'first': rows[0], 'last': rows[-1]}
with open('.canary-run/readiness-summary.json', 'w') as f:
    f.write(json.dumps(summary, indent=2) + '\n')
print(json.dumps(summary))
raise SystemExit(code or bool(summary['non_200']))
```

Before and after the final run:

```bash
sqlite3 -readonly "$CANARY_DB_PATH" \
  'SELECT request_event_id, revoked_at, revoke_event_id FROM monitoring_enrollments;'
ps -p "$CANARY_PID" -o pid=,lstart=,command=
# 16459 Tue Oct 27 19:48:35 2026     ./.canary-run/drydock
```

Initial revoked row:

```text
3ac0a4c6cf3c9f8ae08e7a5dfd14e22d1a1234911ed692da0ed6df604fd2960d|1793094802|3ac0a4c6cf3c9f8ae08e7a5dfd14e22d1a1234911ed692da0ed6df604fd2960d
```

## Full final canary output (exit 0)

```text
[canary 09:53:43] baseline: native|16459|6a90d6bea3000dfeac93194f3801f7f67951dd2f75028549703aa5736fc5c50b|407682749||
[canary 09:53:43] enrolling 30617:679568bca6b36f68ba4de2fcaab355baae30624fca8f51cc7e552285f048853d:monitoring-canary-20261027 as 0e494b4708e0615da9e1074bc2013aac26f6d3ed7c9ca85d11dda33b92186569
connecting to 127.0.0.1:19547... ok.
publishing to ws://127.0.0.1:19547... success.
[canary 09:53:43] durable enrollment: 195948d215e374e7d8af5ed276d85f57da40012210476828fc54f211d8c0036a|0|
[canary 09:53:45] enrollment 195948d215e374e7d8af5ed276d85f57da40012210476828fc54f211d8c0036a applied with zero restart; identities unchanged
connecting to 127.0.0.1:19547... ok.
[canary 09:53:45] replacing relay set -> ws://127.0.0.1:19547 (announcement otherwise preserved)
connecting to 127.0.0.1:19547... ok.
publishing to ws://127.0.0.1:19547... success.
[canary 09:53:48] announcement e8b64625efc5bc5eab43c0b17dab70bcec43c479d8d0b7cbb4aa361c38a10536 stored
[canary 09:53:48] relay replacement applied with zero restart; restoring original relays
connecting to 127.0.0.1:19547... ok.
publishing to ws://127.0.0.1:19547... success.
[canary 09:53:50] announcement fb372b5427106e706c685227f95c637504b4b4f51cbcb73d9356d9f3350be761 stored
[canary 09:53:50] unenrolling
connecting to 127.0.0.1:19547... ok.
publishing to ws://127.0.0.1:19547... success.
[canary 09:53:52] durable revocation: 284dfb3c7f209fb9e72d870559d23d07067a39c75b84a023d73342e3b68cf657|1793094830|284dfb3c7f209fb9e72d870559d23d07067a39c75b84a023d73342e3b68cf657
[canary 09:53:52] final: native|16459|6a90d6bea3000dfeac93194f3801f7f67951dd2f75028549703aa5736fc5c50b|407682749||
[canary 09:53:52] CANARY PASSED: enroll (195948d215e374e7d8af5ed276d85f57da40012210476828fc54f211d8c0036a) / unenroll (284dfb3c7f209fb9e72d870559d23d07067a39c75b84a023d73342e3b68cf657) and relay replacement with zero restart (native); process, signer, db and in-flight work unchanged
```

Snapshot fields: mode/container, PID, signer pubkey, SQLite inode, optional
Qdrant collections, sorted pending/reviewing `patch_event_id/repo_id` identities.
The last two fields are empty, not evidence of exercised Qdrant/nonempty work.

## Drydock and relay evidence

Relevant raw Drydock lines (the final-run interval contained **zero ERRORs**):

```jsonl
{"time":"2026-10-27T19:48:35.932778+10:00","level":"INFO","msg":"tree-sitter symbol extraction available"}
{"time":"2026-10-27T19:48:35.935768+10:00","level":"INFO","msg":"configuration validation passed"}
{"time":"2026-10-27T19:48:35.959468+10:00","level":"INFO","msg":"local nsec signer ready"}
{"time":"2026-10-27T19:48:35.963352+10:00","level":"INFO","msg":"autofix loop suppression enabled","signer_pubkey":"6a90d6bea3000dfeac93194f3801f7f67951dd2f75028549703aa5736fc5c50b"}
{"time":"2026-10-27T19:48:39.94164+10:00","level":"INFO","msg":"health server listening","addr":"127.0.0.1:19090"}
{"time":"2026-10-27T19:53:43.689656+10:00","level":"INFO","msg":"monitoring enrollment processed","repo":"30617:679568bca6b36f68ba4de2fcaab355baae30624fca8f51cc7e552285f048853d:monitoring-canary-20261027","author":"0e494b4708e0615da9e1074bc2013aac26f6d3ed7c9ca85d11dda33b92186569","request_event_id":"195948d215e374e7d8af5ed276d85f57da40012210476828fc54f211d8c0036a","applied":true,"access_kind":"free","expires_at":0,"monitored":true}
{"time":"2026-10-27T19:53:45.69296+10:00","level":"INFO","msg":"control plane changed; resubscribing","filters":6}
{"time":"2026-10-27T19:53:48.026013+10:00","level":"INFO","msg":"control plane changed; resubscribing","filters":4}
{"time":"2026-10-27T19:53:50.194844+10:00","level":"INFO","msg":"control plane changed; resubscribing","filters":6}
{"time":"2026-10-27T19:53:52.358447+10:00","level":"INFO","msg":"control plane changed; resubscribing","filters":3}
```

The repository-only relay received real scoped subscriptions on enrollment and
restoration (not mocked delivery):

```text
    got request {"kinds":[1617,1618,1619],"since":1793094219,"#a":["30617:679568bca6b36f68ba4de2fcaab355baae30624fca8f51cc7e552285f048853d:monitoring-canary-20261027"]}
    got request {"kinds":[30617,30618],"authors":["679568bca6b36f68ba4de2fcaab355baae30624fca8f51cc7e552285f048853d"],"#d":["monitoring-canary-20261027"]}
```

The maintainer response subscription captured both final-run responses.
`nak verify < .canary-run/responses-final.jsonl` exited 0. Each response had an
`e` tag naming the corresponding request, and a `p` tag naming the maintainer:

| Request | Response event | Result |
| --- | --- | --- |
| `195948d215e374e7d8af5ed276d85f57da40012210476828fc54f211d8c0036a` | `1eae3ff7cb95f434ebcf7f5b9170fb03d0888650861dcd9201d962546d3ba807` | `state: enrolled`, `monitored: true`, `access_kind: free` |
| `284dfb3c7f209fb9e72d870559d23d07067a39c75b84a023d73342e3b68cf657` | `0eedfe60b3b058fb168ec9778862765a4e00ddf24b7d6ba685097c5bd395831b` | `state: revoked`, `monitored: false`, `access_kind: free` |

Readiness is not logged per request by the health server. The independent
sampler's first/last receipts were:

```json
{"time":"2026-10-27T09:53:43.497213+00:00","status":200,"body":{"status":"ready","components":[{"name":"database","status":"ok"},{"name":"monitored_repositories","status":"ok"},{"name":"listener","status":"ok"},{"name":"pipeline","status":"ok"}]}}
{"time":"2026-10-27T09:53:52.417363+00:00","status":200,"body":{"status":"ready","components":[{"name":"database","status":"ok"},{"name":"monitored_repositories","status":"ok"},{"name":"listener","status":"ok"},{"name":"pipeline","status":"ok"}]}}
```

Post-run `curl -sS -i http://127.0.0.1:19090/readyz` also returned `HTTP/1.1 200 OK`
at `Tue, 27 Oct 2026 09:54:16 GMT`, with the same ready body.

## Failure found and corrections

The first native run (09:49:28–09:49:36 UTC) changed durable state correctly
but falsely printed `CANARY PASSED` despite both response publications failing.
The local ephemeral relay rejected replies because the canary had not opened a
maintainer response subscription. Request IDs were
`f26507374237b058affb7b92bad8afc4312dee294efb54bff99be92eaa1cd7fd`
and `986545cfd098f7e5fa06ceac70f6a1e69a2a2e3a58fe3331b281bf6368934e7e`.
This is **not** counted as clean end-to-end acceptance. The original error receipt:

```jsonl
{"time":"2026-10-27T19:49:28.149756+10:00","level":"ERROR","msg":"failed to process event","event_id":"f26507374237b058affb7b92bad8afc4312dee294efb54bff99be92eaa1cd7fd","kind":25910,"relay":"ws://127.0.0.1:19547","error":"publish contextvm event ad1f261b739dc92ba1ccb35f6628643e9cbbf7a475552073381fd162f7fd40a9 failed: ws://127.0.0.1:19547: msg: mute: no one was listening for this"}
```

Script changes in `scripts/monitoring-canary.sh`:

- Add `CANARY_MODE=native`, PID liveness checks and log-file reads. Docker stays
  the default and retains its compose/container inspect/log path.
- Document the response-subscription prerequisite and reject request-specific
  `failed to process event` log receipts after resubscription, rather than
  treating a committed enrollment plus resubscription as complete success.
- Readiness checks during log observation and after both relay changes;
  consume grep output fully to avoid a `grep -q`/`pipefail` SIGPIPE false negative.
- Strictly increasing announcement timestamps avoid same-second replacement
  ordering by event-ID tie-break. Check each replacement/restoration's actual
  event ID against SQLite, not only a generic resubscribe line.
- Require the exact `revoke_event_id` as well as the winning request ID and a
  nonzero revocation timestamp. Print durable rows and the final snapshot.
- Do not report Qdrant retention unless its optional check was configured.

A live negative control stopped the response subscriber and reran the corrected
script. It exited **2**, with the following full output (no false green):

```text
[canary 09:52:33] baseline: native|16459|6a90d6bea3000dfeac93194f3801f7f67951dd2f75028549703aa5736fc5c50b|407682749||
[canary 09:52:33] enrolling 30617:679568bca6b36f68ba4de2fcaab355baae30624fca8f51cc7e552285f048853d:monitoring-canary-20261027 as 0e494b4708e0615da9e1074bc2013aac26f6d3ed7c9ca85d11dda33b92186569
connecting to 127.0.0.1:19547... ok.
publishing to ws://127.0.0.1:19547... success.
[canary 09:52:33] durable enrollment: 1b461ff7eba08a48db77c29df7095996b4607aac2364d5704bd8ac182ad1e102|0|
CANARY FAILED: request 1b461ff7eba08a48db77c29df7095996b4607aac2364d5704bd8ac182ad1e102 failed during processing; check the Drydock log (including response publication)
```

The subscriber was then reopened. A full clean run restored revoked state
before the final recorded run. All iterations used the same PID, signer and DB.
No relay mute/authority/readiness checks were weakened.

## Verification and cleanup

These gates all exited 0:

```bash
make ci >.canary-run/make-ci.log 2>&1
# CGO_ENABLED=1 go vet ./...
# CGO_ENABLED=1 go build ./...
# CGO_ENABLED=1 go test -count=1 ./...
bash -n scripts/monitoring-canary.sh
shellcheck scripts/monitoring-canary.sh
git diff --check
bash scripts/monitoring-canary.sh --dry-run
CANARY_MODE=docker bash scripts/monitoring-canary.sh --dry-run
```

Invalid `CANARY_MODE` and invalid native `CANARY_PID` each exited 1, as expected.
The live negative control above exited 2, as expected. There were no Go product
changes requiring a new Go regression test; `make ci` exercised the existing
monitoring/enrollment/listener tests with CGO enabled.

Navigation used Jev `find-lines` for configuration, deployment, composition,
interop, registry and enrollment: 17 requests, 132265 input tokens, approximately
$0.00556. Its partial handler result was widened for the envelope code; it did
not make implementation/authority decisions.

After verification, task-owned Drydock, response subscriber and both relays
were stopped. The temporary `.canary-run` tree (including generated private
keys, database, executable and raw logs) was removed. This sanitized report is
the retained record; rerun the commands above to reproduce with fresh keys.
No files outside this worktree were edited. No tracker commands, push, or
worktree/branch removal were performed.
