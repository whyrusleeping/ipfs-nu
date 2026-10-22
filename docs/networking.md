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

`kad.New` implements `/ipfs/kad/1.0.0` directly over the routed host, with client
mode by default. The owner sets `PubliclyReachable` only from its AutoNAT v2 verdict
and must not construct it in private/isolated mode. A `SetPubliclyReachable` hook
changes server mode as reachability changes. `BootstrapDNS` resolves the deployed
bootstrap PeerIds through the existing dnsaddr resolver, resolves DNS4/DNS6
multiaddr components to bounded literal IP candidates, and keeps only UDP/QUIC-v1
addresses; `AddBootstrap` accepts another bounded lookup Source. No TCP bootstrap
addresses are dialed. Public mode rejects loopback, private and link-local referral
addresses by default; `AllowPrivateAddresses` is an explicit in-process/lab override.
Client mode registers no Kademlia handler and therefore does not advertise
`/ipfs/kad/1.0.0` in Identify. Server streams accept up to 64 sequential length-delimited
messages because Amino peers may reuse one stream for multiple RPCs; `ADD_PROVIDER` is
one-way and the server continues reading until EOF.

Wire/runtime limits: requests (sent and served) are at most 64 KiB with 20 peers/message
and 32 addresses/peer (1024 bytes/address), violations rejecting the message. Responses read
are at most 1 MiB (upstream accepts 4 MiB; popular `GET_PROVIDERS` answers exceed 64 KiB) and
are truncated, not rejected: the first 64 valid provider and 20 valid closer-peer entries
are kept and later ones skipped, an entry with an invalid peer ID is dropped, and addresses
over 1024 bytes or beyond 32 per peer are dropped; protobuf framing errors still fail the
response. Keys are at most 80 bytes (matching upstream's provider-key limit); routing k=20,
lookup alpha=3 and at most 16 rounds. Server defaults are 32 concurrent requests,
4 per authenticated peer and 4096 provider records. Provider records expire after
48 h; `ProvideAndRepublish` refreshes local announcements every 22 h. Client APIs
are `FindNode`, `FindProviders`, `Provide`, `ProvidePinned`, plus `Service.Lookup`
for the lookup.Source hook; `fetch.Kademlia` adapts GET_PROVIDERS. The owner must
pass only pinned blob/file-node/tree-root CIDs to `ProvidePinned` (never interior
shards) and supply usable advertised addresses. Node/CLI wiring and automatic pin
enumeration are not installed yet; hint/LAN/private lookup remains independent.

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

Literal QUIC-v1 direct addresses and single-hop circuit hints through explicit
configured/known literal QUIC relays are dialable in v0. DNS resolution remains a
lookup concern. `node/swarm` supplies the circuit transport seam, authenticates inner
TLS and never publishes/caches its local virtual dial addresses. A cold session
starts a relay immediately alongside direct candidates rather than waiting behind
unproven direct timeouts. No-address cases match ErrNoAddresses. DialError retains per-address reasons
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
reachability testing. The command uses the repo's `node.key` under the repo lock (a relay
and a daemon never share one PeerId at once); `--listen`/`--advertise`/`--admit` and the
limit flags override the `[relay]` config section, a wildcard listener requires explicit
advertised addresses, and it exits cleanly on SIGINT/SIGTERM. The dnsaddr/peer-routing
server (0016) attaches to the same host. Operator ownership, funding, admission/abuse/quotas, deployment and
service limits are prerequisites for default public service, not deferred polish. Development
uses bounded admitted deployments. Keep content commands account-free; no identity-based
relay admission mechanism is selected yet. Model idle reservations/keepalives and relayed
bulk traffic, expose exhaustion clearly, and support explicit self/community relays. See
[validation](validation.md) for cost evidence and [operations](operations.md) for user contracts.

The [circuit-v2 implementation](../p2p/proto/circuit/README.md) exposes raw circuit
streams for D11, not end-to-end authenticated host connections. HOP/STOP use
4096-byte bounded delimited protobufs and RFC 0002 reservation envelopes with domain
`libp2p-relay-rsvp`, codec bytes `03 02`, and canonical relay/peer/expiration payloads.
Reservations are bound to an existing direct connection; active dialing of supplied
peer addresses is disabled. Development full-service admission requires **both**
endpoints on the operator's bounded allow-list; otherwise 120s and 128 KiB per
direction apply by default. Global/per-peer/per-observed-IP slots still constrain
admitted traffic. Production operator/admission policy remains 0039; ordinary
Noise/yamux relayed connections and Nu qpath upgrades are separate work.

### v0 swarm connectivity configuration

