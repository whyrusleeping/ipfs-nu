// Command mdns-peer is a manual two-machine LAN discovery/session probe.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	coreproto "github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:0", "literal UDP listen address")
	keyFile := flag.String("key", "", "0600 node-key file (create if absent)")
	peer := flag.String("peer", "", "optional remote PeerId to discover and ping")
	cluster := flag.String("cluster", "", "optional cluster discovery namespace")
	duration := flag.Duration("duration", time.Minute, "maximum run duration")
	flag.Parse()
	if err := run(*listen, *keyFile, *peer, *cluster, *duration); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(listen, keyFile, peer, cluster string, duration time.Duration) error {
	var key *wire.NodeKey
	var err error
	if keyFile == "" {
		key, err = wire.GenerateNodeKey()
	} else {
		key, err = wire.LoadOrCreateNodeKey(keyFile)
	}
	if err != nil {
		return err
	}
	h, err := host.New(host.Config{Key: key, Listen: []string{listen}})
	if err != nil {
		return err
	}
	defer h.Close()
	fmt.Println("peer:", h.ID(), "listen:", h.Addrs())
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signals, duration)
	defer cancel()
	core, err := coreproto.New(h, coreproto.Config{})
	if err != nil {
		return err
	}
	defer core.Close()
	discovery, err := lookup.New(ctx, lookup.Config{MDNS: lookup.MDNSConfig{Peer: h.ID(), Addresses: h.Addrs, ClusterID: cluster}})
	if err != nil {
		return err
	}
	defer discovery.Close()
	sessions, err := host.NewSessions(h, host.SessionConfig{Lookup: discovery, Cache: discovery.Cache()})
	if err != nil {
		return err
	}
	defer sessions.Close()
	if peer != "" {
		remote, err := wire.ParsePeerID(peer)
		if err != nil {
			return err
		}
		if _, err := sessions.Connect(ctx, remote); err != nil {
			return err
		}
		rtt, err := core.Ping(ctx, remote)
		if err != nil {
			return err
		}
		fmt.Println("mDNS discovered and authenticated:", remote, "ping:", rtt)
	}
	<-ctx.Done()
	return nil
}
