package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/whyrusleeping/nu/node/daemon"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/wire"
	dataproto "github.com/whyrusleeping/nu/proto"
)

// daemonCore uses the real daemon assembly: API, secured local gateway, store,
// serving/fetching, sessions, mDNS, magicsock, disco, AutoNAT and netmon. No
// NoNetwork/NoMDNS/private-mode switches or replacement protocol handlers.
// Operator-only services (public HTTPS, cluster membership, relay fleet) still
// require explicit configuration; "daemon" does not imply invented defaults.
type daemonCore struct {
	d   *daemon.Daemon
	dir string
}

func daemonHost(ip string) (*host.Host, coreService, error) {
	dir, err := os.MkdirTemp("", "nu-interop-daemon-")
	if err != nil {
		return nil, nil, err
	}
	r, _, err := repo.Open(dir)
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	family := "ip4"
	if net.ParseIP(ip).To4() == nil {
		family = "ip6"
	}
	d, err := daemon.Start(context.Background(), daemon.Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0", SwarmListen: []string{fmt.Sprintf("/%s/%s/udp/0/quic-v1", family, ip)}})
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	h := d.Swarm.Host()
	for _, want := range []string{proto.IdentifyID, proto.PingID, dataproto.BytesID, dataproto.HaveID} {
		found := false
		for _, p := range h.Protocols() {
			found = found || p == want
		}
		if !found {
			d.Close()
			os.RemoveAll(dir)
			return nil, nil, fmt.Errorf("daemon missing protocol %s: %v", want, h.Protocols())
		}
	}
	fmt.Fprintln(os.Stderr, "full daemon enabled; protocols:", h.Protocols())
	return h, &daemonCore{d, dir}, nil
}
func (s *daemonCore) Close() error { return errors.Join(s.d.Close(), os.RemoveAll(s.dir)) }
func (s *daemonCore) Ping(ctx context.Context, id wire.PeerID) (time.Duration, error) {
	return s.d.Swarm.Ping(ctx, id)
}

// The daemon owns its core service already. Read identify as a client instead of
// registering a second service (which would mask daemon protocol conflicts).
func (s *daemonCore) Identify(ctx context.Context, id wire.PeerID) (proto.PeerInfo, error) {
	stream, err := s.d.Swarm.Host().OpenStream(ctx, id, proto.IdentifyID)
	if err != nil {
		return proto.PeerInfo{}, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, stream.Reset)
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	m, err := proto.ReadIdentify(stream, proto.DefaultIdentifyLimits())
	if err != nil {
		return proto.PeerInfo{}, err
	}
	if len(m.PublicKey) > 0 {
		key, err := wire.ParsePublicKey(m.PublicKey, wire.MaxKeyBytes)
		if err != nil {
			return proto.PeerInfo{}, err
		}
		if key.PeerID() != id {
			return proto.PeerInfo{}, wire.ErrSignature
		}
	}
	return proto.PeerInfo{Message: m}, nil
}