`[swarm]` accepts `relays = ["/ip4/<ip>/udp/<port>/quic-v1/p2p/<relay>", ...]`
(at most sixteen, one endpoint per relay), `mode = "public" | "private" | "isolated"`,
and `peers = ["/ip4/<ip>/udp/<port>/quic-v1/p2p/<peer>", ...]` for explicit private
members. Public is the default mode but there is **no default public relay fleet**;
operators must configure available admitted relays. A link may introduce an exact
known relay endpoint; a remote signed record cannot independently introduce one.
Private/isolated mode disables relays, AutoNAT, disco, mDNS and public lookup sources.
Private mode additionally authorizes only explicit peers. Private/isolated
cluster membership applies its dynamic gate and disables public connectivity both
at startup and on runtime init/join; re-enabling requires a restart.

`probe_allow = ["<peerID>@<IP>:<port>", ...]` grants only those exact disco
**destinations**, independently of peer assertions. An explicit user link's direct
hint also grants its exact endpoint. These grants permit private candidate disclosure
to that peer; unrelated relayed peers receive no LAN/loopback candidates and cannot
induce UDP probes/replies to arbitrary targets. Signed records, relay metadata and
reflexive observations never grant probe permission. Without suitable grants peers
remain relayed: safe relay sharing does not depend on hole punching. No ranges,
prediction, DNS target resolution or blanket-true authorization are installed.

`portmap_gateway = "<literal IP>"` optionally enables PCP/NAT-PMP/UPnP leases at
that operator-selected gateway for the shared socket's port. Mappings remain tentative
candidates; no automatic gateway discovery is claimed. Two independent observer IPs
must AutoNAT-verify the same endpoint before public reachability withdraws reservations.
Those verified public endpoints enter the address snapshot before withdrawal. Public
signed identify records omit LAN/loopback literals; explicit user share links can
still carry LAN hints. Network invalidation revokes confirmed endpoints and restores
relay demand before rechecking reachability.
D12 runs real invalidation/confirmation/mapping-refresh/renewal/push/probing hooks.
Physical socket replacement is **not implemented**: route/address changes revoke
proofs and explicitly report that limitation rather than claiming successful rebind.

The lower authenticated PeerID always initiates inner QUIC and repairs relay legs.
Incoming crossed attempts cannot overwrite a live leg; setup is serialized per peer,
bounded globally, and retried with identity-based staggers. Recovery closes/joins the
old leg and tries authenticated known backup hints without replacing inner QUIC when
it beats the idle timeout. Initial inner paths use one primary socket (IPv4 when
available); ordinary direct/outer QUIC still supports both configured families, but
cross-family inner multipath is not claimed. `status`/`doctor` expose reachability,
relay TTL/RTT/errors, per-peer paths and failed network-change stages. `LookupSources`
is the streaming attachment seam for 0072; this work does not attach the DHT.

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
   shared UDP socket (`magicsock.WriteDisco` / `ReadDisco`, without a second socket reader);
   authenticate probes with a MAC key derived from the inner QUIC TLS exporter.
4. **Direct path:** after a candidate pair completes a probe round trip, route packets there
   while retaining the virtual address and connection IDs. Keep the relay path warm.
5. **Fallback:** heartbeat the direct path. On loss or network change, use the relay while
   probing again. The goal is to preserve the inner connection and active transfers.
6. **Retry:** restart probing on address changes, new observations/mappings and backoff timers.

### Qpath v1 wire framing and admission

`/nu/qpath/1` is a **security-protocol** multistream token on a raw circuit-v2
CONNECT/STOP stream, not an application stream on an already-secured relayed connection.
A proposer waits for selection confirmation before sending frames. Unsupported peers
answer `na`; do not send speculative QUIC payload or automatically select Noise/yamux.

After selection, each direction is an independent sequence of frames:

```
uint16 big-endian N       // exactly two bytes, excludes this header
byte[N] QUIC_datagram     // 1 <= N <= 1200; QUIC fixed bit (0x40) must be set
```

One frame carries exactly one complete inner UDP datagram, including any QUIC coalescing.
The maximum frame size is 1202 bytes. There is no type byte, preface, zero-length keepalive,
padding frame, fragmentation/reassembly or application-resume marker. A framing vector
`00 03 40 01 02` carries three bytes (not a valid encrypted QUIC packet by itself).
Length/fixed-bit violations, partial header/body or circuit I/O failure terminate **only
that relay leg**, discard its queued packets and interrupt both workers. EOF between
frames closes the leg. Never scan for a new frame boundary after a decode error. Validate
length before allocating; QUIC performs all remaining version, packet and TLS validation.
Frame lengths and circuit endpoints are visible to the relay, inner TLS/application bytes
are not. Relay-stream loss can hold up subsequent frames even while the inner sender
retransmits; bounded queues do not remove this head-of-line limitation.

