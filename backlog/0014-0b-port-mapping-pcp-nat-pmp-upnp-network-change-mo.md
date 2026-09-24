---
id: "0014"
title: '0b: Port mapping (PCP/NAT-PMP/UPnP) + network-change monitor and reaction pipeline'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0005"
spec_refs:
    - D12. Network changes
    - 'D11c. Hole punching: maximum effort'
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
