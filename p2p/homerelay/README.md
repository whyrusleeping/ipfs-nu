# Home/backup reservations

`Manager` owns only circuit-v2 **reservation** lifecycle. It takes an explicit
bounded known-relay list; there is no public default list and no admission or
operator provisioning policy here. It does not turn raw circuits into authenticated
host connections.

- Supply a Host, its circuit Client and an authenticated latency probe (normally
  identify/ping's `Service.Ping`). One literal QUIC endpoint per relay PeerID is
  supported. DNS resolution is the lookup owner's responsibility.
- Unknown reachability starts with reservations needed. At most two relays are
  reserved, chosen from measured candidates by RTT. Four concurrent probes and
  sixteen configured relays bound setup work. Probes are canceled/joined on Close.
- Renew halfway to expiration. `Renew(ctx)` requests serialized early renewal for
  D12; it is not a second competing renewal loop. A failed reservation is withdrawn
  before probing replacements. The surviving backup becomes home; a replacement
  fills the backup slot. Healthy homes are not repeatedly shuffled by RTT jitter.
- Health probes run every five seconds by default (two-second request timeout).
  Known outer-connection loss triggers recovery through Host.WatchChanges without
  waiting for the reservation TTL. A blackholed relay requires a failed health
  probe or QUIC timeout; no instantaneous failure-detection claim is made.
- Retry failed candidates with bounded exponential backoff (1–60 seconds by
  default). State includes RTT, expiry, last success and a readable last error.
- `Addresses` publishes only configured, live, unexpired reservations bound to the
  exact connection that reserved them, as full public circuit hints including the
  destination PeerID. It does not publish relay-supplied extra endpoints or local
  magicsock virtual routing addresses. `Changed` is a coalescible publication
  signal; health-stat changes alone do not cause identify pushes.
- Feed confirmed public reachability into `SetNeeded(false)` to drop local STOP
  admission and withdraw relay addresses; private/unknown results use `true`.
  Private/isolated owners must set `Disabled: true` (or not create the component).
  A disabled manager performs no dialing/reservation/health work, even when asked
  to renew or enable reservations.
- Close the manager before Client/Host. It owns neither. Circuit-v2 has no wire
  cancellation message: dropped reservations disappear locally at once and expire
  at the relay naturally or when their underlying connection closes. Dropping one
  does not close an existing circuit leg; its path owner handles that lifetime.

## Assembly boundary (0020)

These APIs are not yet wired into `node/swarm`. The connectivity owner still needs:

1. A magicsock-backed Host and a circuit-aware `host.SessionConfig.Dial` adapter.
   Use `circuit.ParseAddress` to split hints and independently authorize relays and
   destinations before dialing. Never put the local qpath dial address in public
   hints/cache. Sessions validate returned host/identity and retain their gates,
   canonical sessions, bounded races and cancel/join behavior.
2. Bounded incoming STOP/qpath admission, deterministic crossed-attempt handling,
   and one recovery initiator. Close/join the old relay leg before reattachment.
   This manager promotes **reservations**, not active inner QUIC packet paths.
3. `proto.Config.Addresses` combining advertised direct and live circuit addresses,
   shared with Swarm.Addrs/share links. Route `Changed` to
   `identify.AddressesChanged`. Authenticated fresh `OnPeerRecord` callbacks can
   carry peer failover routes, but are not independent destination authorization.
4. Disco's independent exact-endpoint authorization/candidate disclosure policy,
   plus AutoNAT/portmap/netmon hooks. Physical socket replacement remains absent;
   a no-op hook must not claim successful rebind.
5. Minimal `swarm.relays` configuration, status/doctor surfaces, the 0072 DHT
   attachment seam, and real daemon/share-link NAT-lab acceptance evidence.

The fast tests here use real loopback Hosts/circuit servers. Controlled latency
rankings make selection deterministic while probes still perform real ping I/O.
They cover renewal, failover promotion, backup replacement, public-reachability
withdrawal/restoration and inert disabled mode. They do **not** establish two-NAT
daemon sharing, direct-path upgrades, or error-free active-get relay failover.
