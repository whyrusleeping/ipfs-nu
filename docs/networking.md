# Networking

[Spec overview](../spec.md) · [Data protocols](data.md) · [Names](names.md) · [Following](follow.md)

Nu implements its own fixed libp2p subset over stock quic-go. Iroh is a behavioral reference,
not a dependency. Nu extensions are negotiated; other peers use only shared standard
protocols. This document describes the intended behavior; the test plan below and project
milestones are acceptance gates, not claims of measured results.

## D8. Wire profile and compatibility

| Area | Initial QUIC profile |
|---|---|
| Transport | QUIC-v1 only |
| Security | libp2p TLS 1.3 inside QUIC |
| Multiplexing | QUIC streams |
| Negotiation | multistream-select 1.0, including optimistic/lazy select |
| Identity | Own key: ed25519. Verify: ed25519, secp256k1, ECDSA and RSA |
| Core | identify + push, ping, AutoNAT v2 client/server, circuit relay v2 reservation client/STOP/HOP server, signed peer records, mDNS |
| Routing | Amino Kademlia client; server when publicly reachable, with limits |
| Resources | Per-peer, per-protocol and per-connection limits |

Connection-level interop targets are go-, rust- and js-libp2p, including Kubo, Helia and
Ethereum consensus clients with QUIC endpoints. Ethereum interoperability is not consensus
participation. Joining Amino does not imply UnixFS/Bitswap data support.

**Preview limitations:** the initial QUIC profile cannot reach UDP-blocked paths or TCP-only
peers. Standard non-Nu relayed secure connections await Noise/yamux. `nu doctor` explains
limitations and next actions. Basic routed/private archival does not depend on relays/NAT.

**Production public reach:** HTTPS-compatible Nu relay fallback (planned WSS/TCP 443 with
appropriate security/multiplexing) precedes broad public recommendation, rather than waiting
for hard-NAT tuning, advanced clusters or browser-native p2p. Its exact wire/security mapping
and endpoint/proxy requirements must be specified and tested; “HTTPS works somewhere” is
not a guarantee the admitted relay is reachable. Public HTTPS gateway reads arrive earlier
and do not themselves provide peer connectivity.

**Extensions:** standard-peer DCUtR, browser-facing WebTransport/WebRTC-direct, optional
gossipsub, HTTP over libp2p and further interop. IPFS one-way import has its own adoption gate.
Multiselect 2 remains deferred. AutoNAT v1, circuit v1, rendezvous, floodsub and peer exchange
are outside the selected subset; mplex is not needed by the QUIC-only profile.

