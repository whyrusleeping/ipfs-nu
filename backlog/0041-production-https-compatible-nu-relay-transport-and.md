---
id: "0041"
title: Production HTTPS-compatible Nu relay transport and reach
status: in_progress
priority: 2
created: "2026-09-29"
updated: "2026-10-22"
depends_on:
    - "0013"
spec_refs:
    - docs/networking.md#D8. Wire profile and compatibility
    - Milestones
---

## Description
Specify and implement WSS/TCP-443 (or explicitly approved HTTPS-compatible mapping) plus necessary standard security/multiplexing and Nu relay adaptation for UDP-blocked Nu paths. Preserve negotiated libp2p compatibility and keep this networking component separate from optional IPFS import build tags. Exact wire mapping/security is design work first, not settled in product docs.

Acceptance: UDP-blocked Nu↔Nu verified resumable transfer through declared admitted endpoints, limits/auth/resource controls and endpoint/proxy support matrix; unsupported middleboxes fail clearly; no unconditional wherever-HTTPS-works claim; browser-native p2p and standard-peer DCUtR not prerequisites.

## Acceptance criteria

## Work log
- 2026-10-22: Foundations merged (90a92a7, 7c4bb45; ac8239c): D8 WSS mapping (TLS WebSocket → Noise XX → yamux; Nu↔Nu inner QUIC via circuit + qpath), own p2p/noise, p2p/wss, p2p/yamux (no new modules; D18 updated), Host.DialWSS/AcceptWSS, WSS relay listener with limits; experiments interop with go-libp2p websocket+noise+yamux both directions; relay/wss_test.go. Remaining: home-relay UDP→WSS fallback, node/swarm integration + config/status, natlab UDP-blocked transfer (direct + CONNECT proxy), middlebox failure evidence, review.
- 2026-10-22: Done (bd4022b; merged, coordinator unioned status/doctor relay fields with 0024). Home-relay endpoint sets (one QUIC + one WSS per relay, independent budgets, declared-only automatic UDP→WSS fallback, transport + UDP error reported), swarm.proxy (HTTP CONNECT), swarm.relay_cert_pins, doctor relay-fallback line. Evidence: natlab TestDaemonWSSUDPBlocked — both nodes drop all UDP, only relay TCP/443 (direct, and via CONNECT proxy): 11.4 MB transfer interrupted, ~4 MiB verified partial kept, completed with matching SHA-256; refusing proxy shows doctor-visible HTTP 403; endpoint/proxy support matrix in docs/validation.md. Coordinator re-ran WSS + TestDaemonNAT* on merged tree: pass. Limits: independent security review pending; one-sided daemon restart recovery broken (surviving peer keeps stale inner QUIC session) → task 0075; TLS-intercepting proxies with a locally trusted CA can't be detected without cert pins.
