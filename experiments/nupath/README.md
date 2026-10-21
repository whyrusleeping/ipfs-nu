# Stock-quic-go Nu-path feasibility spike (0038)

Disposable **test-only** code in the separate experiments module; nothing is imported
by the daemon. No dependency changes, quic-go fork, iroh or new production transport.
The decision and measurements live in [validation](../../docs/validation.md#stock-quic-go-nu-path-spike-0038-2026-10-21);
the owning contract is [networking D11](../../docs/networking.md#d11-nu-path).

From the repository root:

```sh
(cd experiments && go test -count=1 -v ./nupath)
(cd experiments && go test -tags natlab -run '^TestNATPaths$' -count=1 -v ./nupath)
(cd experiments && go vet -tags natlab ./nupath && staticcheck -tags natlab ./nupath)
```

Linux `natlab` requires unprivileged user/network namespaces and `ip`, `nft`,
`unshare`, `nsenter`, as described in [the lab README](../../p2p/natlab/README.md).
There is no Internet traffic. It fails rather than skipping unavailable isolation.
Use `-count=3` for the small repetition set in the report. For process CPU/RSS without
compilation, build once with `go test -c -o /tmp/nu-nupath-spike.test ./nupath` from
`experiments/`, then run `/usr/bin/time -v /tmp/nu-nupath-spike.test -test.run
'^TestPaths$' -test.count=1 -test.v`.

## What runs

- `TestPaths`: real circuit-v2 CONNECT/STOP between three Nu QUIC hosts; negotiate
  `/nu/qpath/1`, then libp2p-TLS inner QUIC through uvarint-length packet frames.
  A continuous 17+ MiB deterministic stream crosses relay, direct, 60 ms RTT / 2%
  deterministic packet loss, 250 ms all-path outage, and a replacement circuit with
  **new outer QUIC connections**. SHA-256 verification and stream EOF require no
  inner redial or application replay. Metrics come from stock qlog event hooks.
- `TestMTU`: let stock DPLPMTUD increase packet size on the initial path, then
  silently blackhole UDP payloads over 1200 bytes. Compare against explicit
  1200-byte packets with DPLPMTUD disabled. Restore the ceiling and finish/hash
  the same stream. This is a payload-ceiling simulation, not a kernel interface
  MTU change or ICMP test.
- `TestOutagePastIdle`: blackhole all inner packets past the two-second test idle
  timeout. Connection survival must fail; this does **not** implement/test Nu's
  product-level request resume.
- `TestDatagramBudget`: a 1200-byte outer packet budget cannot fit a 1200-byte
  inner QUIC datagram. A 1400-byte outer budget can, on loopback. No Nu relay
  DATAGRAM extension is implemented.
- `TestOOBBatchHook` (Linux): demonstrate the `ReadMsgUDP` fd-bypass trap and the
  necessity of an explicit `ReadBatch` hook. This is a dispatch check, not a
  complete virtual-address OOB implementation or GSO/ECN correctness result.
- `TestRelayBackpressure` (Linux): a stalled reliable writer cannot block the inner
  send call or grow a transmit queue past 64 datagrams; closing joins the writer.
- `TestNATPaths` (Linux, tagged): two independent private namespaces and EIM/APDF
  NAT fixtures plus a public relay. Namespace socket fds are passed with
  SCM_RIGHTS to the test process; sockets retain their original namespace routing.
  Activate mappings with an observer send, then simultaneously send disposable
  probes from the QUIC sockets. The receiver checks the public translated source.
  Transfer on relay, upgrade to direct, kernel-drop direct UDP, explicitly fall
  back, take a router WAN down/up, and replace the outer connections/circuit, all
  within one inner QUIC stream. There is no host-network `setns` on Go threads.

## Deliberate boundaries

This is not 0015. One fixed virtual peer address per pair; separate inner UDP and
outer relay UDP sockets; one direct candidate; explicit scripted path decisions;
unauthenticated test probes. No shared inner/outer CID dispatcher, socket replacement,
TLS-exporter probe MAC/replay protection, authenticated reattachment protocol,
automatic heartbeat/failover, content protocol, request resume, relay admission policy,
real consumer routers, dynamic NAT allocator or field success-rate claim.

The baseline PacketConn exposes DF via `SyscallConn` but **not** OOB/GSO/ECN; it has
bounded packet channels and separate reliable readers/writers. Finite PacketConn I/O
deadlines are explicitly unsupported in this disposable adapter. The tests close it
before closing its QUIC Transport. Production code needs full deadline/lifecycle
semantics and per-peer routing/budgets, not a copy of this adapter.
