# Networking

[Spec overview](../spec.md) · [Data protocols](data.md) · [Names](names.md) · [Following](follow.md)

Nu implements its own fixed libp2p subset over stock quic-go. Iroh is a behavioral reference,
not a dependency. Nu extensions are negotiated; other peers use only shared standard
protocols. This document describes the intended behavior; the test plan below and project
milestones are acceptance gates, not claims of measured results.

## D8. Wire profile and compatibility

| Area | Initial QUIC profile |
|---|---|
| Transport | QUIC-v1 only; default listen UDP 4171 on all interfaces (configurable; relays use 443) |
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

### Wire primitive encoding and bounds

Node identities are Ed25519. The node-key file is the standard libp2p PrivateKey
protobuf (type 1, 64-byte seed/public concatenation), created exclusively with mode
0600, never overwritten by key generation. Loading rejects nonregular files and
permissions other than 0600. Public key protobufs are deterministic: Type then Data,
no extra fields, minimal varints. Keys are capped at 8192 encoded bytes; RSA modulus
sizes are 2048–8192 bits. secp256k1 verification uses the pure-Go Decred v4 module
because the standard library has no secp256k1 implementation; generation and signing
of non-Ed25519 identities are not exposed.

PeerIds embed public-key protobufs of at most 42 bytes using identity multihash;
larger keys use SHA-256. The binary ID limit is 44 bytes, the text limit 128 bytes.
Accept legacy base58btc and multibase CIDv1 with codec `libp2p-key` (0x72); generate
legacy base58btc and lowercase base32/base36 CIDv1 strings.

Multiaddrs retain exact bytes. Registered protocols outside the dialable subset are
preserved without transport semantics. At a truly unregistered protocol code, retain
the entire remaining suffix opaquely: multiaddr does not encode unknown protocol
lengths. These addresses have no standard textual rendering and must not be dialed.
Callers supply maximum text/binary address sizes.

Signed envelopes use RFC 0002: sign the concatenation of the length-prefixed domain,
payload-type bytes and payload (multiformats uvarint lengths). An empty payload-type
hint is permitted. Peer records use domain `libp2p-peer-record`, type bytes `03 01`,
and the deployed peer-record protobuf (PeerId field 1, uint64 sequence field 2,
repeated AddressInfo field 3, multiaddr bytes in AddressInfo field 1). The record
PeerId must match the envelope signer. Callers supply total-message, address-count
and per-address limits, and enforce increasing sequences when storing records.
Unknown protobuf fields of wire types 0/1/2/5 are skipped; groups, truncated or
nonminimal varints, bad signatures and identity mismatches are errors. Nested parsing
is fixed-depth (envelope → record → AddressInfo). Protobuf uint64 *values* support
all 64 bits; tags, lengths and multiformats integers use `core/varint`'s 63-bit profile.

### Direct QUIC host

Use one stock quic-go Transport per UDP socket, at most one socket per IP family;
pass pluggable PacketConns directly through to preserve their OOB capabilities.
Only QUIC-v1 is enabled; unidirectional streams, 0-RTT and TLS session resumption
are disabled. The ALPN is `libp2p`, TLS minimum version 1.3. Certificates carry
OID `1.3.6.1.4.1.53594.1.1` with DER SignedKey (two OCTET STRINGs: key protobuf,
signature). The host key signs `libp2p-tls-handshake:` concatenated with the exact
certificate SubjectPublicKeyInfo. Verify one current self-signed certificate,
reject unknown critical extensions, and on dial require the derived PeerId to
match the `/p2p` suffix. Certificates are limited to 16 KiB. Generated certificates
use an unrelated P-256 key and one-year validity (one-hour NotBefore allowance).

The initial dialer accepts literal `/ip4|ip6/.../udp/.../quic-v1/p2p/<id>` only;
DNS resolution, address racing and hints are session/discovery-layer work.
Default limits are 128 total pending/established connections, one connection per
peer, 1024 local streams total, 64 per peer and 64 per connection. All are explicit
host configuration fields. QUIC receive-window caps are 1 MiB per stream and
4 MiB per connection; handshake idle timeout is 5 s and connection idle timeout
60 s (20 s keepalive). Reuse a live connection when dialing a known peer. Resolve
simultaneous duplicate dials consistently: the lower binary PeerId keeps its
outgoing connection and the higher keeps that same incoming connection. Closing
or resetting a stream releases its local resource slot; half-closing writes alone
does not. Host shutdown closes connections, listeners, transports and owned sockets.