**Private and isolated clusters** ([D20](cluster.md#network-modes)) keep this wire profile and
gate connections to member PeerIds. They assume members are directly reachable, with no NAT
between them. They therefore run no relays, AutoNAT, port mapping, nu-path or hole punching,
and they never use third-party infrastructure; Amino, lookup servers and defaults are off.
libp2p PSK private networks are not available over QUIC.

## D9. Peer sessions

`connect(PeerId)` returns a session owning all connections to that peer and assigning new
streams to the best path. Race known addresses with short staggers: LAN → IPv6 → public
IPv4 → relay. The first successful path carries data.

Between Nu nodes, the intended normal case is one QUIC connection over interchangeable
relay/direct packet paths (D11). With standard peers, a session can hold separate direct
and relayed connections once that support lands. [Resumable requests](data.md#d14-transfer-protocols)
let transfers change connections without restarting the entire download.

## D10. Home relays

Every non-publicly-reachable node on the public Nu network maintains home/backup circuit
v2 reservations. Private/isolated clusters do not use these defaults.
Select by latency, renew before TTL expiry, replace failed relays, and publish changes
through identify push and signed peer records. Default relays and lookup servers are
preconfigured; development uses project-operated servers.

`nu relay` is a standard circuit v2 HOP server on QUIC/UDP 443. Admitted Nu peers receive
full-service circuits without the common 2-minute / 128 KiB limits; other peers receive
limited circuits. Relays also provide identify and AutoNAT v2 for address observation and
reachability testing. Operator ownership, funding, admission/abuse/quotas, deployment and
service limits are prerequisites for default public service, not deferred polish. Development
uses bounded admitted deployments. Keep content commands account-free; no identity-based
relay admission mechanism is selected yet. Model idle reservations/keepalives and relayed
bulk traffic, expose exhaustion clearly, and support explicit self/community relays. See
[validation](validation.md) for cost evidence and [operations](operations.md) for user contracts.

## D11. Nu-path

A magicsock `net.PacketConn` wrapper under quic-go routes one inner libp2p QUIC connection
over relayed or direct packet paths:

1. **Circuit setup:** A opens a circuit to B through B's home relay (CONNECT → STOP), then
   proposes `/nu/qpath/1` as the security protocol via multistream-select. A non-Nu peer can
   decline; the standard relayed fallback is part of D11b, not v1.
2. **Inner QUIC:** the accepted circuit carries framed QUIC packets. Magicsock gives B a
   stable virtual address; A dials it with libp2p TLS and ALPN `libp2p`. Authentication and
   encryption are end-to-end; the relay sees ciphertext.
3. **Discovery:** peers exchange candidates on `/nu/path/1`. Non-QUIC disco probes use the
   shared UDP socket (`Transport.WriteTo` / `ReadNonQUICPacket`); authenticate probes with a
   MAC key derived from the inner QUIC TLS exporter.
4. **Direct path:** after a candidate pair completes a probe round trip, route packets there
   while retaining the virtual address and connection IDs. Keep the relay path warm.
5. **Fallback:** heartbeat the direct path. On loss or network change, use the relay while
   probing again. The goal is to preserve the inner connection and active transfers.
6. **Retry:** restart probing on address changes, new observations/mappings and backoff timers.

The wrapper must preserve quic-go's OOB, GSO, ECN and batched-receive capabilities. Keeping
path changes below QUIC gives up per-path congestion control; a reliable relay stream also
introduces head-of-line blocking. Datagram relaying is described below. QUIC multipath/QNT
are not part of the stock-stack design; revisit multipath if quic-go supports it upstream.
Run an early bounded stock-quic-go spike for path changes, relay reattachment, MTU/congestion
and RTT/loss discontinuities before building the public session stack on this assumption.
The user-facing requirement is resumable progress; preserving the inner connection is an
additional measured goal, not a reason to block hinted/routed first-share or archival slices.

## D11c. NAT traversal and relay optimization

Try direct paths in the background while data uses the relay. Escalate after cheaper methods
fail; NAT classification and success rates must be measured, not assumed.

### 1. Network profiling

At startup, on network changes and periodically, measure:

- Mapping behavior using at least three observers on different IPs and ports: endpoint-
  independent (EIM), address-dependent, or address-and-port-dependent (EDM).
- Port allocation: preserving, sequential (delta and jitter from rapid probes), or random.
- Filtering through alternate-IP/port replies from relays, as in STUN CHANGE-REQUEST.
- Mapping lifetime through idle-then-probe tests. Keepalive default: 25 seconds, shortened
  for networks with shorter timeouts.
- Hairpinning, IPv6 and firewall behavior, UDP blocking and available port-mapping protocols.

Exchange the compact NAT profile during candidate exchange and display it in `nu doctor`.

### 2. Candidates

Gather LAN addresses on all interfaces, global IPv6, observed/reflexive addresses,
router-mapped addresses, predicted EDM ports and NAT64-synthesized addresses on IPv6-only
networks. Port mapping covers UPnP-IGD, NAT-PMP and PCP, including IPv6 firewall pinholes and
PCP PEER mappings where available. Detect router mapping-epoch changes and re-map.

### 3. Probe scheduling

Probe candidate pairs concurrently, ordered LAN → IPv6 → mapped → EIM reflexive → predicted.
Initial schedule: approximately 0, 20, 50, 100, 200, 500 ms, 1 s, 2 s; then back off.
While relayed, retry on address/profile/network changes and a 30 s → 2 min → 10 min timer.
Optional low-TTL priming opens the local mapping without reaching the far NAT; keep or drop
it based on NAT-lab results.

### 4. Escalation

| NAT pair | Strategy |
|---|---|
| Public, usable IPv6 or router-mapped endpoint | Direct dial |
| Same NAT / LAN | LAN candidates, mDNS and hairpinning |
| EIM ↔ EIM | Simultaneous probing |
| EIM ↔ predictable EDM | Report predicted port; probe a window of ±64–256 ports |
| EIM ↔ random EDM | Birthday spraying: EDM side opens 256 sockets toward EIM; EIM probes random ports on the EDM IP |
| EDM ↔ EDM, at least one predictable | Prediction on the predictable side, spraying on the other |
| EDM ↔ EDM, both random | Relay by default; do not attempt the much larger random search |
| UDP blocked | QUIC preview fails clearly; production public reach uses tested HTTPS-compatible relay fallback |

The birthday-search estimates (256/1024/2048 probes against 256 mappings: approximately
64%/98%/99.97%) are conditional estimates, not measured guarantees for arbitrary NATs.

Prediction and spraying require both Nu peers to agree via `/nu/path/1`. Do not use them
with other libp2p peers. Apply per-peer and global rates, cap spray sockets at 256 by default,
release temporary sockets after the attempt, back off on ICMP/IPS-like failures, and support
`holepunch.aggressive = false`. These methods consume NAT table space and can resemble port
scans. Destination authorization, replay resistance and probe validation require a precise
security contract and independent review before these features ship; rate caps/MAC checks
alone are insufficient. Hard-NAT direct-rate targets are scale-track experiments, not release
gates for first sharing or trusted archival.

### 5. Relay optimization

- Offer a negotiated Nu-only datagram relay extension carrying inner QUIC packets as outer
  QUIC DATAGRAMs. Retain standard circuit streams as fallback.
- Select relays by pair latency, RTT(A→relay) + RTT(relay→B), rather than home-relay latency
  alone.
- Public Nu nodes can offer peer relaying for their owner's devices or an allow-list.
- Continue direct probing while relayed and switch when a direct path becomes available.

### 6. Measurement

Extend the NAT lab with preserving/sequential/random allocation, jitter, mapping timeouts,
CGNAT quotas and IPS-like drops. Test consumer routers, OpenWrt, pfSense/OPNsense with and
without endpoint-independent NAT, and hotspots on two carriers. Field telemetry is opt-in:
NAT-profile pair, strategy, outcome and time to direct. Publish a per-pair success table for
each release; the hard-NAT targets are in the [milestones](../spec.md#milestones).

## D11b. Standard-peer hole punching (extensions)

Upgrade circuit connections with Noise + yamux, run DCUtR, and dial a direct QUIC connection.
Gather observed, port-mapped and IPv6 candidates early; use AutoNAT for reachability checks.
Use a shared UDP socket, swap roles and retry with jitter, re-run DCUtR while relayed, and
use the local NAT profile to select roles and explain failures. Port prediction can help
when our side is predictable; DCUtR cannot negotiate birthday spraying.

Target at least the published DCUtR baselines: 82.9% for port-restricted cone and approximately
70% overall, measured on comparable conditions.

## D12. Network changes

Watch interface/route changes, sleep/wake and VPN toggles. Rebind sockets, recheck addresses
with AutoNAT v2, refresh mappings, renew relay reservations, send identify push and restart
probing. Nu-path aims to preserve the inner QUIC connection through relay fallback; standard
sessions resume at the request level. Adapt Tailscale's BSD-3 portmapper/netmon code rather
than importing the full module.

## D13. Peer-address discovery

Run sources in parallel and start dialing as soon as addresses arrive:

1. Recent address cache.
2. Link hints (`peer`, `relay` circuit multiaddrs).
3. The publisher's `nu.node` records.
4. dnsaddr: upload a signed libp2p peer record to a lookup server, which verifies it and
   publishes TXT records at `_dnsaddr.<peerid-base36>.peers.nu.ipfs.io`. Dial with
   `/dnsaddr/<peerid-base36>.peers.nu.ipfs.io/p2p/<peerid>`.
5. The same server's `GET /routing/v1/peers/{peerid}` endpoint.
6. mDNS, enabled by default for offline LAN discovery.
7. Amino DHT `FIND_PEER`.

Signed records can be configured relay-only. The DNS representation uses lowercase base36
CIDv1 PeerIds to fit DNS labels; the PeerId in the dial address authenticates the peer.
A node record containing only relay/dnsaddr hints does not conceal direct IPs in public
DNS/peer records. Specify/explain default address exposure and relay-only behavior. Hint,
LAN and explicit-member discovery work independently of global Amino support.

## D16. Testing and diagnostics

- Fuzz every parser. Use libp2p `unified-testing` for QUIC-v1 and black-box Kubo, Helia and
  Ethereum clients for the protocols they support.
- NAT CI uses network namespaces for RFC 4787 mapping × filtering combinations, CGNAT,
  IPv6-only, hairpinning and network changes during transfers. Include Nu↔Nu, Nu↔go-libp2p
  and Nu↔rust-libp2p, with expected failures for unsupported QUIC-preview paths, including UDP blocking, and
  successful declared HTTPS-compatible Nu fallback paths at the production reach gate.
- `nu doctor` reports UDP reachability, NAT profile, relay latency/reservations, mappings,
  IPv6 and per-session path history.
- Metrics: direct/relayed bytes, time to first byte, time to direct, path switches and
  reservation uptime. Performance targets: first byte ≤ relay RTT + handshake, successful
  Nu↔Nu connections wherever UDP reaches an admitted relay, and direct rates comparable to
  iroh/Tailscale under the same NAT matrix. Validate setup-cost assumptions in measurements.
- Public defaults: operated/admitted relays/lookup, mDNS and port mapping enabled, DHT
  client/server selection by reachability, and plain-language errors. Private/isolated modes
  disable public defaults. Measure Linux/macOS sleep/wake, interface and VPN changes;
  Linux netns isolated-egress evidence does not establish macOS isolation guarantees.
- Record first-run sharing/browser success and recovery alongside connectivity metrics.
  Hardware/field evidence, methodology and failures go in [validation](validation.md).

## Details still to verify

Production HTTPS-compatible fallback wire/security rules, relay admission/economics,
endpoint support and the authenticated destination/probe contract remain to specify.
Stock-quic-go behavior must be proven for relay-path reattachment after network loss,
MTU/congestion behavior during hidden path changes, datagram size limits, and socket/OOB
handling. Direct-first session setup, pair-aware relay reservation, replay-resistant probing
and destination authorization need precise protocol rules. These are not settled by the
choice of magicsock; no particular fix is selected here.

## References

- libp2p: [QUIC](https://github.com/libp2p/specs/blob/master/quic/README.md),
  [TLS](https://github.com/libp2p/specs/blob/master/tls/tls.md),
  [circuit v2](https://github.com/libp2p/specs/blob/master/relay/circuit-v2.md),
  [hole punching](https://github.com/libp2p/specs/blob/master/connections/hole-punching.md),
  [AutoNAT v2](https://github.com/libp2p/specs/blob/master/autonat/autonat-v2.md)
- [dnsaddr](https://github.com/multiformats/multiaddr/blob/master/protocols/DNSADDR.md) and
  [delegated routing](https://specs.ipfs.tech/routing/http-routing-v1/)
- [Ethereum consensus networking](https://ethereum.github.io/consensus-specs/specs/phase0/p2p-interface/)
- [quic-go transport](https://quic-go.net/docs/quic/transport/) and
  [multipath issue](https://github.com/quic-go/quic-go/issues/3343)
- Tailscale: [NAT traversal](https://tailscale.com/blog/how-nat-traversal-works),
  [magicsock](https://pkg.go.dev/tailscale.com/wgengine/magicsock)
- Iroh: [NAT traversal](https://docs.iroh.computer/concepts/nat-traversal),
  [multipath/QNT](https://www.iroh.computer/blog/iroh-0-96-0-the-quic-multipaths-to-1-0)
- DCUtR: [paper](https://arxiv.org/html/2510.27500),
  [ProbeLab report](https://github.com/probe-lab/dcutr-project/blob/main/docs/dcutr-final-report.md)
- [AutoNAT audit](https://github.com/probe-lab/autonat-perf-audit/blob/main/docs/final-report.md)