Circuit metadata is a relay assertion, never end-to-end peer authentication. A local
virtual address is bound to the expected PeerId; Host checks the actual libp2p TLS identity
before application admission. Only explicitly admitted circuit setups may allocate peer
slots. One live relay leg per peer is allowed: a new circuit cannot replace it based on
asserted metadata. Its owner explicitly closes/joins the old handle before reattachment;
existing inner QUIC packets must still authenticate under the existing connection keys.
The orchestrator owns relay/peer admission, setup concurrency, reservation/reconnect policy
and stale/crossed-attempt handling. This core does not automatically accept public circuits
or infer permission to serve content from a successful qpath negotiation.

### Shared socket and path-set interface

`p2p/magicsock.Conn` owns one physical UDP socket with one batched receive loop. Unknown
CIDs and ordinary physical destinations pass through. Incoming inner packets are mapped
to their virtual peer only by a registered local destination CID **and** a caller-authorized
direct endpoint. Never classify every packet from a peer IP as inner QUIC: an outer relay
connection can use the same endpoint. Virtual-address Host sockets explicitly use
eight-byte local CIDs. The
synchronous public quic-go event observer tracks initial and encrypted `NEW_CONNECTION_ID`
issuance, retirement and close; it is a routing dependency, not optional asynchronous
logging. quic-go upgrades must verify this observer and shared-socket CID rotation.

Virtual addresses are local-only `240.0.0.0/4` (IPv4) or discard-only `100::/64` (IPv6),
port 1. Never advertise them, put them into shared caches/peer records, or send unknown or
removed reserved addresses to the OS. They are never reused within a socket's lifetime.
`AddPeer`/`RemovePeer`, `SetDirectPaths`, `UseDirect`/`UseRelay` and `Paths` provide the
control-plane seam: path candidates are not authorization. Defaults bound the socket to
128 peers, 256 queued relay receive datagrams, 32 direct endpoints and 64 routed CIDs per peer.
Each peer has at most one relay with a 64-datagram transmit queue and two I/O workers.
`Relay.Done` signals failure; `Relay.Close` cancels/joins the leg without closing the
logical PacketConn. The Host socket-policy hooks select the conservative inner config
while keeping real-address outer/standard connections on their normal config. Existing
Sessions can dial a local virtual hint after explicit qpath setup; public hint publication,
relay management and path racing are not implied by that integration.

The socket's disco demux envelope is `01 6e 75 64 01`, followed by a two-byte big-endian
payload length and exactly 1–1024 opaque payload bytes. The first byte has no QUIC fixed
bit. Reject malformed/version-mismatched envelopes; cap the independent receive queue at
32. Receipt never triggers a response, authorizes a destination or selects a path. The
MAC/nonce/replay/destination/rate contract and payload schema are specified below;
framing alone is **not authenticated disco**. `host.Conn.ExportKeyingMaterial` exposes the authenticated
inner TLS exporter for that owner. Unknown non-QUIC traffic remains available to quic-go's
non-QUIC demux. There must not be a competing raw UDP reader.

### Path v1 control and probe security contract (0017)

`/nu/path/1` is one long-lived bidirectional application stream on an already
TLS-authenticated **inner virtual-address** connection. The owner designates one
initiator; `node/swarm` uses the lower inner TLS PeerID (also its inner-QUIC/recovery
initiator), irrespective of which endpoint proposed the raw circuit. Reject duplicate
streams for a peer, ordinary physical connections and non-admitted virtual peers. Losing this stream revokes its probe keys, pending challenges and direct
paths; fall back to the relay. It is not a public UDP discovery service.

All integers below are unsigned big-endian. Each control frame is `uint16 N` followed
by exactly N bytes (maximum 363). Each direction first sends exactly one 18-byte hello:
`version=1:u8, type=0:u8, random_nonce:16 bytes`. Generate a fresh cryptographically
random nonce for every control stream, including reopening on the same inner connection.
Then send candidate frames: `version=1:u8, type=1:u8, generation:u64,
count:u8`, then count (0–16) entries, each `kind:u8, lifetime_seconds:u16,
endpoint:19 bytes`. Kinds are LAN=1, global IPv6=2, mapped=3, reflexive=4. Lifetime
is 1–600 seconds, measured from receipt. Generations start at 1 and strictly increase;
each frame **replaces** the previous list. Duplicate endpoints, unknown versions/types/
kinds, invalid addresses, trailing bytes and malformed lengths terminate the stream.
Endpoint encoding is `family:u8 (4 or 6), address:16 bytes, port:u16`; IPv4 uses
12 zero bytes followed by its four address bytes. IPv4-mapped IPv6, zones, unspecified,
multicast, broadcast, link-local and magicsock virtual addresses are forbidden.
Loopback is allowed only by explicit local destination policy (for tests/local peers).
There is no NAT-profile payload yet; profiles and new candidate kinds require explicit
version/extension negotiation, never reinterpretation of v1 entries.