### Multistream and request framing

Every routed bidirectional stream negotiates `/multistream/1.0.0`, followed by a
protocol proposal. Tokens are uvarint-length-prefixed UTF-8 strings terminated
by one newline; the length includes that newline and is capped at 1024 bytes.
Embedded NUL/CR/newline is invalid. Accept a proposal by echoing it, otherwise send
`na` and permit another proposal (at most 16 per stream). Negotiation has a 5 s
stream deadline. Malformed, rejected/abandoned or over-limit streams are reset in
both directions with application error 0x100; connection errors use code 1.

The host routes incoming streams automatically; transport-level callers may set
`ManualStreams` and accept raw streams themselves. Register each protocol with a
positive, host-wide inbound-handler concurrency limit; exhaustion responds `na`.
Changing a handler while it is executing is rejected. The dispatcher closes the
stream when its handler returns. Applications own request I/O deadlines after
negotiation. `OpenStream(peer, protocol)` eagerly confirms negotiation;
`OpenStreamLazy` pipelines the header, proposal and first write, checks both
acknowledgements before the first read (or explicit `Confirm`), and reports `na`
as `ErrNotSupported`. A lazy rejection can follow sent request bytes, so applications
must not retry a non-idempotent request without their own acknowledgement contract.

Application message helpers use minimal multiformats uvarint length then exact
message bytes, with a caller-supplied maximum checked before allocation. They do
not read ahead, so protocol payload starts immediately after the negotiation.

### Identify, push, ping and address observations

The core protocol service registers `/ipfs/id/1.0.0`, `/ipfs/id/push/1.0.0`, and
`/ipfs/ping/1.0.0`. On each new connection it requests identify; local protocol
changes automatically push the current full snapshot to connected peers advertising
push support. Connection/protocol notifications coalesce and the background worker
processes peers serially. Explicit Identify/Push/Ping requests share a 32-request
limit; each inbound core handler also has a 32-stream limit. Deadlines default to
5 s. Instantiate one core service per host and close it before closing the host.

Identify protobuf fields are public key (1), repeated binary listen multiaddrs (2),
repeated protocol strings (3), observed multiaddr (4), protocol version (5), agent
version (6), and signed peer-record envelope (8). Responses/pushes send one
length-prefixed protobuf of at most 4 KiB; an oversized local snapshot is an error,
not silent truncation. Receivers merge up to ten length-prefixed fragments (8 KiB
per fragment, 64 KiB aggregate) through clean EOF, accepting protobuf unknown fields.
Limits after merging are 128 addresses, 1024 bytes per address/string, and 1024
protocols. Strings must be UTF-8 and protocol names must be valid multistream IDs.
Public keys and signed peer records, when present, must match the authenticated
connection's PeerId. Signed peer records are separately verified, limited to 8 KiB,
and only replace cached certified addresses for strictly increasing sequences;
unsigned listen addresses remain explicitly separate. Malformed input resets the
stream and is not cached. Defaults advertise protocol version `ipfs/0.1.0`, an
agent derived from the product-name constant, and actual bound socket addresses.

The peer-info cache holds at most 256 peers, evicting the oldest update. Address
observations are untrusted claims keyed by the observer's actual connection IP/port,
not its advertised address or PeerId alone. Retain only the newest observation per
endpoint, including local socket address and timestamp, capped at 128 endpoints
(oldest evicted). They are not automatically promoted to advertised addresses or
reachability evidence; AutoNAT/NAT profiling consumes them later. The v0 cache is
in-memory and has no retention/TTL or persistence contract yet.

Ping sends 32 random bytes and requires an exact echo; RTT excludes protocol
negotiation. Servers echo successive 32-byte frames, with a 5 s idle/request deadline
and a 30 s stream lifetime. EOF closes cleanly; partial frames, incorrect echoes
and timeouts reset the stream. Ping, identify and push failures do not close an
otherwise authenticated connection.

### v0 AutoNAT v2

`p2p/proto/autonat.New(host, Config)` registers `/libp2p/autonat/2/dial-back`;
`Config.Server` opts public nodes/relays into `/libp2p/autonat/2/dial-request`.
The owner passes the network's private-mode policy as `Config.Private`, which rejects
construction without registering either protocol. Do not construct AutoNAT in private
or isolated clusters. `AllowPrivateAddresses` is a lab-only override, never a production
default. Literal IPv4/IPv6 QUIC-v1 candidates only; DNS, TCP and circuit probes await
those transports. Addresses with an explicit destination must match the requester.

