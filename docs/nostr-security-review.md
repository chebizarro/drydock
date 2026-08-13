# Design: Nostr-Specific Security Review

**Status:** Implemented (design record)
**Date:** 2026-08-06
**Scope:** Extend the security review feature (`docs/security-review.md`) with a Nostr-aware lens: protocol-specific static rules, a threat-model knowledge pack for the reviewer, and an optional dynamic relay/client probe — applied only to Nostr projects.
**Primary source:** Kimura, Ito, Minematsu, Shiraki, Isobe — *"Not in The Prophecies: Practical Attacks on Nostr"* (IACR ePrint 2025/1459). Referred to below as **[NP25]**.
**Reference implementation:** `nostr-secprobe` — a Go CLI that actively probes relays/clients for these vulnerability classes.

---

## 1. Motivation

`docs/security-review.md` gives Drydock a general security capability: deterministic evidence (SAST, taint, security surface) narrowing the search, an LLM confirming and explaining, and an adversarial verify stage suppressing false positives. That machinery is protocol-agnostic — it knows about SQL injection and command execution, not about `pubkey` substitution or NIP-04 CBC malleability.

Drydock's own domain is Nostr. The repositories it reviews are overwhelmingly Nostr clients, relays, signers, and libraries — and [NP25] demonstrates that the highest-impact bugs in that ecosystem are *protocol-shaped*, not CWE-generic:

- A missing `sig` check is not a "missing validation" smell — it is a **universal event forgery** (§4.1, [NP25]).
- Using the sender-supplied `id` as a cache key is not a "trusting input" smell — it is a **signature-verification bypass leading to redirected Bitcoin transfers** (§6, [NP25]).
- Reusing the NIP-04 ECDH secret for NIP-46 is not "key reuse" in the abstract — it is the **known-plaintext oracle** that upgrades CBC malleability into universal DM forgery (§4.2, [NP25]).

A generic reviewer will not name these, will not rate them correctly, and will most likely miss them entirely, because the dangerous code looks unremarkable — an event handler that *doesn't* call `verify`, a cache lookup that *does* use `ev.ID`. **Absence-of-check** is the dominant failure mode here, and regex SAST is built to find presence-of-pattern.

The implementation adds three things: rules that reason about the Nostr protocol (including absence checks), a knowledge pack that gives the reviewer [NP25]'s adversary models, and an optional dynamic probe for live targets. All are gated on a **Nostr-project detector** so non-Nostr repos see no change.

**Design principles** (inherited from §1 of the security-review design, plus one):

1. *The model is the judgment layer, not the search layer.* Nostr rules find candidate sites; the reviewer decides whether the check is genuinely absent on that path.
2. *Fail closed, anchor to reality.* Every Nostr finding anchors to a real file/line the scan read.
3. *Suppress false positives explicitly.* Absence-checks are noisy by nature — the adversarial verify stage (`internal/securityverify`) is mandatory for this rule class, with a Nostr-specific refute lens ("show me where verification actually happens on this path").
4. *Everything stays local and Nostr-native.*
5. **NEW — *Dynamic probing is opt-in, authorized, and never part of a PR review.*** Active probes send malformed/replayed events to live relays. That is intrusive testing, permitted only against operator-declared targets with explicit authorization, and only in Pathway A audits.

**Non-goals:** probing third-party relays the operator does not own; exploit generation; reimplementing `nostr-secprobe` (we integrate its probe semantics, we do not fork its CLI).

---

## 2. Vulnerability taxonomy (from [NP25])

The paper's seven vulnerabilities and their attack variants become Drydock's Nostr rule taxonomy. Each maps to a CWE so it flows through the existing `securityscan` CWE table, `securityverify` classification, and SARIF output unchanged.

