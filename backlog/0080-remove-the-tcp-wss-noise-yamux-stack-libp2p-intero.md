---
id: "0080"
title: Remove the TCP/WSS + Noise + yamux stack; libp2p interop is QUIC only
status: done
priority: 2
created: "2026-10-22"
updated: "2026-10-22"
depends_on: []
spec_refs:
    - docs/networking.md#D8. Wire profile and compatibility
    - D18. Implementation layout
    - Milestones
---

## Description
User direction (2026-10-22): Nu wants *some* go-ipfs/libp2p interop **via QUIC**, not the libp2p TCP stack and its security/muxer protocols. Task 0041 built libp2p WebSocket-over-TLS → Noise XX → yamux (own implementations) as the UDP-blocked fallback to relays; that is out of scope and should be removed (~2.9k lines including tests and wiring).

Remove:
- Packages `p2p/noise`, `p2p/wss`, `p2p/yamux`.
- `p2p/host/wss.go` (`Host.DialWSS`/`AcceptWSS`) and WSS branches in `p2p/host/conn.go`; WSS address handling in `p2p/proto/circuit/address.go`.
- `relay/wss.go`, `relay/wss_test.go`, WSS listener wiring in `relay/server.go` and the `nu relay --wss-*` flags / `[relay] wss_*` config keys.
- `p2p/homerelay` WSS endpoints (endpoint sets become QUIC-only; drop the UDP→WSS fallback and its separate budget), `node/swarm/wss.go`, WSS bits of `node/swarm/repo_connectivity.go`, and the `swarm.proxy` (HTTP CONNECT) and `swarm.relay_cert_pins` config keys plus their status/doctor fields (`transport`, `udp_error`, relay-fallback line) unless still meaningful for QUIC.
- `experiments/noise_httptrack_test.go`, `experiments/wss_httptrack_test.go`.
- The natlab UDP-blocked WSS scenario (`TestDaemonWSSUDPBlocked`) and the WSS restart cases in the 0075 natlab evidence.
- Docs: the WSS/Noise/yamux sections of `docs/networking.md` (D8 "Production public reach", the endpoint/proxy support matrix, "WSS relay listener and wire implementation (0041)", "Standard non-Nu relayed secure connections await Noise/yamux"), `docs/validation.md` 0041 evidence and the 0041 item of the independent review request, the WSS/Noise/yamux dependency paragraph in spec.md D18, and the "HTTPS-compatible relay" wording in spec.md Goals/Local API/Milestones and in backlog 0049/0029 acceptance text. State plainly that UDP-blocked networks are unsupported (nu doctor says so) until a replacement is accepted.
- Backlog 0041: record in its work log that the WSS/Noise/yamux implementation was withdrawn by this task.

Keep: QUIC-v1 + libp2p TLS, circuit relay v2 over QUIC, `/nu/qpath` and magicsock, which are the QUIC interop and Nu relay path.

Replacement for UDP-blocked reach is NOT part of this task (see the separate proposed task); do not build one here.

Acceptance: the listed packages and config keys are gone, `go build ./...`, `go vet ./...` and `go test ./...` pass, natlab QUIC relay scenarios still pass, the go/rust/js/Kubo QUIC interop matrix still passes, and `grep -riI 'yamux\|noise\|wss\|websocket' --include=*.go .` finds nothing outside unrelated identifiers.

## Acceptance criteria

## Work log
- 2026-10-22: Done (b8f06eb; merged): removed p2p/noise, p2p/wss, p2p/yamux, host TCP adapters, relay WSS listener/flags/keys, swarm.proxy/relay_cert_pins + transport/fallback diagnostics, fallback endpoint logic, WSS experiments and natlab tests (−3,615/+240 lines). Docs/doctor state UDP-blocked networks are unsupported until a replacement is accepted. Evidence: build/vet/staticcheck/test/depguard green; natlab relay matrix, failover, pair-aware peer relay and one-sided restart cases pass (74 s); Docker interop go/rust/js 6/6 + Kubo pass. Only remaining grep hits are the unrelated Jetstream websocket client.
