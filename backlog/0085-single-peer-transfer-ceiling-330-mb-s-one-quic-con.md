---
id: "0085"
title: Single-peer transfer ceiling ~330 MB/s (one QUIC connection) on 10 GbE
status: done
priority: 3
created: "2026-10-22"
updated: "2026-10-22"
depends_on: []
spec_refs: []
---

## Description
Measured 2026-10-22 on alphac (docs/validation.md "LAN benchmarks"): a 10 GiB 1:1 `nu get` fetches at 321–348 MB/s against a 1175 MB/s TCP baseline. 2:1 scales to 666–677 MB/s with an even split, so the fetcher is not the limit. The same ~330–380 MB/s ceiling appeared for plain quic-go on loopback in 0071. Raising UDP buffers from 4 to 16 MiB changed nothing.

Proposal: profile both ends of a single-peer transfer on alphac (CPU per side, quic-go GSO/GRO use, packet size, crypto, Bao verification, stream count per connection). Then decide between tuning (e.g. GSO, larger packets, more parallel requests) and opening a second connection per peer for bulk transfers.

Acceptance: a profile-backed explanation recorded in validation.md, and either a measured improvement in the 1:1 big-file result or a documented reason the ceiling is quic-go's.

## Acceptance criteria

## Work log
- 2026-10-22: Done. Profiled both ends on alphac (perf): neither side CPU-bound; 16 requests in flight or 8× receive windows changed nothing; plain quic-go v0.64.1 hits the same ~360 MB/s. Cause: sender UDP GSO on Realtek r8169 NICs without tx-udp-segmentation offload (software segmentation): plain quic-go 357 → 682 MB/s with GSO off; Intel E810 with offload is the reverse (1025 vs 715). Added swarm.gso = auto|on|off (p2p/host.ConfigureGSO: ethtool ioctl on the default-route NIC; explicit QUIC_GO_DISABLE_GSO wins; logged). alphac with auto: 1:1 big 335 → ~560 MB/s, 2:1 670 → ~900 MB/s. Recorded in docs/validation.md 'LAN benchmarks'; key documented in networking.md 'Direct QUIC host' and api.md. Test: host.TestGSOUplink (checked against ethtool on 4 hosts).
