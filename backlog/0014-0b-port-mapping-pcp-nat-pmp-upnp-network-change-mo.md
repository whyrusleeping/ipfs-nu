---
id: "0014"
title: '0b: Port mapping (PCP/NAT-PMP/UPnP) + network-change monitor and reaction pipeline'
status: in_progress
priority: 2
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0005"
spec_refs:
    - docs/networking.md#D12. Network changes
    - docs/networking.md#D11c. NAT traversal and relay optimization
---

## Description
`p2p/portmap` + `p2p/netmon`, adapted from Tailscale's Go code (BSD-3, keep notices; do **not** import `tailscale.com`).

- Port mapping: PCP (including PEER mappings and IPv6 pinholes), NAT-PMP, UPnP-IGD. Watch the mapping epoch and re-map after a router reboot; renew before expiry; the mapped address becomes a candidate.
- Network monitor (Linux netlink): interface, address and route changes, plus sleep/wake detection.
- D12 reaction pipeline: rebind sockets → confirm addresses (AutoNAT) → refresh mappings → renew relay reservation → identify push → restart path probing. It exposes events that later tasks subscribe to.

**Acceptance**
- NAT lab: miniupnpd and a PCP server in the lab router produce working mappings.
- A scripted interface/address change triggers the full pipeline within 1 s.

## Acceptance criteria

## Work log
- 2026-10-21: Implemented on track/http (c944e21; merged 8152d29). p2p/portmap (PCP MAP/PEER, NAT-PMP, UPnP-IGD; renewal, epoch/remap, candidates; adapted from Tailscale v1.104.2 with headers + NOTICE) and p2p/netmon (Linux rtnetlink, darwin polling, sleep/wake inference, ordered D12 reaction pipeline). Evidence: protocol codec tests + fuzz seeds; rootless natlab with fake PCP/NAT-PMP/UPnP servers installing real nft DNAT rules (fresh public sources reach the app; remap on epoch reset); scripted interface change ran all six pipeline hooks in 10.5 ms. Remaining acceptance: real miniupnpd and PCP server daemons in the lab (only fixtures so far). Flake: netmon natlab test failed once when run concurrently with other natlab packages.
- 2026-10-21: Real-daemon acceptance met (79e3472): miniupnpd 2.2.3 (nft build) extracted rootless from the Ubuntu .deb runs in the lab router namespace; Nu's PCP, NAT-PMP and UPnP clients each create real mappings giving fresh-source inbound reachability, removed on release (TestNATLabMiniUPnPd, ~1.5 s; CI installs miniupnpd-nftables). Netmon flake root-caused (test read a drop-oldest diagnostics queue as lossless) and fixed (83d6f52); 20 repetitions + 3 uncached full tagged runs pass. Daemon hook wiring of the pipeline is left to the owners (0020).
