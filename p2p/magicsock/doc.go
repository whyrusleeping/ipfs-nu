// Package magicsock multiplexes physical QUIC, virtual-peer QUIC and bounded
// disco envelopes on one UDP PacketConn. Explicit path sets select reliable
// qpath relay or direct UDP without replacing the inner QUIC connection.
// Authentication is supplied by Host's libp2p TLS/socket-policy hooks; this
// package neither authorizes probes nor manages public sessions/home relays.
package magicsock
