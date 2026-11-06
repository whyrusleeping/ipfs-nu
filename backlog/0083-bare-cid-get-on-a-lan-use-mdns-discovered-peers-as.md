---
id: "0083"
title: 'Bare-CID get on a LAN: use mDNS-discovered peers as content providers'
status: done
priority: 3
created: "2026-10-22"
updated: "2026-11-06"
depends_on: []
spec_refs:
    - docs/networking.md#D13. Peer-address discovery
---

## Description

Observed 2026-10-22 during a v0 re-check: `nu get <bare-cid>` from a fresh daemonless repo on the same LAN as a holder failed after ~48 s ("no provider ... could be reached"). This happens even though mDNS saw the holder. By design (D13 / "v0 lookup, cache and mDNS"), mDNS only resolves addresses for a known PeerId; providers for a bare CID come from connected peers and the DHT, and nothing dials LAN peers that mDNS found. A link with `peer=` works (0.4 s). A bare-CID get through a daemon worked only because that daemon was already connected to the holder.

Proposal (early-IPFS "magic"): during a fetch, treat fresh mDNS advertisements as candidate providers. Dial them in a bounded way, ask `/nu/have/1`, and also give the LAN a short head start before falling back to the DHT. This also helps when the DHT is unreachable (offline LAN).

Acceptance: on a two-repo LAN, a fresh daemonless `nu get <bare-cid>` finishes in a few seconds with no link hints. Private/isolated modes keep their existing gating, and mDNS stays off where it is off today. Update the networking doc to say that mDNS also supplies provider candidates.

## Outcome

Fetches now offer peers with fresh mDNS entries as D15 step-4 candidates (swarm `lan` source: query, ≤3 s window, ≤16 parallel dials, only peers whose identify lists a /nu data protocol, session gating unchanged); the "no one to fetch from" early error no longer fires while mDNS runs. lookup gained MDNS.Peers and Service.MDNS. Evidence: TestBareCIDFromMDNSPeer (fails without the change), go test ./... green, and a manual CLI run on the dev host where a fresh daemonless `nu get <bare-cid>` from a LAN daemon took 0.28 s (the old binary failed after 39 s). Docs: networking.md v0 lookup/mDNS and data.md D15 step 4. mDNS enablement is unchanged; no explicit DHT delay was added because LAN answers arrive before DHT bootstrap.

Commit: node/swarm: mDNS-heard LAN peers are fetch provider candidates (0083)