`Check(ctx, server, []Request)` uses an existing server connection (sessions can establish
it) and tests exactly the first candidate the server accepts. A cryptographically random
fixed64 nonce must arrive on a different connection with a consistent IP family/QUIC
transport; the dialer's PeerId is deliberately irrelevant. Success claims without that
nonce, unknown statuses and mismatched challenged/result indexes are discarded. Results
identify the selected address/index and the observer's actual connection endpoint; they
are one observer's evidence, not an aggregate NAT classification. Observer selection,
periodic/network-change scheduling, multi-observer confidence and promotion into identify
and signed peer records belong to the owning node, not this protocol service.

Servers use a fresh short-lived host/socket/identity for each probe, never the request
connection or the server's listening port. They select one destination and never fall
back after a failed dial. A destination IP different from the authenticated request's
observed IP requires 30,000–100,000 payload bytes before any dial. Only protobuf data
payload counts; each data field is bounded to 4096 bytes and tiny nonfinal frames are
refused. Clients explicitly opt into paying that cost per candidate, capped at 100,000
bytes. Servers wait for dial-back acknowledgement/closure before closing the probe host;
like go-libp2p, a reset that discards a queued acknowledgement does not undo a successfully
sent nonce. The client still requires independent nonce receipt to claim reachability.

Defaults: 8 inbound and 8 outbound requests, one active request per peer, sliding-minute
limits of 60 requests globally, 12 per peer, and 12 cross-IP data challenges. Rejected
identities do not grow retained rate-limit state. Request messages are bounded to 8 KiB,
50 addresses and 1024 bytes per address; dial-back messages to 1024 bytes. Requests have
a 15 s lifetime, dials at most 10 s, and dial-back exchanges at most 5 s; cancellation and
service Close interrupt I/O and join active operations. Close the service before its host.

Evidence: loopback success (including cross-IP dial data), a filtered UDP endpoint's
negative result, invalid-payment/no-victim-traffic, nonce/policy/rate-limit tests, decoder
fuzz seeds, and an `experiments/` smoke test against go-libp2p v0.50.0 in both directions.
These are not a measured consumer-router/NAT-lab classification result; that acceptance
and broader transport/network profiling remain deferred.

## D9. Peer sessions

`connect(PeerId)` returns a session owning all connections to that peer and assigning new
streams to the best path. Race known addresses with short staggers: LAN → IPv6 → public
IPv4 → relay. The first successful path carries data.