The socket envelope remains `01 6e 75 64 01 | uint16(96) | payload`. The fixed
96-byte probe payload has offsets:

```
0       version=1:u8
1       type:u8                 // ping=1, pong=2
2..17   session_id:16
18..25  sequence:u64            // starts at 1, one counter per sending direction
26..41  challenge:16            // random ping nonce, echoed by pong
42..60  observed_endpoint:19    // all zero for ping; source of ping for pong
61..63  reserved:3              // must be zero
64..95  HMAC-SHA256(bytes[0:64]):32
```

Derive 80 bytes from inner TLS ExportKeyingMaterial using label
`EXPORTER-nu-path-1` and context `uint16(len(lowPeerID)) | lowPeerID bytes |
uint16(len(highPeerID)) | highPeerID bytes | lowPeerHelloNonce:16 |
highPeerHelloNonce:16`, with raw PeerID bytes sorted lexicographically. Both fresh
hello nonces bind the keys and replay state to this particular control stream. Bytes 0–15 are the session ID; 16–47 authenticate low→high,
48–79 high→low. Never reuse keys across connections, persist them, derive them from
outer relay TLS or use a single reflection-prone key in both directions. Verify the
session, exact length, reserved bytes, MAC (constant-time), source authorization and
64-packet sliding replay window before acting. Sequence 0, duplicates and packets older
than the window are rejected. Counter exhaustion terminates the session. A pong must
also echo an unexpired outstanding random challenge for **that exact destination**;
consume it once. Round-trip proof, not a ping or a peer's assertion, admits inner QUIC
from an endpoint. Observed addresses are diagnostics only, never implicit candidates.

**Destination authorization is separate from authentication.** The peer must assert the
exact IP:port in its current unexpired control list, and the socket owner's mandatory
`Authorize(peer, candidate)` policy must permit it. This policy is an independent
anti-scanning boundary: admit only destinations appropriate for the trusted relationship/
network scope; do not install a blanket-true policy for arbitrary Internet peers. An
authenticated malicious peer can lie about addresses. In particular do not let public
peers target private, loopback, link-local, infrastructure or other sensitive endpoints.
The library has no permissive default. Neither an incoming source, a pong observation,
a mapped-address hint nor a wildcard IP/port range grants permission. Reply only to an
exact authorized source, with the same fixed size as the request, no unsolicited
responses, errors, redirects or amplification. No DNS resolution or arbitrary socket
allocation occurs. Mapping candidates must be unexpired and honor PCP PEER restrictions.

Hard bounds: 128 sessions/socket, 16 candidates/session, 32 outstanding challenges,
3 s default challenge expiry, outbound probes **including pongs** 20 packets/s/peer
(burst 32) and 100 packets/s/socket (burst 128). Control receives are limited to
1 frame/s (burst 4); local updates coalesce and are paced to that rate. Authorization
and replay failures are silent and never elicit traffic. Candidate expiry/removal
revokes path admission and pending challenges, not merely its scheduling priority.
Control/key/session teardown clears replay and challenge state. These are safe baseline
bounds, not permission to scan at their maximum rates.

Order candidate attempts LAN → global IPv6 → mapped → reflexive. Attempt at roughly
0/20/50/100/200/500 ms/1/2 s, subject to hard rate caps. While relayed, restart after
30 s, then 2 min, then every 10 min, and immediately on local/remote candidate or network
changes. Direct paths heartbeat every 25 s by default (owner may shorten using measured
mapping lifetime); a missed challenge falls back at its 3 s timeout and restarts probing.
Prefer the first proven path; switch a healthy path only after a five-second dwell and
an RTT improvement of both 20% and 5 ms. Keep validated alternates fresh while direct.
A D12 RestartProbing hook first revokes stale path proofs and selects relay, then
re-exchanges candidates and starts the early schedule. This does not replace sockets,
renew circuits or reconnect inner QUIC; those remain owner responsibilities.

**0021 extension boundary:** v1 authorizes only exact endpoints, has no prediction,
port ranges, spray sockets, low-TTL priming or NAT profiles. Reject such encodings.
A future negotiated extension needs mutually acknowledged, expiring attempt IDs, exact
IP and bounded port-set authorization, independent local policy, per-attempt packet/
socket quotas within shared global limits, cancellation and replay binding. Neither
this MAC nor peer consent alone authorizes arbitrary scanning. Independent security
review of this contract and its owner policy is required before public enablement;
negative tests and loopback/NAT-lab evidence are not that review.

### Stock-stack constraints

Use stock quic-go with the following constraints for the first implementation of 0015:

