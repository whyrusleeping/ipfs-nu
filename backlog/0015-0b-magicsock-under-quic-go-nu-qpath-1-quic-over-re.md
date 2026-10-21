---
id: "0015"
title: '0b: magicsock under quic-go + `/nu/qpath/1` (QUIC over relay stream)'
status: in_progress
priority: 2
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0005"
    - "0013"
    - "0038"
spec_refs:
    - docs/networking.md#D11. Nu-path
---

## Description
`p2p/magicsock`: the core of nu-path (D11 steps 1–2, 4).

- A `net.PacketConn` wrapper given to quic-go. It implements quic-go's OOB-capable interface so GSO, ECN and batched receive keep working, and benchmarks against a raw UDPConn to prove it.
- Virtual per-peer addresses for Nu peers; packets for unknown/real addresses (libp2p peers) pass straight through.
- Per-peer path table: the relay path plus zero or more direct UDP paths; send on the best one; switching is invisible to QUIC.
- `/nu/qpath/1`: negotiated as the security protocol on a circuit relay stream via multistream-select (standard peers answer `na` cleanly; their relayed secure fallback awaits standard-peer support, not an initial QUIC capability). Once accepted, the relayed stream carries length-framed QUIC packets for that peer's virtual address.
- The libp2p QUIC connection (libp2p TLS, ALPN `libp2p`) then runs over the virtual address, end to end.

**Acceptance**
- Two Nu nodes behind NATs in the NAT lab establish a libp2p QUIC connection over a relay using qpath; the PeerIds are authenticated end to end; the relay sees only ciphertext.
- A standard go-libp2p peer offered `/nu/qpath/1` declines cleanly.
- Throughput benchmark: magicsock direct path within ~5 % of a raw quic-go transport.

## Acceptance criteria

## Work log
- 2026-10-21: Functional core merged (dbd92b3; e7ad49d): p2p/magicsock shared-socket Conn (virtual peers, direct/relay paths, ReadBatch, disco envelope), /nu/qpath/1 v1 framing (uint16 length + ≤1200-byte QUIC datagram) in docs/networking.md D11. Evidence: loopback relay→direct→relay→reattach with one authenticated stream; rootless natlab between two EIM/APDF NATs incl. nft blocking and WAN down/up; go-libp2p declines /nu/qpath/1 cleanly; framing fuzz seeds. UNMET: throughput — virtual direct median 483 MB/s vs raw quic-go 862 MB/s (~44% lower; target ~5%). Profiling/optimization in progress.
