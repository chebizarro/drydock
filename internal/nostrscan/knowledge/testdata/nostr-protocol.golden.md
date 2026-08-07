# Nostr NIP and threat-model knowledge pack

Version: 1.0.0
Primary paper: IACR ePrint 2025/1459, Not in The Prophecies: Practical Attacks on Nostr

## Adversary models

### MU — Malicious user
A legitimate Nostr user who possesses its own user keypair and deviates from the protocol to break the security goals. MU can publish and query as an ordinary user and can often obtain public events, including NIP-04 ciphertext, from open relays, but does not gain a relay's authority merely by being a user.
Source: [NP25] §2.2, Definition 1

### MS — Malicious server or relay
A server behaving like a legitimate relay that can intercept, read, modify, and deliver events while deviating from protocol. Relays are not trusted infrastructure: anyone can operate one, a previously honest/default relay can turn malicious, and clients must validate relay-delivered data end to end.
Source: [NP25] §2.2, Definition 2

## Security goals and specification gaps

### confidentiality — Confidentiality
The content of an event issued by a legitimate user must remain secret from every unauthorized entity; for encrypted direct messages this is the expected E2EE property.
Source: [NP25] §2.3, Definition 3

### integrity — Integrity
A received event whose digital signature verifies must have content that was not altered. This requires recomputing the event id from the received body and verifying the signature over that id rather than trusting relay-supplied fields or cached results.
Source: [NP25] §2.3, Definition 4; §7.2; §7.3.1

### authenticity — Entity authenticity
When a requested event's signature verifies, the content must actually have been sent by the particular Nostr user being requested. A valid signature binds an event to its embedded pubkey; it does not authenticate that pubkey as the intended person's key.
Source: [NP25] §2.3, Definition 5; §3

### nip-security-gap — NIP security-goal gap
The NIPs examined by the paper do not clearly define security goals, key-authentication requirements, consistent client-side deduplication across relays, or when every event signature must be verified. Do not treat silence as a guarantee; require explicit confidentiality, integrity, authenticity, freshness, and key-binding invariants.
Source: [NP25] §2.3; §7; §7.2

## Vulnerability briefs

### NOSTR-V1 — Lack of public-key authenticity [MS]
Mechanism: MS replaces the embedded pubkey and content, recomputes id, and signs with its own key, so ordinary signature verification succeeds. Preconditions: the client obtains the target identity/event through MS without a pinned or independently authenticated key. Impact: forged profiles, contacts, posts, and payment metadata can be attributed to the victim; DM plaintext is not directly exposed because MS lacks the shared secret. Mitigation: authenticate fingerprints out of band or through auditable key transparency; never equate a valid self-contained signature with identity authenticity.
Source: [NP25] §3 (Vuln. 1); §7.1

### NOSTR-V2 — Missing signature verification [MU]
Mechanism: a client accepts non-profile events without verifying the signature over the recomputed id, allowing altered fields and an invalid sig. Preconditions: MU can obtain and resend an event to an affected ingest path; arbitrary encrypted-DM text additionally needs CBC manipulation. Impact: forgery of posts, contacts, and other events, plus a prerequisite for DM forgery and replay. Mitigation: recompute ids and verify signatures on every event before use, storage, decryption, or cache trust.
Source: [NP25] §4.1 (Vuln. 2); §7.2

### NOSTR-V3 — Unauthenticated NIP-04 encryption [MU]
Mechanism: NIP-04 AES-256-CBC has no MAC, so IV and ciphertext are malleable; a known plaintext/ciphertext block permits controlled changes. Preconditions: MU can obtain and inject ciphertext, the receiver fails event-signature verification, and controlled forgery needs a known block. Impact: forged DMs and, with recipient previews, URL or message recovery. Mitigation: verify every outer signature, migrate to versioned HKDF-separated and MAC'd NIP-44 v2, reject downgrade, and disable recipient previews.
Source: [NP25] §4.2 (Vuln. 3); §7.2–§7.3; Appendix F.2; NIP-44 v2

### NOSTR-V4 — Cross-protocol key reuse [MU]
Mechanism: reusing a keypair and ECDH secret across NIP-04 and NIP-46 lets a malicious connect flow expose fixed request plaintext with matching ciphertext, creating a CBC oracle. Preconditions: the victim accepts an attacker-crafted Nostr Connect session substituting the DM peer key, and key material is reused. Impact: persistent known blocks enable targeted DM forgery with V2/V3. Mitigation: derive distinct protocol keypairs with domain-separated KDF contexts, keep NIP-46 transport keys separate from the user identity, and leave NIP-04.
Source: [NP25] §4.2 (Vuln. 4); Appendix F.3; NIP-46 Keypairs