- **Stable inner packet budget:** set `InitialPacketSize = 1200` and
  `DisablePathMTUDiscovery = true` on inner connections, on both peers. Every eligible
  direct path must carry a 1200-byte UDP payload; circuit framing must carry the same
  complete datagram without truncation. Hidden path changes do not reset quic-go's MTU
  estimate. Do not discover a larger inner MTU on one path and silently move that
  connection to a smaller one. A larger adaptive budget needs a separately validated
  design, not a quic-go fork or an assumption that loss will reduce the packet size.
- **One congestion history:** stable virtual addresses hide path changes from QUIC.
  RTT, congestion window and loss recovery carry over; stock quic-go exposes no reset
  for a hidden route switch. Expect transient loss, underutilization and retransmission
  bursts when RTT/capacity change. Do not advertise per-path congestion control or
  simultaneous multipath. Reliable relay streams additionally introduce head-of-line
  blocking and nested congestion control; they are a correctness fallback, not a bulk
  performance guarantee.
- **Bounded packet adapter:** relay reads/writes run independently of the inner QUIC
  event loop. Use a bounded per-peer relay transmit queue (initial cap: 64 datagrams),
  drop packets rather than block QUIC or accumulate an unbounded reliable-stream
  backlog, and cancel/join old writers on reattachment. A failed relay is a lost packet
  path, not closure of the shared PacketConn. Bound receive queues and framing lengths
  before allocating. A new circuit and new outer QUIC connections can replace the old
  relay path without replacing the inner connection, provided recovery beats its idle
  timeout. Otherwise reconnect and resume verified requests; do not replay stream bytes.
- **Correctness before socket offloads:** the initial inner adapter may use plain
  PacketConn I/O with GSO/ECN disabled. An optimized wrapper must implement its own
  `ReadBatch` as well as quic-go's `OOBCapablePacketConn`: forwarding only `SyscallConn`
  and `ReadMsgUDP` lets x/net bypass the wrapper's receive routing via the underlying
  fd. Split GSO super-packets into individual datagrams before relay framing; never
  forward UDP segmentation, physical source-address or ECN metadata blindly through
  a circuit. Offloads must preserve these semantics on both relay and direct paths
  before enabling them. The conservative inner mode does not require disabling
  optimizations on ordinary direct/outer relay QUIC connections.

The initial optimized adapter preserves physical/outer OOB, GSO and ECN. Inner direct
writes retain GSO segmentation but remove stale physical source/interface and ECN metadata;
inner receives (relay or direct) expose no physical OOB. Inner ECN validation therefore
falls back rather than pretending reliable-stream packets have meaningful IP marks.
This is not a per-path ECN implementation. Physical `ReadBatch` receives directly into
quic-go's buffers; do not swap their indexed ownership. Copy only queued relay frames
and holes left by filtered/disco packets. A relay arrival wakes an idle physical read
with a private deadline, without changing the logical caller deadline. There is no
second UDP reader or direct-path userspace receive queue. The synchronous CID observer
still incurs stock quic-go event-construction costs; near-raw throughput is not assumed.
See [implementation and ownership](../p2p/magicsock/README.md),
[core measurements](validation.md#nu-path-packet-core-0015-2026-10-21) and
[stock-stack feasibility](validation.md#stock-quic-go-nu-path-spike-0038-2026-10-21).

QUIC multipath/QNT are not part of this design. The user-facing requirement is resumable
progress; preservation of the inner connection is conditional on the surviving packet
budget and recovery deadline, not a reason to block hinted/routed first-share or archival
slices.

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

#### v1 netcheck observers and profile (0018)

`p2p/netcheck` implements this step. AutoNAT v2 and circuit v2 are **not** extended: AutoNAT
answers reachability from a fresh port only, and NAT profiling needs replies from a chosen
alternate IP/port and to a chosen port, so it has its own small observer protocol.

**Descriptor stream `/nu/netcheck/1`.** On an authenticated connection the client writes the
single byte `01` and half-closes; the observer replies `version=1:u8 | count:u8 (1–6) |
count × (role:u8, endpoint:19)` and closes (endpoint encoding as in `/nu/path/1`). Roles are
primary=1, alternate port=2 (same IP, different port) and alternate IP=3 (same family and port,
different IP); at most one set per family, primary first, roles in order, sets contiguous.
Other encodings are rejected. Handler concurrency is 16 streams; 5 s deadline. `nu relay`
serves it when configured with literal (never wildcard) observer sockets; a set may advertise
1:1-NAT public addresses that differ from its bound addresses. The descriptor only names
UDP sockets: it is not reachability evidence and grants nothing.

**UDP probes.** Every packet in either direction is exactly 64 bytes:

```
0-3   magic 02 6e 63 01        // no QUIC fixed bit, not the disco envelope
4     type: 1 request, 2 response, 3 hairpin (client-to-client)
5     request flags: 1 reply from alternate port, 2 from alternate IP (at most one)
6-7   request target port, 0 = reply to the source port
8-23  random transaction ID
24-39 cookie (requests: zero for plain requests); response: fresh cookie for the source IP
40-58 response: observed source endpoint of the request
59    response: role of the replying socket
60-63 reserved zero
```

Fields that are not defined for a type must be zero. Observers answer a plain request from
the receiving socket to its source. A request with flags or a target port needs an unexpired
cookie issued to the **same source IP**: `expiry:u32 (unix seconds) | HMAC-SHA256(key,
"nu-netcheck-1 cookie" | expiry | ip16)[:12]`, a per-process random key, 10-minute default
lifetime (1 min–1 h). Cookies prove return routability to an IP, never a port. Change flags
are honoured only on the primary socket and only for roles the observer has; the target port
always uses the requester's observed IP. Replies are never larger than requests (no
amplification) and leave from the observer's own sockets only. Budgets: 20 replies/s per
source IP (burst 40, at most 4096 tracked IPs) and 2000/s globally (burst 4000). Malformed,
unauthorised and over-budget requests are dropped silently. An observer is thus at most an
equal-size reflector toward an IP that already proved it receives its replies, like STUN
with CHANGE-REQUEST/RESPONSE-PORT; it cannot be aimed at other hosts. Hosts sharing one
CGNAT IP can receive these fixed-format packets on their mappings; that is accepted.

**Measurement** (one family at a time, dedicated sockets, never the shared QUIC socket):

1. A mapping socket asks each observer's primary (≤ 4 observers per family, preferring those
   with an alternate IP, then an alternate port), then the first answering observer's
   alternates. Equal mapped endpoints everywhere: EIM (`none` when equal to the local
   endpoint). Different across ports of one IP: APDM. Equal per IP but different across IPs:
   ADM; if no same-IP pair exists, unresolved EDM. At least three observer endpoints on two
   IPs are needed for a full class. Differing external IPs set `varying-ip`.
