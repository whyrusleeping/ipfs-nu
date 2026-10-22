---
id: "0021"
title: '0b+: Port prediction + guard-railed birthday spraying for hard NATs'
status: in_progress
priority: 3
created: "2026-09-24"
updated: "2026-10-22"
depends_on:
    - "0017"
    - "0018"
spec_refs:
    - docs/networking.md#D11c. NAT traversal and relay optimization
---

## Description
The hard-NAT strategies (D11c step 4), selected from both peers' NAT profiles.

- **Port prediction** for port-preserving/sequential EDM NATs: the EDM side allocates toward the peer and reports the predicted port and jitter; the EIM side probes a window (±64…256, adaptive). Also used on our side of DCUtR later.
- **Birthday spraying** for EIM ↔ random EDM: the EDM side opens up to 256 sockets toward the EIM side's address; the EIM side probes random ports on the EDM IP (≈1024–2048 probes over a few seconds; 98–99.97 % expected). Release sockets immediately afterwards.
- EDM ↔ EDM: prediction where possible; both-random is **not** attempted by default.
- Optional low-TTL priming, kept or dropped based on lab results.
- **Guardrails:** Nu↔Nu only, negotiated in `/nu/path/1`; only after cheaper methods fail; per-peer and global rate limits; socket cap; back off on ICMP/IPS-like drops; `holepunch.aggressive = false` switch.

**Acceptance (NAT lab)**
- Measure the conditional ≥98% EIM ↔ random-EDM hypothesis with trial count/uncertainty and negative results recorded in docs/validation.md. This is an optimizer experiment, not a first-share, archival or v1 release requirement.
- Measured success for sequential EDM with jitter and concurrent background traffic.
- Guardrails verified: the IPS-drop rule triggers backoff; the socket cap is respected; nothing is sprayed toward non-Nu peers.

## Acceptance criteria

## Work log
- 2026-10-22: Two attempts with an opus agent were declined by the model (no output) — likely the 'spraying' framing tripping a safety filter despite the consent-based scope (authenticated peers of one session, probes only to the peer-reported NAT IP, MAC'd, bounded). Back to todo; next try with the disco author (sol) after 0029, framed as cooperative ICE-style rendezvous.
