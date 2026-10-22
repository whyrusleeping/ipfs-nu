# quic-go connection-ID lifecycle callback (backlog 0070)

Experiments-only. The main Nu module stays on **stock** quic-go v0.64.1; nothing here
is imported by it. This directory holds an upstream-ready quic-go patch, a measurement
module that builds against the patched copy, and the text a human can send upstream.

| File | Purpose |
|---|---|
| `0001-add-Config.ConnectionIDTracer.patch` | `git format-patch` against quic-go **v0.64.1**; also applies cleanly to quic-go master `2ccf27f2` (2026-10-22) |
| `setup.sh` | creates gitignored `./quic-go`: v0.64.1 from the module cache (`go mod download`, checksum-verified) + `git am` of the patch |
| `go.mod` | separate module; `replace` quic-go ⇒ `./quic-go`, nu ⇒ `../..` |
| `magicsock/` | snapshot of `p2p/magicsock` at `241e2ce`; only `quic.go` (observer), `cid_test.go`, `doc.go` and the `cidRoute.owner` type differ |
| `bench_test.go` | rerun of the 0015 method (`BenchmarkCIDTracer`) + a fast virtual-direct CID-rotation acceptance test |
| `bench-*.txt` | raw `go test -bench` output behind the numbers below |

```sh
cd experiments/quicgo-cid && ./setup.sh
go vet ./... && go test ./...
NU_SLOW=1 GOMAXPROCS=4 go test -run '^$' -bench '^BenchmarkCIDTracer$' -benchtime=1s -count=5 -benchmem .
# patch's own tests, in the patched copy:
(cd quic-go && go test -run 'TestConnIDGenerator|TestConfig' . && go test -run TestConnectionIDTracer ./integrationtests/self/)
```

`cd experiments && go test ./...` does not descend into this nested module.

## Proposed API

```go
type Config struct {
	// ...
	Tracer func(ctx context.Context, isClient bool, connID ConnectionID) qlogwriter.Trace
	// ConnectionIDTracer is called once for every new connection,
	// with the same arguments as the Tracer.
	// The returned ConnectionIDTracer (which may be nil) is notified about the lifecycle
	// of the connection IDs that the Transport uses to route packets to this connection.
	// Unlike the Tracer, it doesn't cause any per-packet work.
	ConnectionIDTracer func(ctx context.Context, isClient bool, connID ConnectionID) *ConnectionIDTracer
}

// For a single connection, the callbacks are never called concurrently,
// but callbacks for different connections may be.
// The callbacks must not block, and they must not call any methods on the connection.
type ConnectionIDTracer struct {
	// Called for the handshake connection IDs and for every connection ID issued in a
	// NEW_CONNECTION_ID frame, before the connection ID is sent to the peer.
	AddedConnectionID func(ConnectionID)
	// Called 3 PTOs after the peer retired a connection ID, or when the connection is
	// closed; exactly once for every connection ID passed to AddedConnectionID.
	RemovedConnectionID func(ConnectionID)
}
```

Semantics: the callbacks mirror exactly the changes the connection makes to its
Transport's packet-handler map. Added fires for the server's copy of the client's original
DCID and the handshake source CID (at construction), then in `issueNewConnID` right after
the Transport's own `AddConnectionID`, before the NEW_CONNECTION_ID frame is queued.
Removed fires where the Transport routing entry is removed or handed to the closed-connection
handler: `RemoveRetiredConnIDs` (retired IDs after their 3-PTO grace; the client's original
DCID after handshake completion), `RemoveAll`, and `ReplaceWithClosed`.

Implementation: one optional `*ConnectionIDTracer` pointer in `connIDGenerator`, set from
`Config.ConnectionIDTracer` inside `newConnection`/`newClientConnection` (no constructor
signature change for test hooks). When unused the cost is a nil check **on CID lifecycle
events only**; the per-packet path is untouched. Tests: `TestConnIDGeneratorTracer`
(order, before-send, delayed retirement, exactly-once on both close paths, nil members),
`TestConfigClone` updated, and integration `TestConnectionIDTracer`: a 50 MB transfer
forces CID rotation; every received 1-RTT packet's DCID must be active in the receiver's
tracer at receive time; all IDs are removed after close. Unit, internal, qlog and
integration tests pass (also `-race` for the touched tests). Local golangci-lint is too old
for quic-go's `.golangci.yml`; upstream CI will lint.

### Why this shape (alternatives)

- **Selective qlog event construction** (e.g. per-event-type filter on `qlogwriter.Trace`):
  event construction happens at ~50 `if c.qlogger != nil` call sites, and packet events
  carry every frame; filtering would need per-site checks and would still allocate
  packet events containing NEW_CONNECTION_ID/RETIRE frames. Much larger change.
- **Transport-level callback** (`Transport.ConnectionIDAdded(conn, id)`): would also
  work, but Nu (and likely others) select per-peer behaviour per *connection* via
  `GetConfigForClient`/`Dial` configs, mirroring `Config.Tracer`.
- **Expose sequence numbers / stateless reset tokens**: not needed for demultiplexing;
  can be added later as extra fields without breaking callers.
- Name is bikesheddable (`ConnectionIDHooks`, `ConnectionIDEvents`).