2. Five fresh sockets map toward that observer back to back. Preserving when most keep their
   local port; otherwise delta = median consecutive port difference, jitter = maximum
   deviation; sequential when 0 < |delta| ≤ 256 and jitter ≤ 64, else random.
3. The first of those sockets is the filtering socket: it never contacts anything else. It
   requests an alternate-port reply, and an alternate-IP reply when available; otherwise
   another observer IP (never contacted from it) sends to its mapping via a target-port
   request from the mapping socket. Both received: EIF; port only: ADF; neither: APDF.
4. Hairpin: one fresh socket sends a type-3 packet to another's external endpoint. Received:
   yes; missing under EIF: no; missing under ADF/APDF: unknown (filtering may drop it).
5. Lifetime (IPv4): one fresh socket per rung maps once and idles. At rung time a separate
   control socket asks the observer to send, from the endpoint the rung contacted, to that
   rung's mapping (the rung never sends again). Coarse rungs double from 5 s to 240 s; three
   fine rungs split the failing octave in quarter-octave steps; the estimate is the geometric
   mean of the alive/expired bracket (≤ 9 % bracket error). Never expired: `>=max`; already
   expired at the first rung: `<min`. Keepalive owners may shorten the 25 s default from it.
6. UDP blocked: IPv4 sockets work but no observer answers. IPv6: `unavailable` without a
   routed IPv6 socket, `blocked` without answers, else `ok` with its mapping/filtering (a
   stateful IPv6 firewall shows as ADF/APDF with mapping `none`). Port mapping protocols come
   from the socket owner's `p2p/portmap` client, not from netcheck.

A quick IPv4 profile sends about 15 requests (unanswered probes are retransmitted up to three times within
their 1 s wait) and takes roughly 1–3 s; the lifetime ladder adds 11 sockets (one per rung
plus a control socket) and takes up to about 3 × the measured lifetime.
`netcheck.Monitor` runs the quick profile on start, after each network-change hook
(`Monitor.Hook`, register on the D12 pipeline; it only triggers and returns) and every 30
minutes; the ladder runs on start, after network changes and at most every 6 h. A network
change cancels an in-flight run and discards the old lifetime; runs without observers
retry after 1 minute. Observers come from the owner. `nu relay --netcheck <ip:port>
[--netcheck-alt-port <port>] [--netcheck-alt-ip <ip>]` serves them (literal IPs, one set
per family; not yet a `[relay]` config key). A public-mode daemon profiles against its
connected configured relays that serve `/nu/netcheck/1`, registers `Monitor.Hook` at the
`RestartProbing` stage, serves `/nu/natprofile/1` and exposes `Swarm.NATProfile()` for
status/`nu doctor`; private/isolated mode starts none of this. Public Nu peers running
observers and disco's use of remote profiles are not wired yet.

