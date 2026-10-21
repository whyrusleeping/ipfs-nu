---
id: "0038"
title: Early stock-quic-go Nu-path feasibility spike
status: in_progress
priority: 2
created: "2026-09-29"
updated: "2026-10-21"
depends_on:
    - "0005"
    - "0003"
spec_refs:
    - docs/networking.md#D11. Nu-path
    - docs/validation.md#Early experiments
---

## Description
Harness code lives in the separate `experiments/` Go module when it needs third-party libp2p peers; anything reused by 0015 moves into the main module properly.

Build a bounded experimental relay-frame/direct PacketConn harness on stock quic-go; exercise direct ↔ reliable relay, network loss/relay reattachment, RTT/loss discontinuities, MTU changes and active large transfer. The harness need not wait for full identify/circuit/session implementation; use disposable peers and test framing.

Acceptance: reproducible transfer/path tests and congestion/resource results in docs/validation.md; go/no-go and adjustments to owning docs before 0015 public stack investment; no dependency fork assumed. Distinguish request-resume product success from preservation of the inner connection.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/net (fabac7e; merged). GO for 0015 on stock quic-go v0.64.1, with conservative adjustments now normative in docs/networking.md D11/D11c/D12: fixed ~1200-byte inner packets with PMTU discovery off, explicit ReadBatch (no SyscallConn passthrough), DATAGRAM payload budget over relays, bounded relay workers, conditional connection preservation. Evidence (experiments/nupath, docs/validation.md): verified 17+ MiB stream survived relay→direct→lossy→outage→new circuit without inner redial (3 runs, 4.1–4.65 s); 3 rootless NAT-lab runs between two NAT fixtures incl. WAN down/up. Not claimed: shared-socket CID demux, physical socket replacement, authenticated probing, final qpath framing, GSO/ECN.
