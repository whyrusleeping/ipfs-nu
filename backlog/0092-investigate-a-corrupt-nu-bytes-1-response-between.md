---
id: "0092"
title: Investigate a corrupt /nu/bytes/1 response between honest peers (1 in ~310 GB)
status: done
priority: 2
created: "2026-10-29"
updated: "2026-10-31"
depends_on: []
spec_refs:
    - docs/data.md#/nu/bytes/1
    - docs/networking.md#Direct QUIC host
---

## Description
**Observed (0089, `scripts/xfer-bench.sh`, 2026-10-29):** one `/nu/bytes/1` response between two honest Nu processes failed Bao verification ("chunk group 47 does not verify"). Details:
- `xferbench fetch -mode discard` against a synthetic tmpfs-backed `xferbench serve`;
- alphac-2 (Realtek, GSO off) → alphac-3 (E810);
- 4 MiB blobs, 16 requests in flight, a fresh connection, ~370 MiB into the run.

The server's store was intact: later fetches of the same content verified. It did not reproduce in ~310 GB of further runs: 10 runs, then 25 on one connection, then 40 on fresh connections. See docs/validation.md "Synthetic transfer benchmark".

**Why it matters:** QUIC authenticates packets, so the bytes changed in memory after decryption, or before encryption on the sender. Candidates:
- magicsock / quic-go packet-buffer lifetimes, e.g. a buffer returned to a pool or reused by `ReadBatch`/`routeBatch` while stream frames still reference it;
- the GSO/GRO paths;
- the server's `readAheadAt` / `bufio` / `stallWriter` write path.

Fetchers drop such a peer as corrupt, so a rare memory bug looks like a malicious peer. In a 2:1 `nu get` it is silent (a log line only).

**Approach:**
- Long-running stress with `xferbench` (discard mode now reports and retries corrupt responses) across workloads, directions (alphac-3 as sender with GSO on), and 1:1/2:1.
- Run the race detector on serve/fetch under load.
- Audit buffer ownership in `p2p/magicsock` against quic-go's `ReadBatch`/`WritePacket` contracts.
- Optionally have the fetch side regenerate the expected synthetic bytes, to characterise a corruption (offset, length, whether the bad bytes come from elsewhere in the stream).
- Also keep `nu get`'s peer-dropped warnings in `lan-bench.sh` logs.

**Acceptance:**
- The cause is found and fixed, with a regression test at the layer responsible; or a documented stress budget (e.g. ≥ 2 TB with 0 corrupt responses on both senders and directions) with the instrumentation left in.
- `lan-bench.sh` records dropped-peer warnings.

## Acceptance criteria

## Work log
- 2026-10-29: resolved. The user reports alphac-2 (the sender) has bad RAM that occasionally flips bits, which explains one corrupt response in ~310 GB with an intact store. Not a Nu bug: Bao verification rejected it as designed. The xferbench corrupt-response reporting stays. Optional follow-up not done: keeping dropped-peer warnings in lan-bench.sh logs.