### NOSTR-V5 — Domain-name and metadata leakage [MU]
Mechanism: resolving and fetching a decrypted URL exposes its domain through DNS/TLS SNI and correlates it with timing, sender, and size metadata. Preconditions: the recipient generates a preview and MU can observe the network path or control the forged destination. Impact: recipient IP/activity disclosure and a known plaintext block supporting recovery of secret paths/tokens. Mitigation: never fetch recipient-side previews; create any preview sender-side before encryption and treat message-derived URLs as attacker-controlled.
Source: [NP25] §5 (Vuln. 5), §5.1–§5.2; §7.3

### NOSTR-V6 — Automatic recipient-side link preview [MU]
Mechanism: displaying/decrypting a DM triggers HTTP, so CBC manipulation can redirect the domain or prepend an attacker URL while retaining secret text in the path/query. Preconditions: NIP-04 malleability, acceptance of the forged event, and automatic recipient preview without consent. Impact: the described URL attack recovers the non-domain portion with certainty; generic message leakage succeeds with non-negligible probability. Mitigation: disable recipient previews and create them only on the sender before E2EE transmission.
Source: [NP25] §5 (Vuln. 6), §5.1–§5.3; §7.3

### NOSTR-V7 — Inadequate cache search [MS]
Mechanism: a verification cache is queried with sender-provided id before recomputing it; MS reuses a verified id with altered content and any signature to force a hit. Preconditions: a valid id is cached and MS controls delivery. Impact: signature bypass for forged profile/payment metadata, potentially redirecting Bitcoin. Mitigation: recompute id from the complete event, compare with embedded id, discard mismatch, and only then consult a cache keyed by recomputed id.
Source: [NP25] §6 (Vuln. 7); §7.3.1

### NOSTR-R1 — Generic replay [MU]
Mechanism: MU intercepts a DM event, changes created_at and id to look new, and resends unchanged ciphertext with a signature that should fail. Preconditions: a V2-affected receiver accepts the invalidly signed event. Impact: an old DM is presented as fresh under the sender's identity. Mitigation: recompute ids, verify every signature, and apply freshness/deduplication after cryptographic validation.
Source: [NP25] Appendix E.2; §7.2

### NOSTR-R2 — Truncated replay [MU]
Mechanism: CBC ciphertext is truncated before Ci, Ci-1 becomes the new IV, and the modified event is replayed so original Pi is the first plaintext block. Preconditions: NIP-04 content, ciphertext access, and a V2-affected receiver. Impact: a suffix of an old DM is accepted as a new partial-message forgery. Mitigation: verify event signature/id before decryption; authenticated NIP-44 additionally rejects truncation.
Source: [NP25] Appendix E.3; §7.2; NIP-44 v2

## NIP cheat sheet

### NIP-01 — Events, ids, signatures, and kinds
id is lowercase SHA-256 of canonical UTF-8 JSON [0,pubkey,created_at,kind,tags,content], and sig is a BIP-340 Schnorr signature over id. Recompute id and verify sig. Kind determines interpretation: regular events coexist; replaceable keep latest per pubkey+kind; ephemeral are not stored; addressable keep latest per pubkey+kind+d.
Source: NIP-01, Events and signatures; Kinds

### NIP-04 — Deprecated encrypted direct messages
Deprecated/unrecommended kind 4 DMs derive an ECDH secret and use AES-256-CBC with a transmitted IV but no ciphertext MAC. Do not add NIP-04 use; migrate to NIP-17/NIP-44 and never rely on CBC encryption for integrity/authenticity.
Source: NIP-04; [NP25] §4.2 (Vuln. 3)

### NIP-44 — Versioned authenticated encryption
Version 2 identifies the algorithm in the payload, applies ECDH then HKDF-extract with salt nip44-v2, expands per-message keys from a random nonce, encrypts with ChaCha20, and authenticates with HMAC-SHA256. Validate the outer signature before decrypting, reject unknown versions, verify MAC in constant time, and do not downgrade.
Source: NIP-44, Versions; Version 2

### NIP-46 — Nostr Connect remote signing
A disposable client keypair exchanges NIP-44-encrypted kind 24133 requests with a remote-signer keypair; the separate user keypair signs requested events. Never substitute the user's long-term key for client transport keys or reuse an undifferentiated NIP-04 ECDH secret across protocols.
Source: NIP-46, Keypairs; Overview; [NP25] §4.2 (Vuln. 4)

### NIP-59 — Gift wrap
An unsigned rumor is NIP-44-encrypted inside an author-signed kind 13 seal, then encrypted inside kind 1059 signed by a random one-time key; kind 21059 is ephemeral. Routing p tags and relay access still expose recipient metadata, so prefer recipient relays enforcing NIP-42 AUTH and validate each layer's semantics.
Source: NIP-59, Overview; Protocol Description; Other Considerations

### event-kinds — Event-kind semantics are security semantics
Validate behavior per kind. Regular: 1, 2, 4–44, 1000–9999. Replaceable: 0, 3, 10000–19999. Ephemeral: 20000–29999. Addressable: 30000–39999, keyed by pubkey+kind+d. Apply deduplication, replacement, retention, routing-tag, and inner/outer signature rules at the correct layer.
Source: NIP-01, Kinds; NIP-59, Protocol Description