Between Nu nodes, the intended normal case is one QUIC connection over interchangeable
relay/direct packet paths (D11). With standard peers, a session can hold separate direct
and relayed connections once that support lands. [Resumable requests](data.md#d14-transfer-protocols)
let transfers change connections without restarting the entire download.

### v0 direct/hinted sessions

`host.NewSessions` wraps a Host with one canonical Session per PeerId. `Connect`
coalesces concurrent attempts and reuses live authenticated host connections;
`Session.OpenStream` opens an optimistic protocol stream, reconnecting before a
new request if the connection has gone away. Never replay sent application bytes.
The request protocol verifies chunks/objects and supplies a new resume request.
Stream creation contexts do not replace request I/O deadlines; always Close/Reset.
A canceled connect waiter does not cancel the shared attempt for other waiters;
the attempt is bounded by ConnectTimeout, and manager Close/Forget cancels it.

Candidates come from explicit/member/link addresses, the local cache, and streaming
lookup results, starting dials as soon as addresses arrive. Sort available candidates
LAN/loopback first, then global IPv6, then public IPv4, using confidence within each
rank. Default stagger is 50 ms; a failed last active dial advances immediately.
Defaults are 128 canonical sessions/explicit peers, 32 candidates per attempt,
16 concurrent dials across the manager, four per attempt, 2 s per dial and 5 s per
connect attempt. The first successful authenticated path wins; cancel/join losing
dials and close extra successful connections. Retained session handles become invalid
after Forget, which releases the session slot and can also revoke membership.

Only literal QUIC-v1 direct addresses are dialable in v0. DNS/circuit hints are
preserved but fail with ErrUnsupportedAddress until their resolver/transport lands.
No-address cases match ErrNoAddresses. DialError retains per-address reasons
and lookup errors and unwraps them for errors.Is/As. Certificate PeerId mismatch
has a typed Expected/Actual error, preserved through QUIC's handshake failure.
Successful dial addresses enter the optional cache at confidence 1 with seven-day
expiry; do not promote an address merely because Dial reused another connection.
Cache persistence failure warns but does not discard a working connection.

Private mode treats explicit Peers/SetPeer as administrative authorization and
never queries the supplied Resolver. Hints/cache cannot authorize a new member;
AddHints for a nonmember is rejected. The owner must not separately start public
lookup/mDNS services in explicit mode. Private sessions install an additional host
connection guard, intersecting any application/harness policy. Construct it before
registering application handlers; use Host.Config.AllowPeer to gate private identity
admission from the moment the listener opens. The guard does not undo data served
before its installation. Closing the private manager leaves a deny-all guard until
host shutdown, never silently making it public.
The session manager owns neither the Host nor the lookup service; close those too.

Host connection gating applies to inbound/outbound certificate identities, connection
admission and new stream admission. Primary and additional policies intersect and
run outside host locks. SetConnectionGater/RecheckConnections invalidate concurrent
admission decisions and close newly denied live connections. This supports harness
partitions without socket replacement; clearing the primary gate cannot override a
private-membership guard. Disconnect explicitly drops a peer's current connections.

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

`p2p/portmap` owns one UDP lease for an explicitly selected gateway and bound local
address; callers replace the client when either changes. It tries PCP v2 MAP, NAT-PMP,
then UPnP WANIPConnection (v1/v2) or WANPPPConnection v1. Supplying a remote endpoint
requests PCP PEER and constrains the candidate to that peer. IPv6 uses PCP MAP/PEER
firewall pinholes, not NAT-PMP or UPnP IPv6FirewallControl. A mapping is a **tentative**
candidate, not a reachability proof. Consumers must honor its peer restriction and expiry.

Default requested lifetime is two hours (configurable 1 s–24 h), renewal is at half the
**granted** lifetime, and epoch probes run every 30 s or sooner when renewal is due. An
epoch rollback, including zero, or lag beyond elapsed-time/drift tolerance invalidates
the candidate and re-maps it. UPnP uses WAN connection uptime as a conservative reboot/
reconnect signal. Transient renewal failures never extend the previous expiry. Cancel
and join the renewal loop, then explicitly release the lease on shutdown. Gateway/local
address selection and lifecycle wiring belong to the socket owner, not a global daemon.

PCP/NAT-PMP requests use a connected UDP socket to the selected gateway (port 5351),
with bounded retransmission and a default two-second per-protocol timeout. Replies are
capped at 1100 bytes and checked for opcode, result, local port and, for PCP, nonce,
protocol and PEER endpoint. Unknown mandatory PCP options fail. UPnP performs unicast
SSDP discovery at the gateway (port 1900); discovery and HTTP headers are capped at
8 KiB, HTTP/XML bodies at 64 KiB and XML nesting at 32. Description, URLBase and control
URLs must be HTTP IP literals at that gateway: no DNS, proxy, credentials or redirects.
UPnP requests a finite lease on a nonzero external port; it never requests an all-ports
wildcard or silently falls back to a permanent mapping. Protocol failures are returned
by `Refresh`/`Check`; `Run` retries until canceled. Per-file Tailscale v1.104.2 adaptation
origins and the full BSD-3 license are in `NOTICE`; no Tailscale runtime is imported.

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

`p2p/netmon.Start` subscribes to Linux rtnetlink link/address/IPv4+IPv6 route groups
before returning. Events are bounded, coalesced invalidations: consumers re-read OS
state rather than treating notifications as complete snapshots. Overflow requests a
full rescan. Darwin has a pure-Go polling fallback comparing interfaces/addresses and
the BSD routing RIB (250 ms default); it is compile-checked, not yet runtime-validated.
A wall-clock gap greater than two seconds (or backward jump) emits a combined inferred
sleep/wake/rescan event on return. This is not a pre-suspend notification; integrations
with explicit suspend/resume signals may call `Notify`. Short sleeps without OS network
changes or an explicit notification are not guaranteed to be detected.

`Pipeline.Register(Stage, Hook)` installs ordered, context-aware recovery hooks at
`Rebind`, `ConfirmAddresses`, `RefreshMappings`, `RenewRelay`, `IdentifyPush`, and
`RestartProbing`. Multiple hooks at a stage run in registration order. Each hook has a
caller-selected timeout and must honor cancellation; Go cannot interrupt arbitrary
blocking callbacks. Failure is reported but does not suppress later recovery stages.
Monitor subscriptions start with a rescan, merge bursts into a one-element queue and
close on unsubscribe/monitor close. Pipeline subscribers receive stage results and a
`Complete` result with joined errors; their bounded queues drop oldest results under
backpressure. The pipeline takes a snapshot of registrations for each reaction.

Socket/host owners wire rebind and AutoNAT first, then mapping-client replacement or
`Refresh`, relay reservation renewal, identify push and path probing. These hooks are
provided without editing those owners; unregistered stages are no-ops, not evidence
that a running daemon has performed the operation. The NAT-lab test registers all six
hooks and verifies a real interface/address change completes the pipeline within 1 s.

## D13. Peer-address discovery

Run sources in parallel and start dialing as soon as addresses arrive:

1. Recent address cache.
2. Link hints (`peer`, and `addr` direct or relay-circuit multiaddrs).
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

### v0 lookup, cache and mDNS

`p2p/lookup.Resolver` streams results while all sources run in parallel. The default
service combines the cache and libp2p mDNS; additional sources implement the bounded,
cancelable Source interface. Defaults: 32 simultaneous lookups, 3 s per lookup, ten
sources including cache/mDNS, 256 distinct results per lookup and 32 buffered results.
Cancellation stops source work; a source must honor context and the emit callback.
Hints are address claims, never membership or identity authorization. Normalize every
address to a terminal `/p2p/<destination>`; reject conflicting destination identities.
Share links accept repeated `peer` and `addr` query fields (8 KiB link, 16 peers,
32 addresses, 1024 bytes per address). A suffix-less address needs exactly one peer;
with multiple peers the destination suffix disambiguates, and must name a listed peer.
The member-record adapter accepts an authenticated member rkey and its addrs field;
cluster record verification remains the caller's job. Content/name/`via` resolution
is separate. The lookup layer does not consume atproto feeds.

The address cache retains Seen, Expires, source and original confidence. Effective
confidence is `original * 2^(-age / halfLife)`, with age clamped to zero and a default
24 h half-life. Hints start at 0.6 with 24 h expiry; mDNS at 0.5 with up to 120 s expiry.
Expired entries are not returned. Defaults cap 256 peers and 32 source/address entries
per peer, evicting oldest entries. Independent sources do not refresh each other's
confidence. Persistence has a Load/Save snapshot interface for a later SQLite adapter.
The interim FileStore uses JSON `{v:1, entries:[...]}`, at most 1 MiB/8192 entries,
regular 0600 files, same-directory temporary files, fsync, atomic rename and directory
fsync. Callers own the private parent directory. Invalid versions/entries fail closed.

mDNS advertises PTR/SRV/TXT plus A/AAAA under `_p2p._udp.local.` on UDP 5353
(224.0.0.251 and ff02::fb), using `dnsaddr=<multiaddr>/p2p/<id>` TXT strings.
TXT, SRV and address records use cache-flush; PTR does not. SRV port 4001 is a
compatibility placeholder: only TXT multiaddrs are used for peer connectivity.
Query on startup and lookup, then every 30 s; answer matching service/instance/host
queries, including unicast-response requests. Advertise at most eight addresses,
TTL 120 s, expand wildcard listeners to the selected interface's IPs, and omit
unscoped link-local IPv6. Packet limit is 9000 bytes, 128 questions/records total
checked before DNS decoding, and 32 received address claims per packet. Ignore
malformed/other-service/self advertisements and cap received TTL at 120 s.
Cluster service names are `_p2p-<first-10-hex-SHA256(clusterID)>._udp.local.`;
this is collision avoidance, not authentication or privacy. Discovery is enabled
unless DisableMDNS is set; explicit interfaces allow loopback-only test harnesses.
No multicast-capable interface is a reported startup error, not silent enablement.

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

The opt-in Linux harness is [`p2p/natlab`](../p2p/natlab/README.md), run with
`go test -tags natlab ./p2p/natlab`. It isolates even root callers, uses rootless
user+network namespaces when available, and runs only bounded v0 smoke scenarios.
Explicit endpoint fixtures model RFC mapping/filtering classes; ordinary Linux
masquerade is not treated as proof of any mapping class. The harness README lists
unvalidated/deferred topology and allocator cases; synthetic evidence is indexed
in [validation](validation.md#linux-nat-lab-smoke-2026-10-21).

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
