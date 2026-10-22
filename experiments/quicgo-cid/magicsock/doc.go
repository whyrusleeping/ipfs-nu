// Package magicsock is an experiments-only snapshot of nu/p2p/magicsock at
// 241e2ce, adapted to track inner connection IDs with the proposed
// quic.Config.ConnectionIDTracer (see ../README.md) instead of a qlog
// tracer. Only quic.go and the cidRoute owner type differ from the original.
// Do not import it from the main module; the main module stays on stock quic-go.
//
// Package magicsock multiplexes physical QUIC, virtual-peer QUIC and bounded
// disco envelopes on one UDP PacketConn. Explicit path sets select reliable
// qpath relay or direct UDP without replacing the inner QUIC connection.
// Authentication is supplied by Host's libp2p TLS/socket-policy hooks; this
// package neither authorizes probes nor manages public sessions/home relays.
package magicsock
