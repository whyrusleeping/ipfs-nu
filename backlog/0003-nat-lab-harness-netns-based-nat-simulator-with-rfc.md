---
id: "0003"
title: 'NAT lab harness: netns-based NAT simulator with RFC 4787 matrix + network-change scripting'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0001"
spec_refs:
    - D16. "Just works" is an engineering requirement
    - 'D11c. Hole punching: maximum effort'
---

## Description
A reusable test harness that runs Nu nodes (and third-party libp2p nodes) behind simulated NATs using Linux network namespaces + nftables. Feature tasks add scenarios on top of it.

- Topologies: two peers behind separate NATs, a relay/observer with ≥ 2 public IPs and ports, double NAT (CGNAT), shared NAT (hairpin on/off), IPv6-only (+ NAT64), UDP fully blocked.
- NAT behaviours, configurable per box: every RFC 4787 mapping × filtering combination; port allocation (preserving, sequential with jitter, random); mapping timeouts; per-subscriber port quotas; IPS-like "drop after N unsolicited packets" rule.
- Scripted events: interface down/up, address change, simulated sleep/wake, route change during a transfer.
- Produces machine-readable results (pair → outcome, time to first byte, time to direct), which later feed the per-pair success table.

**Acceptance**
- `make natlab` (or `go test -tags natlab`) runs on a stock Linux CI runner with root/netns.
- Includes a smoke scenario that proves the NAT behaviours are what they claim to be (e.g. an observer sees different ports for EDM).

## Acceptance criteria

## Work log
