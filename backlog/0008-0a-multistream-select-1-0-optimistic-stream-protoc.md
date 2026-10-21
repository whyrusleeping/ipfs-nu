---
id: "0008"
title: '0a: multistream-select 1.0 (optimistic) + stream/protocol routing'
status: done
priority: 1
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0005"
spec_refs:
    - docs/networking.md#D8. Wire profile and compatibility
---

## Description
Protocol negotiation and stream routing on the host.

- multistream-select 1.0: listener and dialer, including **optimistic/lazy select** (send the header and protocol ID together with the first message; 0 extra RTT) and correct handling of `na`.
- Protocol handler registry and stream routing; per-protocol concurrency limits; stream reset semantics.
- Uvarint-length-prefixed message helpers (for protobuf and DRISL framing).

**Acceptance (v0 cut; deferred evidence is in 0068)**
- One smoke test in `experiments/`: negotiation with go-libp2p, including optimistic select and an `na` rejection. (rust-libp2p: 0011.)
- Fuzz target with seeds for the multistream reader.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/net (cfff5d1, merged 4ae1cad). Evidence: experiments/TestMultistreamSelect (optimistic select, na, incoming routing vs go-libp2p), TestProtocolConcurrency, TestMessageFraming, FuzzMultistreamReader seeds.