**Profile v1** is 18 bytes, address-free, big-endian, every enum zero = not measured:

```
0 version=1 | 1 udp4 (ok=1, blocked=2) | 2 mapping4 | 3 filtering4 | 4 allocation4 |
5-6 delta:int16 | 7-8 jitter:u16 | 9-10 lifetime seconds:u16 |
11 flags (bit0 lifetime is a lower bound, bit1 an upper bound, bit2 varying external IP) |
12 hairpin (yes=1, no=2) | 13 ipv6 (unavailable=1, blocked=2, ok=3) | 14 mapping6 |
15 filtering6 | 16 portmap (bit0 PCP, bit1 NAT-PMP, bit2 UPnP, bit7 checked) | 17 observer IPs
```

Mapping: none=1, EIM=2, ADM=3, APDM=4, unresolved EDM=5. Filtering: EIF=1, ADF=2, APDF=3.
Allocation: preserving=1, sequential=2, random=3. Decoders accept exactly one canonical
encoding: other versions/lengths, out-of-range enums, reserved bits, delta/jitter outside
sequential allocation, bounds without a lifetime, or IPv4/IPv6 results without working UDP
are errors, never guessed. Vectors and fuzz seeds are in `p2p/netcheck`; a new field or
enum value needs version 2. The doctor form is one line, e.g. `udp=ok mapping=apdm
filtering=apdf allocation=sequential(delta=+1,jitter=0) lifetime=30s hairpin=no
ipv6=unavailable portmap=none observers=3`.

**Exchange `/nu/natprofile/1`.** The requester writes `max_version:u8` (≥ 1) and half-closes;
the responder writes `len:u8 | profile` (len 0 = not measured yet). Any Nu peer may serve
it; it is a separate protocol rather than an identify field (no libp2p field-number
squatting) or a `/nu/path/1` frame (v1 forbids profile payloads). Path owners fetch it when
a path session starts and when the peer's candidate generation changes; a profile informs
probe scheduling and escalation only, never destination authorization. Reports with
mapped addresses stay local (`nu doctor`).

Evidence (`go test -tags natlab -run TestNetcheckNATLab ./p2p/netcheck`, about 31 s with
scenarios in parallel): real packets through rootless netns fixtures, observers running
`relay.New` with the descriptor stream. EIM/EIF/preserving/hairpin, EIM/EIF/sequential via a
cross-observer (no alternate IP), ADM/ADF/sequential, APDM/APDF/random, EIM/APDF/random,
APDM/EIF/preserving, EIM home NAT behind an ADM/ADF/sequential CGNAT, and UDP blocking
(descriptor fetched first) are each classified exactly, with the fixture's sequential
delta, and lifetimes of 4–9 s within ±20 %. The fixture ports are reserved in advance, so
this does not show allocator jitter; real routers and IPv6 firewalls remain field work.

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
  QUIC DATAGRAMs. Enable a leg only when its current DATAGRAM payload budget can carry
  the complete 1200-byte inner packet plus extension overhead. A 1200-byte outer QUIC
  packet cannot do this; do not silently truncate or fragment, or assume DATAGRAM support
  alone is sufficient. Recheck the budget on outer path changes. Retain standard circuit
  streams as fallback; outer fragmentation/reassembly would require a separate bounded
  protocol.
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
probing. Nu-path keeps the logical PacketConn and virtual addresses alive while replacing
physical sockets and relay circuits. Preserve the inner QUIC connection only when recovery
beats its idle timeout and meets D11's packet budget; standard sessions, and expired inner
connections, resume at the request level. Adapt Tailscale's BSD-3 portmapper/netmon code rather
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
that a running daemon has performed the operation. `p2p/disco.Service.RestartProbing`
implements the final hook: revoke stale direct proofs, fall back to the surviving relay,
coalesce a candidate re-exchange and restart the early probe schedule. Mapping changes
call `Service.Update`; `disco.FromMapping` enforces expiry and PCP PEER restrictions.
The NAT-lab test registers all six
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

### Public dnsaddr and routing/v1 lookup

`p2p/lookup.NewDNS` and `NewHTTPSource` implement the existing `Source` interface;
add them explicitly to `lookup.Config.Sources`. They do not change discovery defaults
and must not be installed in private/isolated mode. DNS can use a configured peer domain
(`_dnsaddr.<base36-peerid>.<domain>`) and/or explicit dnsaddr roots; `Resolve` handles an
individual dnsaddr hint. It uses miekg/dns wire encoding with connected, context-bounded
DNS I/O, TXT character-string concatenation, suffix matching at every recursion hop,
cycle/deduplication checks, and UDP-to-TCP fallback on truncation. DNS servers are literal
IP:port values; absent configuration uses `/etc/resolv.conf`. No lookup server is selected
implicitly. Defaults per lookup: 3 s, 8 KiB per DNS message, 128 records total, depth 8,
16 queries, 32 results and 1024 bytes per multiaddr. DNS names are limited to 180 bytes
before the peer-label/query prefix. Result TTL is the minimum along the dnsaddr chain,
capped at five minutes. DNS answers are address claims, not authorization or DNSSEC proofs.