| ID | [NP25] ref | Vulnerability | Impact | CWE |
|---|---|---|---|---|
| `NOSTR-V1` | Vuln. 1, §3 | **Lack of public-key authenticity.** The `pubkey` in a received event is never authenticated; a malicious relay substitutes its own keypair and re-signs. Signature verification *passes*. | Key-substitution forgery of any event (profile, contacts, DMs) | CWE-345, CWE-322 |
| `NOSTR-V2` | Vuln. 2, §4.1 | **Missing signature verification** on received events. | Universal event forgery by any user | CWE-347 |
| `NOSTR-V3` | Vuln. 3, §4.2 | **Unauthenticated encryption** — NIP-04 AES-CBC with no MAC. | CBC malleability → DM forgery; combines with V4/V6 for plaintext recovery | CWE-353, CWE-327 |
| `NOSTR-V4` | Vuln. 4, §4.2 | **Lack of key separation** — the same ECDH shared secret serves NIP-04 and NIP-46 with no domain separation. | Known-plaintext oracle enabling universal DM forgery | CWE-323, CWE-1204 |
| `NOSTR-V5` | Vuln. 5, §5 | **Domain-name / metadata leakage** from message handling. | Recipient IP + link disclosure to attacker-controlled hosts | CWE-200 |
| `NOSTR-V6` | Vuln. 6, §5.1–5.3 | **Automatic link-preview generation on the recipient side** for encrypted DMs. | Plaintext recovery: URL non-domain part w.p. 1; whole message w. non-negligible probability | CWE-200, CWE-918 |
| `NOSTR-V7` | Vuln. 7, §6 | **Inadequate cache search** — sender-provided `id` used as cache key instead of recomputing it. | Signature-verification bypass; forged profiles → redirected Bitcoin transfers | CWE-345, CWE-354 |
| `NOSTR-R1` | App. E.2 | **Generic replay** — no dedup/`created_at` freshness on accepted events. | Replay of prior events as new | CWE-294 |
| `NOSTR-R2` | App. E.3 | **Truncated replay** — CBC ciphertext truncation replayed as a valid DM. | Partial-message forgery | CWE-294, CWE-353 |

Relay-side classes, drawn from `nostr-secprobe`'s active probe set, extend the taxonomy for relay implementations:

| ID | Check | CWE |
|---|---|---|
| `NOSTR-RELAY-SIG` | Relay accepts events with an invalid signature | CWE-347 |
| `NOSTR-RELAY-ID` | Relay accepts events whose `id` does not match the serialized body (stale-id mutation) | CWE-345 |
| `NOSTR-RELAY-DUP` | Relay accepts duplicate event ids (ID replay) | CWE-294 |
| `NOSTR-RELAY-MALFORMED` | Relay accepts malformed pubkey encoding/length, invalid kind, out-of-range past/future timestamps, empty/oversized/malformed tags | CWE-20 |
| `NOSTR-RELAY-RATE` | No rate/burst limiting under concurrent publish | CWE-770 |

