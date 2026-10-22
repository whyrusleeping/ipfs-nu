# Nu path discovery

`disco` is the bounded Nu-only control plane for `magicsock`, using stock
quic-go through `host`. No iroh, Tailscale or go-libp2p runtime is imported.
The exact wire/security contract is in **docs/networking.md, D11, Path v1**.
Independent security review is still required before public enablement.

## Socket / daemon owner integration (0020)

1. Create a magicsock socket and Host. Explicitly admit the remote PeerID with
   `AddPeer`, negotiate `/nu/qpath/1` on its circuit, and `AttachRelay`. Dial the
   virtual address with the expected PeerID; inner libp2p TLS must authenticate it.
2. `disco.New(ctx, Config{Host, Socket, Candidates, Authorize})` registers
   `/nu/path/1` and starts one queue consumer and scheduler per socket. The
   **circuit initiator only** calls `Service.Open(ctx, innerConn)`. This selects
   the protocol on that exact connection, not Host's preferred physical session.
   Duplicate peer streams and physical-address connections are rejected. One
   service owns the socket's disco queue; don't add a second UDP reader.
3. `Candidates(peer)` supplies at most 16 deduplicated, expiring local endpoints
   appropriate to disclose to that peer. `Gather(port, now)` enumerates usable
   addresses on all up interfaces (excluding loopback and scoped link-local).
   Filter to the socket's actual bind/family; add observed addresses **of this
   socket** and mapped addresses. A candidate is not a reachability proof.
4. `Authorize(peer, candidate)` is **mandatory** and independent of the peer's
   assertions. Use an explicit trusted-peer/network-scope policy. Never permit
   arbitrary public peers to scan local/private/infrastructure addresses, and
   never use an unconditional true callback for arbitrary peers. Candidate
   disclosure/privacy and authorization are owner decisions, not global defaults.
   Callbacks are concurrent, quick/nonblocking, and must not call Service methods.
5. Consume `portmap.Client.Changes()` in the socket owner; rebuild the provider
   using `FromMapping(mapping, intendedPeerEndpoint, now)` and call `Update()`.
   Honor PCP PEER restrictions, expiry, gateway replacement and mapping epochs.
   `Update` coalesces control writes and restarts probing. Local writes are paced
   at one per second; received control frames allow a four-frame burst.
6. Register `Service.RestartProbing` at `netmon.RestartProbing` after rebind,
   confirmation, mapping refresh, relay renewal and identify push. It revokes
   direct proofs, selects the surviving relay and restarts candidate exchange /
   early probing. Earlier stages must preserve or reattach a relay; this package
   does not replace sockets, manage reservations, reconnect sessions or resume
   requests. A missing relay is reported by the hook, not silently repaired.
7. `Status(peer)` reports selected endpoint, RTT, time to first direct path,
   observations, packet/rejection/fallback counts and bounded-state sizes.
   Observations are diagnostics, **not** candidate authorization. Close the
   service before Host/socket; it resets control streams and joins its workers
   without closing application connections, circuits or the shared socket.

Defaults: 25 s direct heartbeat, 3 s probe expiry; shorten heartbeat using measured
mapping lifetime. Loss is detected by missing heartbeat response (up to 28 s with
defaults), not instantaneous OS packet-loss notification. Switching preserves the
virtual peer address, CIDs and QUIC/application streams. Healthy-path hysteresis:
5 s dwell and both 20% and 5 ms RTT improvement. Validated alternatives heartbeat
while direct; early attempts/backoff never stop while relay-only. Fresh hello
nonces bind exporter keys to each control-stream instance, even if reopened on
the same inner connection. All responses are fixed-size and consume peer/global
outbound budgets; source authentication never implicitly permits a new endpoint.

## Evidence and deliberate limits

- `go test ./p2p/disco`: a real inner Host connection over a framed in-process
  relay upgrades, withdraws candidates, falls back, re-upgrades and retains the
  same application stream. Security negatives, rate bounds, hysteresis, mapping
  restrictions, directional/session key binding and decoder fuzz seeds run fast.
- `go test -tags natlab ./p2p/disco -run '^TestNATLabDisco$' -v`: rootless real
  namespaces/nftables, EIM/EIF ↔ EIM/EIF and EIM/EIF ↔ EIM/APDF, then link flap
  plus inside/outside address replacement during a verified 4 MiB echo transfer.
  A Unix-stream bridge supplies the **stable relay framing leg**, not circuit
  reservation, outer relay reconnection or a network-blocking relay. Reflexive
  fixture mappings are primed by an observer-bound packet before candidate
  exchange; this avoids nft conntrack caching a pre-mapping DNAT miss.
- This is not a field success matrix or the 1 GB soak; long/bulk matrices and
  no-reconnect exit gates remain 0024/0025. Public/IPv6/mapped router topologies,
  physical socket replacement, constrained-MTU paths and capacity changes need
  further evidence. Small disco probes do not certify the required 1200-byte
  inner packet budget; owners must restrict eligible paths accordingly.
- NAT profiles, prediction, port ranges, spray sockets, priming strategies and
  datagram relay optimization are not v1. Unknown control/probe encodings fail
  closed; 0021 must negotiate a separate extension and independent quotas /
  destination authorization, not silently widen v1 permission.
- Daemon/CLI wiring, circuit crossing/admission policy and public destination
  policy are intentionally **not** installed here. Review them independently
  before enabling this protocol for untrusted Internet peers.
