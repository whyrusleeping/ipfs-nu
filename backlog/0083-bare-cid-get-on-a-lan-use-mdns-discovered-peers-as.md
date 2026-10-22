---
id: "0083"
title: 'Bare-CID get on a LAN: use mDNS-discovered peers as content providers'
status: todo
priority: 3
created: "2026-10-22"
updated: "2026-10-22"
depends_on: []
spec_refs:
    - docs/networking.md#D13. Peer-address discovery
---

## Description
Observed 2026-10-22 during a v0 re-check: `nu get <bare-cid>` from a fresh daemonless repo on the same LAN as a holder failed after ~48 s ("no provider ... could be reached"). This happens even though mDNS saw the holder. By design (D13 / "v0 lookup, cache and mDNS"), mDNS only resolves addresses for a known PeerId; providers for a bare CID come from connected peers and the DHT, and nothing dials LAN peers that mDNS found. A link with `peer=` works (0.4 s). A bare-CID get through a daemon worked only because that daemon was already connected to the holder.

Proposal (early-IPFS "magic"): during a fetch, treat fresh mDNS advertisements as candidate providers. Dial them in a bounded way, ask `/nu/have/1`, and also give the LAN a short head start before falling back to the DHT. This also helps when the DHT is unreachable (offline LAN).

Acceptance: on a two-repo LAN, a fresh daemonless `nu get <bare-cid>` finishes in a few seconds with no link hints. Private/isolated modes keep their existing gating, and mDNS stays off where it is off today. Update the networking doc to say that mDNS also supplies provider candidates.

## Acceptance criteria

## Work log
- 2026-10-22: Accepted by the user.