## Results (0015 method, 2026-10-22)

Same host/settings as docs/validation.md "Nu-path throughput profiling (0015)": AMD EPYC
9684X, Go 1.26.2, `GOMAXPROCS=4`, UDP loopback, 4 MiB unidirectional stream per op over one
established libp2p-TLS connection, 1200-byte fixed packets, PMTUD off, eight-byte CIDs,
1 MiB/4 MiB receive-window caps. Shared machine (load average 15–25 of 192 CPUs), two
five-repeat sets pooled (n=10), MB/s decimal, median (range):

| Case | MB/s | vs Raw | B/op | allocs/op |
|---|---|---|---|---|
| Raw | 726 (677–819) | — | 479,329 | 15,308 |
| RawObserver (no-op qlog) | 570 (497–608) | −21.5% | 2,818,348 | 42,671 |
| RawCIDTracer (no-op hook) | 741 (690–803) | +2.0% (noise) | 477,311 | 15,312 |
| VirtualDirectQlog (current magicsock) | 568 (337–615) | −21.8% | 2,857,543 | 44,502 |
| **VirtualDirect (hook magicsock)** | **703 (654–734)** | **−3.2%** | 528,880 | 17,195 |
| RawPMTU | 836 (329–938) | (+15.1%) | 403,169 | 12,753 |

Per set, VirtualDirect was 3.7% (689 vs 716) and 2.4% (714 vs 732) below Raw. Same-session
stock baseline (`experiments/nupath`, unpatched quic-go, n=5): Raw 747, RawObserver 555,
VirtualDirect 601 MB/s (−19.5%). Single outliers (337, 329) are shared-machine stalls,
kept in ranges. The remaining ~50 KB/op and ~1.9k allocs/op are magicsock's own.

## Upstream submission (human steps)

quic-go's [AI-POLICY.md](https://github.com/quic-go/quic-go/blob/master/AI-POLICY.md)
requires disclosing AI use, a submitter who understands every line, and short,
human-written/edited descriptions. **Edit the texts below in your own words before posting.**

1. Read the patch (`git -C quic-go show HEAD` after `./setup.sh`) until you can defend
   every line, especially the exactly-once and before-send guarantees.
2. Open an issue first (API additions are discussed before PRs) using the draft below.
3. If the maintainers agree on the shape (they may rename it or prefer another design),
   fork quic-go, `git checkout -b connection-id-tracer origin/master`,
   `git am /path/to/0001-add-Config.ConnectionIDTracer.patch` (re-author with
   `git commit --amend --reset-author` if needed), run `go test ./...` and
   `golangci-lint run` (CI version), push, and open the PR with the draft below,
   linking the issue. Adapt to any naming changes.
4. Once a **stock release** contains the API: bump quic-go in the main module and switch
   `p2p/magicsock/quic.go` to this directory's `magicsock/quic.go` observer (see
   docs/networking.md D11), then rerun this benchmark with the main-module packages to
   close 0070's acceptance criterion. Delete this module afterwards.

Side observation (not part of the patch; verify before reporting): in `server.go`
`handleInitialImpl`, if `AddWithConnID` fails, `conn.closeWithTransportError` waits for
`conn.ctx.Done()` before `conn.run()` was started, which looks like it could block.

### Draft issue

> **Title:** API to learn when connection IDs are issued / retired (without qlog)
>
> We use quic-go in Nu, a p2p file-sharing daemon. Several QUIC connections share one UDP
> socket with our own packet routing (relay vs. direct paths), so the socket wrapper must
> know which local connection IDs belong to which peer, including IDs issued later in
> NEW_CONNECTION_ID frames and retired ones.
>
> Today the only way to see this is a qlog tracer. Even with a recorder that ignores almost
> every event, quic-go has to construct all packet/frame events, which costs us ~20%
> throughput and ~6x allocated bytes on a loopback bulk transfer.
>
> Would you accept a small callback for this? For example a
> `Config.ConnectionIDTracer` factory (same signature as `Config.Tracer`) returning
> `AddedConnectionID` / `RemovedConnectionID` callbacks, called from the places where the
> connection adds/removes IDs in the Transport's routing table. We have a patch with tests
> and are happy to change naming/shape.
>
> AI disclosure: the patch and this draft were written with an AI coding agent (Claude);
> I reviewed and edited them.

### Draft PR description

> We use quic-go in Nu, a p2p file-sharing daemon that routes several QUIC connections over
> one shared UDP socket. Fixes #<issue>.
>
> This adds `Config.ConnectionIDTracer`. Its callbacks are called when the connection
> adds or removes a connection ID in the Transport's routing table: handshake IDs,
> IDs issued in NEW_CONNECTION_ID (before the frame is sent), retired IDs after the
> 3-PTO delay, and all remaining IDs on close. Unlike a qlog tracer, it adds no
> per-packet work; with it, our wrapper went from ~22% to ~3% below an uninstrumented
> transfer.
>
> Tests: unit tests for the generator and config cloning; an integration test that forces
> CID rotation with a 50 MB transfer and checks every received packet's DCID was reported
> as added, and every ID is removed after close.
>
> AI disclosure: written with an AI coding agent (Claude), reviewed by me line by line.
