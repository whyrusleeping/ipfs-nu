---
id: "0003"
title: 'NAT lab harness: netns-based NAT simulator with RFC 4787 matrix + network-change scripting'
status: done
priority: 2
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0001"
spec_refs:
    - docs/networking.md#D16. Testing and diagnostics
    - docs/networking.md#D11c. NAT traversal and relay optimization
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
- 2026-10-21: Done on track/net (d73e599, merged). p2p/natlab (natlab tag): rootless user+net namespaces with a root fallback, segments/NAT routers via nftables (EIM/ADM/APDM fixtures, EIF/ADF/APDF filtering, port allocation fixtures, hairpin, UDP block), network-change primitives (address/route/link changes, rebinding, conntrack flush), per-process Nu hosts. Evidence: mapping smoke observes EIM 5000/5000/5000, ADM 5000/5000/5001, distinct random APDM ports; filtering verified with unsolicited packets; hairpin/UDP-block/rebind/flush/link-cycle pass; suite ~6 s rootless locally. .github/workflows/natlab.yml written but not yet run on GitHub (no remote). Deferred: IPv6/NAT64 presets, nine-pair validation, two-NAT/CGNAT e2e, mapping lifetime measurements.
