---
id: "0018"
title: '0b+: NAT profiling (mapping, port allocation, filtering, lifetime, hairpin, IPv6)'
status: done
priority: 2
created: "2026-09-24"
updated: "2026-10-22"
depends_on:
    - "0012"
    - "0013"
    - "0014"
spec_refs:
    - docs/networking.md#D11c. NAT traversal and relay optimization
---

## Description
Continuous "netcheck" (D11c step 1). Produces a compact **NAT profile** that is exchanged with peers and shown in `nu doctor`.

- Mapping behaviour from ≥ 3 observers on different IPs and ports (relays + public peers): EIM / address-dependent / address+port-dependent.
- Port allocation pattern for EDM: preserving / sequential (δ, jitter via rapid back-to-back probes) / random.
- Filtering behaviour, via relays replying from an alternate IP/port (RFC 5780-style).
- Mapping lifetime (idle-then-probe); hairpinning; IPv6 availability and firewall; UDP blocked; port mapping available.
- Re-run on start, on every network-change event, and periodically. Keep it cheap (few packets).

**Acceptance**
- The NAT lab classifies every configured NAT behaviour correctly (mapping, allocation, filtering, timeout within ±20 %) on both IPv4 and double NAT.
- Profile encoding is stable and versioned.

## Acceptance criteria

## Work log
- 2026-10-21: From 0013: relays answer AutoNAT probes from a fresh alternate port, but not an alternate source IP — define the profiling request semantics here rather than inventing a circuit/AutoNAT extension.
- 2026-10-22: Done (fcb117e, c0a5386; merged 26351b8, coordinator unioned WSS + netcheck in relay.Config and nu relay flags). p2p/netcheck: /nu/netcheck/1 observer descriptor + 64-byte UDP probes with cookies, alternate port/IP replies, anti-amplification limits; cross-observer filtering when no alternate IP; lifetime bracketing (≤9% error); v1 18-byte canonical profile (no addresses) with vectors + fuzz; /nu/natprofile/1 exchange; Monitor on start/network change/30 min; nu relay --netcheck flags; Swarm.NATProfile() seam for doctor. Evidence: natlab TestNetcheckNATLab 8 scenarios incl. double NAT and UDP blocked — mapping/filtering/allocation/hairpin exact, lifetimes within ±20%. Not done: IPv6 lab case, real routers, profile use in disco escalation/keepalives, public peers as observers, relay config key; observer needs security review before default public deployment.
- 2026-10-22: Doctor/status now show the measured netcheck profile via a default NATProfiler adapter (3bd2894), observation fallback before a measurement.
