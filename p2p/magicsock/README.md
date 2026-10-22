# Magicsock / Nu-path packet core

One `Conn` owns one UDP socket and implements `net.PacketConn`, quic-go's
`OOBCapablePacketConn`, **and its explicit `ReadBatch` hook**. The quic-go read loop
is the sole physical reader, receiving directly into its own batch buffers. Real-address QUIC (including outer circuit connections and standard
libp2p peers), inner QUIC and disco envelopes share that socket and dispatcher.

The wire contract is [D11](../../docs/networking.md#d11-nu-path). This is the packet
core, **not automatic traversal or public relay/session orchestration**.

## API / ownership

- `New(Config{UDP: udp})`: takes ownership on success only. Defaults: 128 peers,
  256 queued relay receive packets, 64 queued transmit packets per relay. One relay per peer;
  direct path sets have at most 32 endpoints; CID routing has at most 64 entries per
  peer. Limits are checked before allocation. `Close` interrupts I/O and joins workers.
- `AddPeer(PeerID)` / `PeerAddr`: stable local-only `netip.AddrPort` per peer.
  IPv4 uses `240.0.0.0/4`, IPv6 uses discard-only `100::/64`, port 1. Removed
  addresses are never reused; reserved destinations never escape onto UDP.
- `SetDirectPaths(peer, []AddrPort)`: **caller-authorized, validated** endpoints,
  not candidates supplied by an untrusted peer. Does not send probes or switch.
- `UseDirect(peer, endpoint)` / `UseRelay(peer)`: explicitly select the send route;
  an unset direct selection means relay. Bootstrap the inner TLS handshake over
  qpath before selecting direct; direct-first virtual handshakes are not implemented.
  `Paths` returns a detached snapshot.
  Incoming inner packets are accepted only from authorized direct endpoints and
  routed by destination CID, not by endpoint alone. Unknown CIDs remain physical.
- `AttachRelay(peer, negotiatedStream)`: takes ownership on success. The stream
  must already have selected `/nu/qpath/1`; Close/Reset must interrupt concurrent
  I/O. Returns a `Relay` handle with `Done` and joining `Close`. Close the old leg
  explicitly before reattaching: relay-asserted identity never overwrites a live leg.
  A failed leg keeps its slot until Close joins both workers, preventing rapid failed
  reattachments from exceeding the worker budget. Per-leg framing/EOF/I/O failures
  do not close the logical PacketConn.
- `RemovePeer`: close its relay and discard routes/CIDs; applications must also
  revoke/close the authenticated host connection if removing membership.
- `WriteDisco` / `ReadDisco(ctx, buffer)`: bounded socket envelope only. Physical
  source addresses survive demux. UDP demux is driven by the active QUIC read loop;
  ReadDisco is not a second socket reader. There are no automatic responses, MAC
  checks, destination authorization or path changes; those belong to 0017.
- `Stats`: counters and peer/CID counts, not a reachability or authentication claim.

Relay writers are independent and bounded. Overflow drops QUIC datagrams rather
than blocking the QUIC event loop or accumulating an unbounded stream backlog.
Read/write deadlines are implemented on the logical PacketConn; close wakes readers.
Do not separately read or close the underlying UDPConn after ownership transfer.

## quic-go / host integration

`host.New(host.Config{PacketConns: []net.PacketConn{socket}, Key: key})` installs the
optional structural `host.PacketConnPolicy` hooks. Host sets an eight-byte local CID
length on policy sockets; ordinary UDP hosts keep the stock default. For direct quic-go users, set `Transport.ConnectionIDLength` to
`magicsock.ConnectionIDLength` and apply `QUICConfigForPeer` on both the listener's
`GetConfigForClient` and the dial configuration. Do not replace its tracer.

For virtual peers the policy fixes inner packets at 1200 bytes and disables PMTU
discovery. A synchronous **public quic-go qlog event observer** registers initial
source CIDs and issued `NEW_CONNECTION_ID` values, removes retired CIDs and releases
routes on connection close. It never serializes logs. This is a correctness dependency
on pinned quic-go v0.64.1's public event API; upgrading quic-go must run the rotation
and shared-socket tests. Handshake-header sniffing alone misses encrypted CID issuance.

Host verifies that the authenticated libp2p TLS peer matches the virtual-address
binding **before application admission**. Circuit metadata does not authenticate a
peer. `qpath.Dial`/`qpath.Accept` select the security protocol and attach the leg;
then normal `Host.Dial(localVirtualMultiaddr)` performs inner end-to-end authentication.
Existing `host.Sessions` can use that local address via `SetPeer`, without new racing,
reservations or fallback policy. **Never persist, cache publicly or advertise virtual
addresses.** Sessions skip virtual verified-cache entries and reject virtual discovery
hints; identify omits virtual observed addresses. Listen addresses and sealed own peer
records remain physical. Future orchestration must preserve those publication guards.

Physical-address packets retain OOB/GSO/ECN metadata. Inner relay writes split GSO
super-packets into complete frames. Inner direct writes preserve GSO but discard ECN
and stale source/interface metadata; inner receives have no physical OOB metadata.
This deliberately leaves inner ECN unavailable (QUIC validation falls back), instead
of pretending reliable-stream packets have meaningful IP ECN marks. Kernel batched
receive and the explicit virtualized `ReadBatch` remain active. Normal UDP batches
have no intermediate copy, pooled receive buffer or per-packet channel hand-off.
Filtering holes are compacted by copying, never swapping quic-go's buffer pointers.
A queued relay arrival interrupts an idle fd read using a private deadline, without
expiring the caller's logical deadline; relay frames alone use the receive/copy queue.
CID keys are inline values, virtual source addresses are allocated per peer/batch,
and GSO control messages are borrowed views rather than rebuilt allocations.

**The original ~5% versus uninstrumented raw QUIC target remains unmet.** Profiling
and a raw-socket/no-op-observer control attribute the remaining ~16–22% measured gap
to stock quic-go's full qlog event construction required by CID tracking, rather than
lost batching/GSO or the removed queue. The optimized adapter is approximately on par
with that instrumented raw control. See the [profiling report and proposed acceptance
split](../../docs/validation.md#nu-path-throughput-profiling-0015-2026-10-21);
that proposal does not silently redefine or close the original criterion.

## Packet legs (0019)

`Relay.AttachPacketLeg` adds one negotiated unreliable `PacketLeg` (the circuit
datagram extension) to a live stream relay. The writer tries the leg first and writes
a packet on the stream when the leg reports it as not sent (outer budget) or on error
(the leg is then closed and detached); the leg's reader delivers into the same receive
queue. `Stats.DatagramSent/DatagramReceived` and `PathSet.Datagram` expose it. The
stream leg is never replaced by a packet leg.

## Next owners

0017: use the inner `host.Conn.ExportKeyingMaterial` API to derive domain-separated
probe keys; specify payload/MAC/replay/destination/rate rules before interpreting disco.
Drive `SetDirectPaths` / `UseDirect` / `UseRelay`, heartbeats and re-probing. Socket
replacement/rebinding is not implemented by this core; interface down/up without
replacing the bound socket is tested.

0020: own bounded circuit accept/setup concurrency, relay/peer admission, reservations,
renewal, dial racing, home/backup relay choice and reconnection. Observe `Relay.Done`,
close the old handle, negotiate/attach a replacement; reconnect and resume requests if
the inner idle timeout has expired. Neither this package nor qpath owns a Host, a
circuit Client or a Sessions manager. Closing qpath handles must precede closing the
circuit client/host; Host closes its supplied PacketConn.

## Fast evidence

```sh
go test ./p2p/magicsock ./p2p/proto/qpath
go test -tags natlab -run '^TestQpathNAT$' -count=1 -v ./p2p/proto/qpath
(cd experiments && go test -run '^TestQpathDeclined$' -count=1 -v ./nupath)
```

Default tests include framing fuzz **seeds**, bounded queue/deadline/identity checks,
CID issuance/retirement, GSO relay segmentation, and a shared-socket loopback stream
surviving relay → direct → relay → reattachment. The tagged rootless lab uses two
EIM/APDF NATs and a public relay, one UDP socket per host, with scripted probing/path
selection and a WAN interface flap. These are not field traversal-rate measurements.
An opt-in raw/virtual throughput benchmark is in `experiments/nupath` and uses
`slow.Skip`; the original ~5% direct overhead target is **not met by current evidence**.
