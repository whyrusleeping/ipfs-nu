---
id: "0076"
title: Fetch starts on first provider (don't wait for slow sources); replica retry backoff; private-gating dial transient
status: in_progress
priority: 2
created: "2026-10-22"
updated: "2026-10-22"
depends_on: []
spec_refs:
    - docs/data.md#D15. Content-provider discovery
    - docs/networking.md#D9. Peer sessions
---

## Description
Found during 0074:

1. `fetch.Fetcher.Fetch` waits for every provider source (`Providers.Find` over `fetch.Multi`) before walking, so a hinted `nu get` on a daemon whose DHT bootstrap is slow or failing is delayed up to the bootstrap timeout even though the hinted peers are live. Fetching should start as soon as any source yields a usable peer and keep adding providers as they arrive (the Providers interface already streams via a callback).
2. `replica` manager: a fast-failing placement fetch calls `Kick()` immediately → ~35 reconciles/s with a state-file rewrite each pass while a peer is unreachable. Add backoff on failure (bounded exponential, reset on progress or a relevant state change).
3. `p2p/host` dialer-side transient under private gating: a non-member's `Connect` can return nil for a few ms before the responder's post-handshake refusal tears the connection down (TLS 1.3 client-cert check after the client's Finished). Consider waiting for handshake confirmation (quic-go `HandshakeComplete`/first stream acceptance) before reporting success to callers.

Acceptance: a hinted get with an unreachable DHT bootstrap starts transferring within ~1 s (test with a stalled provider source); replica reconcile rate while a peer is down stays bounded (test counter); a non-member dial to a private node reports failure, not transient success (test). Fast tests only.

## Acceptance criteria

## Work log
- 2026-10-22: Done (0f0e8c5..4693898; merged). Fetch runs provider discovery in the background and starts on the first usable peer (TestFetchStartsBeforeSlowSourcesFinish: hinted fetch beside a stalled source completes in ~0.2 s); blob stage attempts every blob instead of cancel-on-first-error; replica bounded exponential backoff with reset on state change (TestFailedFetchBackoff); Host.Dial waits for the responder's post-handshake verdict so a refused dial never reports transient success (TestRefusedDialNeverLooksConnected; TestPrivateCluster asserts Connect fails).
