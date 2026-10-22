package homerelay_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/homerelay"
	"github.com/whyrusleeping/nu/p2p/host"
	nuproto "github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/relay"
)

func TestHomeBackupRenewalPromotionAndReachability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(host.Config{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	identify, err := nuproto.New(h, nuproto.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer identify.Close()
	client, err := circuit.NewClient(h, circuit.ClientConfig{MaxReservations: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var relays []*relay.Server
	var addrs []wire.Multiaddr
	ranks := map[wire.PeerID]time.Duration{}
	for i := range 3 {
		k, err := wire.GenerateNodeKey()
		if err != nil {
			t.Fatal(err)
		}
		r, err := relay.New(relay.Config{Host: host.Config{Key: k, Listen: []string{"127.0.0.1:0"}}, Server: circuit.ServerConfig{ReservationTTL: 2 * time.Second}})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		relays = append(relays, r)
		addr, err := wire.ParseMultiaddr(r.Host.Addrs()[0].String()+"/p2p/"+r.Host.ID().String(), 1024)
		if err != nil {
			t.Fatal(err)
		}
		addrs = append(addrs, addr)
		ranks[r.Host.ID()] = time.Duration(i+1) * time.Millisecond
	}
	var changes atomic.Int32
	m, err := homerelay.New(ctx, homerelay.Config{
		Host: h, Client: client, Relays: []wire.Multiaddr{addrs[2], addrs[0], addrs[1]}, Timeout: 200 * time.Millisecond, RetryInterval: time.Second,
		Changed: func() { changes.Add(1) },
		// Real ping I/O with controlled measured rankings keeps this test independent
		// of scheduling jitter while testing latency-based selection, not list order.
		Ping: func(ctx context.Context, id wire.PeerID) (time.Duration, error) {
			_, err := identify.Ping(ctx, id)
			return ranks[id], err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s, err := m.Wait(ctx, func(s homerelay.State) bool { return s.Home != "" && s.Backup != "" })
	if err != nil {
		t.Fatal(err)
	}
	if s.Home != relays[0].Host.ID() || s.Backup != relays[1].Host.ID() {
		t.Fatal("not selected by RTT", s)
	}
	if len(s.Addresses) != 2 {
		t.Fatal("missing live relay addresses", s)
	}
	for _, addr := range s.Addresses {
		_, _, peer, err := circuit.ParseAddress(addr)
		if err != nil || peer != h.ID() {
			t.Fatal("invalid advertised circuit", addr, err)
		}
	}
	// Half-TTL renewals must run without an explicit owner request.
	initial := s.Relays[1].LastSuccess
	_, err = m.Wait(ctx, func(s homerelay.State) bool { return s.Relays[1].LastSuccess.After(initial) })
	if err != nil {
		t.Fatal("no early renewal", err)
	}
	if err := m.Renew(ctx); err != nil {
		t.Fatal("D12 renewal", err)
	}
	if err := relays[0].Close(); err != nil {
		t.Fatal(err)
	}
	s, err = m.Wait(ctx, func(s homerelay.State) bool { return s.Home == relays[1].Host.ID() && s.Backup == relays[2].Host.ID() })
	if err != nil {
		t.Fatal("backup was not promoted/replaced", err)
	}
	if len(s.Addresses) != 2 || changes.Load() < 3 {
		t.Fatal("failover was not published", s, changes.Load())
	}
	m.SetNeeded(false) // confirmed public reachability: no reservations/defaults
	s, err = m.Wait(ctx, func(s homerelay.State) bool { return !s.Needed && s.Home == "" && s.Backup == "" })
	if err != nil || len(s.Addresses) != 0 {
		t.Fatal("public reachability retained advertisements", s, err)
	}
	m.SetNeeded(true)
	_, err = m.Wait(ctx, func(s homerelay.State) bool { return s.Needed && len(s.Addresses) == 2 })
	if err != nil {
		t.Fatal("private reachability did not restore reservations", err)
	}
}

func TestDisabledManagerNeverUsesNetwork(t *testing.T) {
	var calls atomic.Int32
	m, err := homerelay.New(context.Background(), homerelay.Config{Disabled: true,
		Ping: func(context.Context, wire.PeerID) (time.Duration, error) { calls.Add(1); return 0, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetNeeded(true)
	if err := m.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := m.Snapshot()
	if !s.Disabled || s.Needed || len(s.Addresses) > 0 || calls.Load() != 0 {
		t.Fatal("private/isolated relay activity", s)
	}
}
