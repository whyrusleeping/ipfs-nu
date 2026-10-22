# Circuit relay v2

Implements [libp2p circuit-v2](https://github.com/libp2p/specs/blob/master/relay/circuit-v2.md)
RESERVE, HOP CONNECT and STOP, including signed reservation vouchers. These are
**raw circuit streams**, not secure end-to-end libp2p connections. Task 0015 will
negotiate `/nu/qpath/1` and carry authenticated inner QUIC over them. Standard-peer
Noise/yamux upgrades are explicitly outside 0013; no transport is registered with
`p2p/host`, no secure peer identity is inferred from STOP, and no content handlers
are run on plaintext circuit bytes.

## Client

`NewClient(h, ClientConfig{})` registers STOP. Its defaults are 8 reservations,
32 active circuits and a 10s setup timeout. First establish a direct QUIC
connection to a relay with `h.Dial`; this package does not perform discovery or
implicitly dial supplied peer addresses.

* `Reserve(ctx, relayID)` obtains/renews a slot. It returns expiration, relay
  addresses, connection limits and the raw voucher. When a voucher is supplied,
  its envelope signer, codec, relay, reserving peer and expiration must all match.
  Missing vouchers are accepted because the standard makes them advisory; Nu
  servers always issue them. Addresses are observations, not reachability proofs.
* `Maintain(ctx, relayID, update)` reserves immediately and renews halfway to
  expiration. It returns on cancellation or the first failure; the owner selects,
  redials/replaces relays and publishes address changes from the callback. The
  callback must return promptly. Cancellation stops maintenance but does not send
  a nonstandard cancellation message: the reservation expires or is invalidated
  on disconnect.
* `Connect(ctx, relayID, targetID)` returns a `*Circuit` after HOP/STOP succeeds.
  Its context only bounds setup. `Accept(ctx)` returns inbound circuits. STOP is
  accepted only over the exact direct connection on which this client reserved,
  while that reservation is valid. Incoming circuits are bounded and queued.
* Always `Close` or `Reset` the **Circuit**, even after EOF. `CloseWrite` sends FIN
  without preventing reads; `Close` releases both sides. Closing the client aborts
  active operations and circuits but does not own/close the host.

`Circuit` embeds `*host.Stream` and carries `Relay`, `Peer` and `Limit` metadata.
The peer field on STOP is the relay's **assertion**, not an end-to-end identity.
The caller must negotiate a security protocol and authenticate the remote peer
before treating the stream as a peer connection. `Limit` is seconds and bytes
**per direction**, with zero meaning unlimited. Relays enforce these limits;
clients surface them to their next-layer consumer.

## HOP server and admission

`NewServer(h, key, ServerConfig{})` registers HOP. The signing key must match the
host identity. Reservations are passive: CONNECT uses only the exact existing
reserving connection, never an initiator-supplied address. Expired/disconnected
entries are discarded before admission/lookup; retained maps remain bounded.

| Control | Default |
|---|---|
| Reservation TTL | 1 hour (configurable 1s–24h; rounded up to Unix seconds) |
| Reservation slots | 128; one reservation per PeerId |
| Reservations per observed IP | 8 |
| Active circuits | 128 |
| Active circuits per peer / IP | 8 / 16 |
| Pending handshakes | 32, each bounded by 10s |
| Non-admitted circuit duration / bytes | 120s / 128 KiB in **each** direction |
| Admitted circuit limit | Unlimited duration/data; slot/IP/peer limits still apply |

`Limited` must set both positive duration and data caps; `{}` selects the defaults.
`FullLimit` may set larger finite caps or remain zero. `Admitted` is an explicit
allow-list (at most 1024 IDs), copied at construction. **Both** source and target
must be admitted for full service: admission of a popular destination cannot
launder unadmitted clients into unlimited service. An admitted target's reservation
reports its potential full-service limit; each CONNECT reports the actual pair
limit to both endpoints. Optional concurrent `AllowPeer(id, ip)` can refuse
reservations and circuits entirely; it must not block. IPv4-mapped IPs normalize
to IPv4. IP accounting uses observed transport IPs, not peer-advertised addresses;
the same IP appearing on both ends counts once per circuit.

Exceeding either directional data cap or the circuit duration resets **both**
legs. Duration starts after successful setup. FIN is propagated independently in
each direction; service shutdown/connection loss releases the circuit resources.
Reservations may expire without interrupting an already active circuit.

`ServerConfig.Addresses` supplies up to 16 concrete direct QUIC relay addresses,
including `/p2p/<relay>`. Empty uses the host's explicit listeners. Wildcard
listeners need operator-supplied advertised addresses; a wildcard is not silently
advertised. No public reachability is inferred merely from address configuration.

## Wire bounds and vouchers

HOP `/libp2p/circuit/relay/0.2.0/hop` and STOP
`/libp2p/circuit/relay/0.2.0/stop` use minimal-uvarint-length-delimited protobufs
with a 4096-byte frame cap (matching go-libp2p). Peer IDs use the existing wire
profile; address lists have at most 16 entries, each at most 1024 bytes. Nested
messages are explicitly bounded and have fixed depth. Unknown fields are skipped;
duplicate known singular fields, wrong wire types, overflowing duration/expiration
values and missing required type/peer/expiration fields are rejected. Passive
relay ignores bounded peer-address hints. Status codes are the standard 100,
200–204 and 400–401; failed operations return typed `Status` errors.

Voucher payload fields are canonical protobuf in relay/peer/expiration order,
without unknown fields. Envelopes use domain `libp2p-relay-rsvp` and codec bytes
`03 02`, signed with the relay's host identity key via existing `p2p/wire` signed
envelopes. Verification compares the complete canonical payload as well as signer
and codec; vouchers remain advisory evidence, not authorization to dial targets.
`testdata/messages.json` contains literal control-message vectors; fuzz seeds
exercise HOP, STOP and their nested structures in normal `go test`.

## Standalone assembly / CLI hook

`relay.New(relay.Config{Host: host.Config{Key: key, ...}, Server: ...})` owns a
QUIC host, identify/push/ping service, AutoNAT v2 server, and circuit HOP server.
Default listen is `0.0.0.0:443`; pass concrete advertised addresses for that
wildcard. `Close` shuts down services then the host. No default public operators,
account identity, content daemon, DNS routing server or implicit admission is
configured. Production admission/quotas/operators/cost remain 0039.

`cmd/nu/cmd_relay.go` (`nu relay`) does exactly this: it loads the persisted node key
under the repo lock, parses listen (`/ip4|ip6/<ip>/udp/<port>/quic-v1` or host:port),
advertised-address, admitted-peer and limit flags over the `[relay]` config section,
calls `relay.New`, prints the serving addresses (`--json` for scripts), waits for
SIGINT/SIGTERM and calls `Close`. Bind errors and wildcard listeners without
`--advertise` fail plainly. UDP 443 may require deployment capabilities/privilege;
don't run the Go toolchain as root.

AutoNAT v2 probes already use a fresh source port, distinct from the request
connection. There is **no standard AutoNAT v2 alternate-source-IP request field**;
this implementation does not invent one. Explicit alternate-IP source selection
and a coordinated multi-observer profiling request API remain for the profiling
consumer. Multiple bound relay/observer instances can be operated on distinct
IPs, but that alone is not proof that probes originate from an alternate IP.

## Evidence

* `go test ./p2p/proto/circuit ./relay`: loopback three-peer raw echo/FIN, signed
  vouchers and renewal, no-reservation rejection, reservation-IP/circuit slots,
  admitted transfer beyond the limited byte cap, and directional byte enforcement
  for an unadmitted source targeting an admitted destination.
* `go test -tags natlab -run TestCircuitToNATTarget -v ./p2p/natlab`: rootless
  namespace NAT target, observed masqueraded reservation, successful public-client
  circuit echo, scheduled reservation renewal, and a real 1s duration-cap reset.
* In `experiments/`, `go test -run '^TestCircuitV2$' -v .`: upstream go-libp2p's
  reservation client against Nu, Nu against upstream's relay, vouchers verified
  both ways, and a Nu CONNECT/STOP raw stream through upstream's actual relay.
  This deliberately does not claim a stock peer's Noise/yamux connection upgrade.

## Nu datagram relay extension (0019)

`datagram.go` / `datagram_server.go` / `datagram_client.go` implement
`/nu/relay/datagram/1/{hop,stop}` from [D11c §5](../../../docs/networking.md#5-relay-optimization):
an admitted pair with a live stream circuit asks the relay to forward inner QUIC packets
as outer QUIC DATAGRAM frames (`channel:u16 | packet`). The relay allocates one channel
per outer connection, offers the pair to the target over its reservation connection,
proves the 1202-byte payload budget on both legs with `host.Conn.ProbeDatagram`, then
sends READY. The stream circuit remains the fallback; the pair ends with either control
stream, either connection, the relay, or the pair's last stream circuit. `DatagramLeg`
satisfies `magicsock.PacketLeg`: a too-large packet is reported as not sent so the owner
writes it on the stream. Fuzz: `FuzzDatagramMessages`. Evidence:
[validation](../../../docs/validation.md#datagram-relay-extension-0019-2026-10-22).
