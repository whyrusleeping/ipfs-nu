# NAT lab (Linux, opt-in)

Run `go test -tags natlab -count=1 -v ./p2p/natlab` (or `go test -tags natlab ./...`).
Nothing in this package is built without **both** Linux and the `natlab` tag.
Requires `ip`, `nft`, `unshare`, `nsenter`, `sysctl`, and a kernel with network/user
namespaces, veth, bridge, conntrack and nft NAT/dynamic-set support. These are stock
Linux CI facilities. No external network, named `/run/netns` mounts, kernel modules
for NAT64, or `conntrack` command are needed by the smoke tests.

## Privileges and isolation

Call `Isolate(t)` first in each **top-level** test; return immediately when it
returns false. It re-execs that test in a fresh network namespace. Unprivileged
callers also get a fresh user namespace with their UID mapped to root. A parent
has a 50-second watchdog; each child has a 45-second Go test timeout. Topologies
use their own shorter context. Missing tools/privileges **fail**, not skip.

The smoke tests run rootless on the development machine, including bridge
creation, nftables NAT/filtering, hairpin, conntrack flush, address changes and
link cycling. No sudo or host sysctls are used. On machines that disable
unprivileged user namespaces (including some AppArmor configurations), compile
with `go test -c -tags natlab -o /tmp/natlab.test ./p2p/natlab`, then run
`sudo /tmp/natlab.test -test.v`. Do not run the Go toolchain as root. Even root
uses an isolated network namespace. `.github/workflows/natlab.yml` is an
on-demand root-fallback job and uploads logs plus NDJSON results.

The library's `NU_NATLAB_ISOLATED` and `NU_NATLAB_HELPER` variables are private
re-exec markers; **do not set them yourself**. `New` refuses to run without the
isolation marker. No topology is connected to the real host network. Namespace
holders and node helpers are killed and reaped in reverse construction order.
Closing their control pipes also terminates them if their parent exits.

## API

```go
func TestExample(t *testing.T) {
    if !natlab.Isolate(t) { return }
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    lab, err := natlab.New(ctx) // check errors at each step
    if err != nil { t.Fatal(err) }
    defer lab.Close()
    internet, _ := lab.Namespace("internet")
    public, _ := internet.Segment("public", "198.18.0.1/24")
    router, _ := lab.Namespace("router")
    _ = public.Attach(router, "wan", "198.18.0.20/24", "")
    private, _ := router.Segment("lan", "10.0.0.1/24")
    client, _ := lab.Namespace("client")
    _ = private.Attach(client, "eth0", "10.0.0.2/24", "10.0.0.1")
    _, _ = router.NAT(natlab.NATConfig{
        WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr("198.18.0.20"),
    })
    node, _ := client.StartHost(natlab.HostConfig{Listen: "10.0.0.2:4001"})
    _ = node // Dial, Check (AutoNAT), Remote, Probe, Receive, SendFrom
}
```

See `lab_linux_test.go` for complete, error-checked examples. `StartHost` runs
real `p2p/host` QUIC hosts and `p2p/proto/autonat` in re-executed helpers; no
`setns` on Go threads or quic-go changes. AutoNAT's private-address override is
only enabled inside these helpers. Each node's small JSON control channel is
serialized; don't call one Node concurrently. `Namespace.Command`/`Start` can
also launch third-party binaries in a namespace with their own control protocol.

Compose separate routers/private segments for two NATs; attach an inner router's
WAN to an outer router's private segment for CGNAT. Multiple nodes on one private
segment model shared NAT. Attach more public IP aliases with `Address`, or more
observer namespaces, for multiple IPs/ports. These are composable primitives,
not an automatically-run topology matrix. A router owns one `nu_natlab` nft table;
one NAT instance per router namespace. Topology construction is sequential.

`Link`, `Address`, `NAT.Rebind`, and `NAT.FlushMappings` are synchronous scripted
events. `Run("ip", ...)` supports route changes. For sleep/wake, lower/raise the
link, then restore the default route (Linux removes it when the link goes down).
The caller decides when events occur, using protocol/transfer progress rather
than sleeps. `Rebind` rewrites NAT rules and flushes state; assign the new WAN IP
with `Address` first. Flush uses a small netfilter netlink helper, not host state.

## Real port-mapping daemon

`go test -tags natlab -run '^TestNATLabMiniUPnPd$' -v ./p2p/portmap` exercises
PCP, NAT-PMP and UPnP against miniupnpd's **nftables** backend. Set `NU_MINIUPNPD`
to an absolute binary path, or install `miniupnpd-nftables` on PATH. Without it,
this test explicitly skips; the fixture-backed portmap lab still runs. No sudo
is needed to obtain the Ubuntu binary:

```sh
tmp=$(mktemp -d)
(cd "$tmp" && apt-get download miniupnpd miniupnpd-nftables &&
  for package in *.deb; do dpkg -x "$package" root; done)
NU_MINIUPNPD="$tmp/root/usr/sbin/miniupnpd" go test -tags natlab -run '^TestNATLabMiniUPnPd$' -v ./p2p/portmap
```

