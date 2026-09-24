---
id: "0008"
title: '0a: multistream-select 1.0 (optimistic) + stream/protocol routing'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0005"
spec_refs:
    - 'D8. The libp2p subset (v1: QUIC only)'
---

## Description
Protocol negotiation and stream routing on the host.

- multistream-select 1.0: listener and dialer, including **optimistic/lazy select** (send the header and protocol ID together with the first message; 0 extra RTT) and correct handling of `na`.
- Protocol handler registry and stream routing; per-protocol concurrency limits; stream reset semantics.
- Uvarint-length-prefixed message helpers (for protobuf and DRISL framing).

**Acceptance**
- Interop with go-libp2p and rust-libp2p negotiation, including optimistic select and rejection paths.
- Fuzz the multistream reader.

## Acceptance criteria

## Work log