The HTTP source calls `GET /routing/v1/peers/{peer-id}` through `httpsrc`. It accepts
IPIP-417 `application/json` (`{"Peers":[{"Schema":"peer","ID":"…","Addrs":["…"]}]}`)
and `application/x-ndjson` peer objects, ignores unknown schemas/fields, treats legacy
404 as an empty result, and rejects conflicting destination suffixes. Defaults: 64 KiB,
32 peer objects, 32 addresses, 1024 bytes/address, JSON depth 16 and a 3 s overall/stall
bound. HTTP address freshness is at most 30 s, shortened by Cache-Control. Results are
unsigned hints; libp2p dialing authenticates the requested PeerId. The server returns
200 with an empty list when absent, omits unsupported protocol claims, supports GET/
OPTIONS CORS and JSON/NDJSON negotiation, and advertises bounded cache freshness.
IPIP-484 transport/protocol filtering, provider routing and IPNS endpoints are not part
of this service.

`relay.Config.Lookup` optionally creates a `LookupService`. Its config supplies a
`lookup.DirectoryConfig` (required peer domain, optional admission callback, default
1024 retained peers, 16 concurrent verifications, one-hour retention) and optional
`HTTPListen`/`DNSListen` strings. DNS binds UDP and TCP to the same port. Empty strings
open no listeners; `LookupService.Directory` is also an `http.Handler` and `dns.Handler`
for owner-managed frontends. HTTPListen is plaintext: public deployment requires an
HTTPS frontend. DNS is authoritative only for its peer TXT namespace and never recurses.
UDP responses are capped at 1232 bytes (512 without EDNS), with TCP fallback; TXT strings
are split at 255 bytes. The service exposes listener addresses and asynchronous errors,
and closes with the relay. No CLI or production hostname/listener is implicitly enabled.

Publication is the Nu extension **`PUT /nu/lookup/v1/peers/{peer-id}`**, with an
`application/octet-stream` body containing the unmodified RFC 0002 signed libp2p peer
record envelope. Limit: 8 KiB, 32 addresses, 1024 bytes/address. The directory verifies
the domain-separated signature, payload type, signer/record/path identity and address
suffixes, then requires a strictly increasing sequence for a resident identity. Empty
address lists withdraw the previous addresses. Responses: 200 accepted, 409 old/equal
sequence, 403 admission refusal, 413 oversized body, 422 malformed/invalid record,
429 capacity/concurrency limit. It performs no outbound work on submitted addresses.
Records and high-water marks are memory-only; TTL expiry hides records and expired
entries can be pruned for capacity. Anti-replay across pruning/restart is not claimed.
TLS and final libp2p authentication remain required; a signed address is not evidence
that its IP is reachable or belongs to the signer. Public operators still need admission,
rate/abuse policy and TLS/DNS deployment configuration.

`NewPublisher` takes the key, current-address callback, persistently increasing
`NextSequence`, configured HTTPS base URL/client and optional per-call host allowlist.
`Publish` is a change hook; `Run` publishes initially, on owner-provided notifications
(e.g. `Host.WatchChanges`) and every 30 minutes by default. The interval must be shorter
than server retention. Failures return to the owner for retry/backoff. `RelayOnly` filters
to terminal relay-circuit addresses **before signing**; direct addresses and plain dnsaddr
indirections are omitted, including when the result is empty. This cannot erase old
public/DNS caches or direct addresses published through another source. `httpsrc.Put`
is capped at 8 KiB and rejects redirects/resume, while sharing Fetch's HTTPS/IP/host,
time and response-size guards. The lookup server's public metadata is not a control API.

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

Production HTTPS-compatible fallback wire/security rules, relay admission/economics
and endpoint support remain to specify. The path v1 destination/probe contract above
requires independent review and an explicit socket-owner authorization policy before
public enablement.
The conservative stock-quic-go path is constrained by D11. Physical socket replacement,
multihomed/source-interface offload behavior and capacity-collapse/long reliable-relay
performance still need validation; the near-raw direct throughput target is not established.
Adaptive inner MTU is not part of the initial design. Public direct-first setup,
reattachment admission/crossed-attempt policy and pair-aware relay reservation remain
owning control-plane tasks. `p2p/disco` supplies replay-resistant probes and exact candidate
authorization, but does not choose the daemon's peer trust or destination policy. Fast shared-socket/NAT-lab tests establish packet-core behavior, not those public
security contracts, operating capacity or field traversal rates.

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