The test launches the daemon only inside the isolated router, with temporary
config/PID/lease/minissdpd paths. It creates its own nft mappings; fresh public
sources must reach the private application only while mapped. miniupnpd rejects
reserved WAN addresses, so this scenario uses `11.0.0.0/24` **inside the isolated
lab only**, with no real-network uplink. Ubuntu 2.2.3's startup log confirms PCP
support as well as NAT-PMP and UPnP. Its shared libraries must be available on the
host (inspect `ldd` when using an extracted binary).

## NAT models and limits

**Kernel mode** (default) is ordinary nftables masquerade + conntrack filtering.
`Allocation: Random` uses nft `fully-random`. Default allocation attempts normal
Linux port preservation. Linux can reuse or change ports opportunistically, so
this mode is deliberately **not** named EIM, ADM or APDM. It's appropriate for
realistic AutoNAT reachability tests. It does not promise EIF or hairpin support.

**Fixture mode** explicitly reserves public ports for listed inside UDP
endpoints, with SNAT/DNAT per mapping class:

* `EndpointIndependent`: one external endpoint per inside endpoint.
* `AddressDependent`: distinct external endpoints per destination address.
* `AddressPortDependent`: distinct external endpoints per destination IP+port.

List `Inside` and `Destinations` in `NATConfig`. All addresses must be IPv4;
limits are 64 inside endpoints and 64 destination endpoints. Unlisted inside
endpoints are dropped. EIM accepts arbitrary destination addresses; ADM accepts
arbitrary ports on the listed IPs; APDM only accepts listed tuples. This is a
bounded, known-destination fixture model, **not** a general dynamic NAT daemon.
Port reservations are made at construction, not on flow arrival: `Preserve`
uses the inside port for the first class and increments on collisions;
`Sequential` allocates from 40000; `Random` reserves randomly chosen ports in
10000–59999 and resolves collisions. It is not a simulation of allocator jitter
or of fresh randomness on every conntrack binding.

Filtering is independently selectable: `FilterEndpointIndependent`,
`FilterAddressDependent`, or `FilterAddressPortDependent`. Outbound packets
refresh dynamic nft sets of active mappings and allowed remote IPs/tuples.
Unsolicited inbound packets must pass the corresponding learned filter; ordinary
conntrack replies remain allowed. Fixture `Lifetime` (default 1 minute, >=1s)
bounds idle pinholes and learned filter entries, **not** established conntrack
flows. It is not evidence for mapping-lifetime algorithms. `FlushMappings`
destroys both conntrack flows and learned pinholes. Hairpin DNAT plus private-side
masquerade is opt-in with `Hairpin`. `BlockUDP` drops all forwarded UDP, including
established flows, in either mode. ICMP/other traffic is not a simulated UDP path.

The knobs support all nine mapping/filtering pairings, but only three
representatives run by default, per the v0 test budget. All combinations outside
those representatives remain unvalidated. Fixtures aren't a substitute for
measurements against real NAT implementations.

## Acceptance evidence and output

* `TestAutoNATReachability`: a genuinely public node is nonce-verified Reachable;
  a node using a real masqueraded connection is Unreachable at its **observed
  external endpoint** from a fresh source port. Both within 5s from startup
  (development run: roughly 0.4s and 0.75s). This supplies 0012's NAT-lab check.
* `TestMappingAndFiltering`: three destination tuples on two public IPs prove
  EIM yields one port, ADM two, and APDM three. Unsolicited packets from the same
  endpoint, same-IP/different-port, and another IP verify the corresponding
  filters **before** those alternative destinations are contacted. Preserving
  and random fixture allocation are exercised. This supplies 0003's smoke check.
* `TestHairpinAndNetworkChanges`: checks hairpin off/on, flush, address rebinding,
  link down/up plus route restoration, and total UDP blocking.
* `TestCircuitToNATTarget`: a private target reserves on a public relay; an outside
  client echoes raw circuit bytes to it. Checks scheduled reservation renewal and
  an actual 1s duration-limit reset. Helpers opt in with `CircuitClient` /
  `CircuitServer`; these raw echo helpers do not emulate secure peer connections.

Tests emit `Outcome` JSON records. Set `NU_NATLAB_RESULTS=/absolute/path.ndjson`
to append standalone records (including through isolation re-execs), or capture
`go test -json`. Records identify scenario, pair, outcome and elapsed nanoseconds.
Optional `time_to_first_byte_ns`/`time_to_direct_ns` fields are omitted when not
measured; these protocol smoke tests do not pretend to measure file transfers.
The on-demand CI job collects standalone records as an artifact.

Deferred: NAT64/IPv6-only scenario presets and validation, third-party lab
consumers, CGNAT/two-NAT end-to-end assertions, transfer-in-flight network-change
measurements, exact mapping timeouts, sequential jitter, per-subscriber quotas,
IPS-like drop-after-N rules, and the exhaustive RFC matrix. None of these are
silently emulated by a weaker masquerade rule. They can be added by the feature
consumers without expanding normal `go test ./...`.
