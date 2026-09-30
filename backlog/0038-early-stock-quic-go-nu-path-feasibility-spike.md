---
id: "0038"
title: Early stock-quic-go Nu-path feasibility spike
status: todo
priority: 1
created: "2026-09-29"
updated: "2026-09-29"
depends_on:
    - "0005"
    - "0003"
spec_refs:
    - docs/networking.md#D11. Nu-path
    - docs/validation.md#Early experiments
---

## Description
Build a bounded experimental relay-frame/direct PacketConn harness on stock quic-go; exercise direct ↔ reliable relay, network loss/relay reattachment, RTT/loss discontinuities, MTU changes and active large transfer. The harness need not wait for full identify/circuit/session implementation; use disposable peers and test framing.

Acceptance: reproducible transfer/path tests and congestion/resource results in docs/validation.md; go/no-go and adjustments to owning docs before 0015 public stack investment; no dependency fork assumed. Distinguish request-resume product success from preservation of the inner connection.

## Acceptance criteria

## Work log
