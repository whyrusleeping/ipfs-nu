---
id: "0081"
title: UDP-blocked reach via a Nu HTTPS relay carrying inner QUIC packets (iroh-style), no Noise/yamux
status: proposed
priority: 3
created: "2026-10-22"
updated: "2026-10-22"
depends_on:
    - "0080"
spec_refs:
    - docs/networking.md#D8. Wire profile and compatibility
---

## Description
Proposal (not accepted). After 0080 removes the libp2p WSS/Noise/yamux fallback, Nu nodes behind UDP-blocking networks cannot reach peers. An iroh/Tailscale-DERP-style alternative needs no new security protocol or muxer: an HTTPS (WebSocket or HTTP/2 stream) connection to a Nu relay carries the **inner QUIC packets** that magicsock already sends over `/nu/qpath`. The inner QUIC connection keeps libp2p TLS end-to-end authentication. The relay only authenticates who the client is for routing (a signed challenge using the node key over the TLS channel) and forwards packets to the destination peer's relay connection or its UDP path. It reuses magicsock path sets, so traffic upgrades to direct UDP when possible.

Questions for the user: is UDP-blocked reach needed for v1 at all, and if so is this Nu-only relay transport acceptable (it is not libp2p-interoperable)?

Acceptance if accepted: UDP-blocked Nu↔Nu transfer through a declared Nu relay in natlab, with bounded framing, per-peer limits and a clear doctor message when blocked.

## Acceptance criteria

## Work log
- 2026-10-22: DEFERRED by the user: UDP-blocked networks are not needed at this stage. Stays proposed; do not start without new user direction.
