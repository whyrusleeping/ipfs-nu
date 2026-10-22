---
id: "0024"
title: '0b exit: `nu doctor`, connectivity metrics, NAT-lab exit scenarios in CI'
status: done
priority: 2
created: "2026-09-24"
updated: "2026-10-22"
depends_on:
    - "0020"
    - "0014"
    - "0003"
spec_refs:
    - docs/networking.md#D16. Testing and diagnostics
---

## Description
Operability for connectivity, plus the Phase 0b exit gate.

- **`nu doctor`:** UDP reachability, NAT profile (basic until profiling lands), relay latencies and reservation state, port-mapping result, IPv6, per-session path history ("why is X relayed?").
- **Metrics** (Prometheus): direct vs relayed bytes, time to first byte, time to direct, path switches, reservation uptime, probe outcomes.
- **Phase 0b exit scenarios in the NAT lab:** 100 % connect across the matrix (UDP-blocked excepted, with a clear `nu doctor` explanation); Nu↔Nu direct wherever both sides are EIM or one is public/IPv6/mapped; a transfer survives a Wi-Fi → hotspot style switch **without reconnecting**.

**Acceptance**
- The scenarios above run in CI and gate merges to the networking packages.

## Acceptance criteria

## Work log
- 2026-10-22: Done (35a3a8a..92f241d; merged 2a26d11, coordinator unioned with 0072 DHT and 0029 workflow). Fixes: transient send errors (ENETUNREACH etc.) no longer kill QUIC connections across address changes; new remote candidate generation re-probes immediately; netmon identify stage re-identifies peers. nu doctor (udp, nat, relays, port mapping, ipv6, per-session why-relayed + history), GET /metrics (hand-rolled exposition, no new dep), NAT-lab gate in .github/workflows/natlab.yml on networking paths. Evidence: real-daemon natlab suite — relay matrix with doctor 'why' rows, home relay SIGKILL failover (same get), direct upgrade + metrics/doctor assertions, Wi-Fi→hotspot switch mid-get completes without restart, UDP-blocked doctor diagnosis; coordinator re-ran TestDaemonNAT* on the merged tree: pass. Remaining: wire swarm.Config.NAT to the p2p/netcheck profiler; CI not yet observed on GitHub.