**Mitigation guidance** ([NP25] §7) is attached to each finding's remediation note, so the reviewer's advice matches the paper: verify signatures on *all* events (§7.2); recompute `id` before any cache lookup and discard on mismatch (§7.3.1); generate link previews sender-side only, never on the recipient (§7.3); prefer NIP-44 (versioned, HKDF-separated, MAC'd) over NIP-04; provide key authenticity via OOB fingerprints or key transparency (§7.1).

---

## 3. Architecture

```
                      ┌──── Nostr project detector (NEW) ────┐
                      │ go.mod / package.json / Cargo.toml    │
                      │ nostr deps · NIP refs · kind consts   │  → confidence score; gates everything below
                      └───────────────────────────────────────┘
                                        │ (is-nostr)
        ┌───────────────────────────────┼───────────────────────────────┐
        ▼                               ▼                               ▼
 nostrscan (NEW)              nostr knowledge pack (NEW)      nostrprobe (NEW, opt-in)
 protocol rules +             NIP + [NP25] threat model        adapts nostr-secprobe
 absence-of-check analysis    → context layer + reviewer       relay/client probes
 (V1-V7, R1-R2)               preamble (MU / MS models)        against authorized targets
        │                               │                               │
        └───────────────┬───────────────┴───────────────┬───────────────┘
                        ▼                               ▼
              securityreview / auditengine  ──▶  securityverify (nostr refute lens)
                        │                               │
                        ▼                               ▼
                 reviewengine.Finding (category "security", CWE + NOSTR-* rule id)
                        │
                        ▼
              publisher: 1111 comments · 1630/1633 gate · 30619 report · SARIF
```

Nothing downstream of `reviewengine.Finding` changes. Nostr findings dedup, gate, publish, and land in SARIF exactly like every other security finding.

---

## 4. Nostr project detector (`internal/nostrscan`)

Every component below is inert unless the repository is a Nostr project. Detection is deterministic and cheap, computed once per checkout and cached alongside the codemap:

- **Dependency manifests** — `go.mod` (`github.com/nbd-wtf/go-nostr`, `fiatjaf.com/nostr`, `github.com/nostr-protocol/...`), `package.json` (`nostr-tools`, `@nostr-dev-kit/ndk`, `nostr-relaypool`), `Cargo.toml` (`nostr-sdk`, `nostr`), `pubspec.yaml`, Swift/Kotlin package files.
- **Protocol markers** — NIP references in docs/comments, `wss://` relay URLs, event-kind numeric constants in known ranges, the `["EVENT"|"REQ"|"CLOSE"|"OK"|"EOSE"|"AUTH"]` message verbs, `nsec1`/`npub1`/`nevent1` bech32 prefixes, `schnorr`/`secp256k1` usage adjacent to event structs.
- **Structural markers** — a struct/class/type with the NIP-01 event shape (`id`, `pubkey`, `created_at`, `kind`, `tags`, `content`, `sig`).

Output is a `NostrProfile{IsNostr bool, Confidence float64, Roles []Role, Evidence []Marker}` where `Roles ⊆ {client, relay, signer, library, dvm}` — roles select which rule subsets apply (relay checks are pointless in a client, link-preview checks pointless in a relay). Below a configurable confidence floor the whole Nostr lens is skipped and logged; it is never silently on.

---

## 5. Static rules (`internal/nostrscan`)

Two rule classes, because the paper's findings split cleanly along that line.

### 5.1 Presence rules (regex, reuse `securityscan`)

Straightforward pattern matches, expressed as `securityscan.Rule` values with `Classification: finding` so they ride the existing engine, dedup, and CWE table:

- NIP-04 encryption in use at all (`nip04.Encrypt`, `nip04Encrypt`, `getSharedSecret` + `aes-256-cbc`) → `NOSTR-V3`, with "migrate to NIP-44" remediation.
- AES-CBC construction over a Nostr shared secret with no MAC/HMAC/AEAD nearby → `NOSTR-V3`.
- ECDH shared-secret derivation with no HKDF/`info`/domain-separation string, or the same derived secret reaching both a NIP-04 and a NIP-46 code path → `NOSTR-V4`.
- Link-preview / OpenGraph / unfurl calls (`fetchPreview`, `LPMetadataProvider`, `og:image`, `linkPreview`, `unfurl`) reachable from DM/`kind:4`/`kind:14` rendering → `NOSTR-V6`.
- Outbound HTTP built from message-derived URLs → `NOSTR-V5`.

Also added: **Nostr security-surface locator rules** (`Classification: surface`, per §6.3 of the base design) tagging event ingest, signature verification sites, encrypt/decrypt, signer/bunker boundaries, relay subscription handling, event caches, and preview/render paths. These are context, never findings, and they seed the absence analysis below.

### 5.2 Absence-of-check rules (dataflow, the hard and valuable half)

This is where [NP25]'s highest-severity findings live, and where regex alone cannot go. Implemented over the existing `internal/codemap` call graph and the taint provider's reachability machinery:

- **`NOSTR-V2`** — from each event-ingest site (relay message parse, subscription callback, `["EVENT",...]` handler), walk forward to where the event is *used* (stored, rendered, trusted). If no signature-verification call dominates any path to a use site, emit a candidate. Verification sites are identified by the surface tags plus known API names (`ev.CheckSignature`, `verifySignature`, `verifyEvent`, `schnorr.Verify`).
- **`NOSTR-V7`** — a cache/dedup/store lookup keyed on an event `id` field that arrived from the wire, with no recomputation (`ev.GetID()`, `serialize`+`sha256`) and integrity comparison dominating the lookup.
- **`NOSTR-V1`** — a `pubkey` from a received event used as a trust anchor (contact matching, profile attribution, DM sender display) with no authenticity check against a pinned key, fingerprint, or key-transparency lookup on the path.
- **`NOSTR-R1`** — accepted events reaching persistence with no dedup on recomputed id and no `created_at` freshness/monotonicity check.
- **`NOSTR-R2`** — DM decryption paths accepting ciphertext of arbitrary/truncated block length with no length or integrity validation before decrypt.

Every absence rule emits with `Confidence` deliberately capped below the gating threshold, so an absence candidate **cannot gate a PR until the verify stage confirms it**. This is the mechanism that keeps this rule class from crying wolf.

### 5.3 Verify lens

`internal/securityverify` gains a Nostr-specific refute lens registered for `NOSTR-*` findings: *"Here is a claimed missing check with its ingest→use path. Point to the exact file:line where this check DOES occur on this path, or to the framework/library guarantee that performs it. Default to refuted if uncertain."* This directly attacks the absence rules' dominant false-positive mode — verification happening one frame up, inside a library, or in a wrapper the call graph missed.

---

## 6. Knowledge pack (`internal/nostrscan/knowledge`)

A compact, versioned corpus injected as a `nostr-protocol` context layer (priority 2, alongside `taint`) and as a reviewer system preamble when the Nostr lens is active:

- **Adversary models** from [NP25] §2.2 — *malicious user (MU)* and *malicious server/relay (MS)*, stated explicitly, because the correct severity of nearly every finding above depends on which model applies. A relay is not trusted infrastructure; the reviewer must be told so.
- **Security goals** ([NP25] §2.3) — what E2EE is supposed to guarantee on Nostr and where the NIPs are silent.
- **Per-vulnerability briefs** — one paragraph each for V1–V7/R1–R2: mechanism, preconditions, impact, and the §7 mitigation, so the reviewer explains a finding the way the paper does rather than generically.
- **NIP cheat sheet** — the relevant surface of NIP-01 (event/id/sig), NIP-04 (deprecated, unauthenticated), NIP-44 (versioned, HKDF-separated, ChaCha20 + HMAC), NIP-46 (remote signing, key-separation requirement), NIP-59 (gift wrap), plus event-kind semantics.

The pack is data (embedded files), not prompt strings scattered through code, so it is reviewable, testable with golden files, and updatable as NIPs evolve. It carries a `source` citation per entry — findings quote [NP25] §-refs in their remediation notes.

---

## 7. Dynamic probing (`internal/nostrprobe`, optional)

`nostr-secprobe` already implements the paper's checks as live probes: relay publish control, stale-id mutation, duplicate-id replay, invalid-signature acceptance, malformed pubkey/kind/timestamp/tag policy, rate/burst behaviour with latency percentiles, a tokenized preview-leakage harness for clients, and NIP-04-vs-NIP-46 HKDF domain-separation checks.

Drydock integrates it as an **audit-only evidence source** (Pathway A), never in a PR review:

- **Integration shape.** The default backend directly imports `git.sharegap.net/cascadia/nostr-secprobe` at `v0.2.0`, using its published `pkg/probes` library API. If the library path fails, Drydock falls back to an operator-installed `nostr-secprobe` binary, following the skipped-if-absent pattern already used for Trivy/gitleaks. As of `v0.2.0` the probe library runs on `fiatjaf.com/nostr`, the same stack Drydock itself uses, so the integration adds no second Nostr implementation to the dependency graph. Upstream results are still mapped to Drydock-owned evidence types at the `internal/nostrprobe` boundary — the containment is now cheap rather than necessary, and it keeps the backend swappable.
- **Authorization, non-negotiable.** Probing runs only when *all* hold: `security.nostr.probe.enabled: true`, the target appears in an operator-level (not repo-level, not PR-author-controlled) `authorized_targets` allow-list, and `i_understand: true` is set. Active/intrusive checks require a second explicit flag. This mirrors the existing "PR author cannot influence review policy" rule (DRYDOCK-nf8) — a fork's `.drydock.yaml` must never be able to point Drydock's prober at a third party.
- **Evidence, not verdict.** Probe results become `SecurityEvidence` entries and corroborate static findings: a static `NOSTR-V2` candidate plus a live `NOSTR-RELAY-SIG` acceptance is a confirmed, gating finding. `INCONCLUSIVE` probe results never gate.
- **Reporting.** Probe outcomes appear in the kind 30619 summary counts and in the gift-wrapped detail; raw target hostnames stay out of public events.

---

## 8. Configuration

Extends the `security:` block from §10.1 of the base design:

```yaml
security:
  enabled: true
  nostr:
    enabled: auto            # auto | true | false — auto = run iff detector says Nostr project
    min_detect_confidence: 0.6
    roles: auto              # auto | [client, relay, signer, library, dvm]
    rules: all               # all | [NOSTR-V1, NOSTR-V2, ...] | exclude: [...]
    knowledge_pack: true     # inject NIP + [NP25] threat-model context layer
    absence_analysis: true   # dataflow absence-of-check rules (needs codemap)
    verify_votes: 2          # absence rules default to a stronger refute panel than 1
    probe:                   # dynamic probing — Pathway A only
      enabled: false
      active: false          # intrusive checks (replay/malformed/rate)
      i_understand: false
      authorized_targets: [] # OPERATOR-LEVEL ONLY; ignored from repo config
      timeout: 30s
```

Env: `DRYDOCK_SECURITY_NOSTR_ENABLED`, `DRYDOCK_SECURITY_NOSTR_PROBE_TARGETS` (operator allow-list), `DRYDOCK_SECURITY_NOSTR_PROBE_ACTIVE`. Validation follows the existing repoconfig rules; **all probe fields fail closed** and `authorized_targets` supplied via repo config is rejected with a logged warning, never merged.

---

## 9. Phased implementation

**Phase N1 — Detection and knowledge**
- `nostrscan`: Nostr project detector (deps/protocol/structural markers, roles, confidence), cached per checkout.
- `nostrscan/knowledge`: embedded NIP + [NP25] threat-model pack; context layer + reviewer preamble.

**Phase N2 — Static rules**
- Presence rules (V3, V4, V5, V6) + Nostr surface locator rules on the `securityscan` engine, with the CWE mapping.
- Absence-of-check analysis over `codemap` (V1, V2, V7, R1, R2).
- Nostr refute lens in `securityverify`; confidence capping for absence candidates.

**Phase N3 — Wiring and policy**
- `security.nostr:` config section + env vars, operator-only probe allow-list.
- Wire the lens into `securityreview` (Pathway B) and `auditengine` (Pathway A), role-gated.

**Phase N4 — Dynamic probing and eval**
- `nostrprobe`: library integration with `nostr-secprobe` (+ binary fallback), authorization gate, evidence mapping, corroboration logic.
- Eval: extend the labeled dataset with Nostr vuln fixtures drawn from the paper's PoC scenarios; measure precision/recall/FP separately for absence rules, which are the risky class.

---

## 10. Testing and risks

**Testing.** Golden-file tests for the knowledge pack and detector output. Table-driven rule tests with fixture repos per role (client/relay/signer) in both vulnerable and fixed variants — the fixed variants are the FP tests, and they matter more than the positive cases. Absence-analysis tests assert the call-graph reasoning directly (check present in a wrapper → no finding; check absent on one of three paths → finding). Probe integration tests run against a local in-process relay, never a network target.

**Risks.**
1. *Absence-analysis false positives* are the central risk — verification hidden in a library, a framework, or a dynamic dispatch the call graph cannot see. Mitigations: confidence capping below the gate, a dedicated refute lens, `verify_votes: 2` default, and FP rate reported separately for this rule class in eval.
2. *Detector false positives* would apply Nostr rules to non-Nostr repos; the confidence floor plus explicit logging of why a repo was classified Nostr keeps this auditable.
3. *Probe misuse* — the operator-only allow-list and double opt-in are the control; this must be reviewed as carefully as the payment gating.
4. *NIP drift* — the knowledge pack is versioned data with citations so it can be refreshed without touching rule code.
