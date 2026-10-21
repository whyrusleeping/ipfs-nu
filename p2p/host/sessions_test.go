package host_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func sessionHost(t *testing.T, k *wire.NodeKey, listen string) (*host.Host, *wire.NodeKey) {
	t.Helper()
	var err error
	if k == nil {
		k, err = wire.GenerateNodeKey()
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := host.Config{Key: k}
	if listen != "" {
		cfg.Listen = []string{listen}
	}
	h, err := host.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, k
}

const resumeProtocol = "/test/resume/1"

func resumeHandler(t *testing.T, h *host.Host, payload []byte) {
	t.Helper()
	if err := h.Handle(resumeProtocol, 4, func(s *host.Stream) {
		_ = s.SetDeadline(time.Now().Add(time.Second))
		request, err := wire.ReadMessage(s, 8)
		if err != nil || len(request) != 8 {
			s.Reset()
			return
		}
		offset := binary.BigEndian.Uint64(request)
		if offset >= uint64(len(payload)) {
			s.Reset()
			return
		}
		end := min(len(payload), int(offset)+64<<10)
		if err := wire.WriteMessage(s, payload[int(offset):end], 64<<10); err != nil {
			s.Reset()
		}
	}); err != nil {
		t.Fatal(err)
	}
}
func TestSessionsVerifiedResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a, _ := sessionHost(t, nil, "")
	b, key := sessionHost(t, nil, "")
	payload := bytes.Repeat([]byte("request-resume"), 10000)[:128<<10]
	resumeHandler(t, b, payload)
	cache, err := lookup.NewCache(ctx, lookup.CacheConfig{})
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := lookup.New(ctx, lookup.Config{Cache: cache, DisableMDNS: true})
	if err != nil {
		t.Fatal(err)
	}
	defer discovery.Close()
	// A dead first hint must not delay the working address until its dial timeout.
	dead, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	deadAddress, err := host.UDPAddress(dead.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	_ = dead.Close()
	link := "nu://bafk-test?peer=" + b.ID().Base36() + "&addr=" + url.QueryEscape(deadAddress.String()) + "&addr=" + url.QueryEscape(b.Addrs()[0].String())
	hints, err := lookup.ParseHints(link, lookup.DefaultHintLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := discovery.RememberHints(ctx, hints); err != nil {
		t.Fatal(err)
	}
	sessions, err := host.NewSessions(a, host.SessionConfig{Lookup: discovery, Cache: cache, Stagger: time.Millisecond, DialTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	type result struct {
		session *host.Session
		err     error
	}
	connected := make(chan result, 4)
	for range 4 {
		go func() { session, err := sessions.Connect(ctx, b.ID()); connected <- result{session, err} }()
	}
	var session *host.Session
	for range 4 {
		select {
		case r := <-connected:
			if r.err != nil {
				t.Fatal(r.err)
			}
			if session != nil && session != r.session {
				t.Fatal("concurrent calls created different sessions")
			}
			session = r.session
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	same, err := sessions.Get(b.ID())
	if err != nil || same != session {
		t.Fatal("not one session per peer", err)
	}
	firstConn, ok := a.Connection(b.ID())
	if !ok {
		t.Fatal("missing connection")
	}
	fetch := func(offset int) []byte {
		s, err := session.OpenStream(ctx, resumeProtocol)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		_ = s.SetDeadline(time.Now().Add(time.Second))
		var request [8]byte
		binary.BigEndian.PutUint64(request[:], uint64(offset))
		if err := wire.WriteMessage(s, request[:], 8); err != nil {
			t.Fatal(err)
		}
		data, err := wire.ReadMessage(s, 64<<10)
		if err != nil {
			t.Fatal(err)
		}
		want := payload[offset:min(len(payload), offset+64<<10)]
		if sha256.Sum256(data) != sha256.Sum256(want) {
			t.Fatal("chunk did not verify")
		}
		return data
	}
	first := fetch(0)
	if reused, _ := a.Connection(b.ID()); reused != firstConn {
		t.Fatal("stream did not reuse connection")
	}
	// Kill/restart the serving node with its same key/address. The caller keeps
	// its verified prefix, not a stream, and sends a new request at that offset.
	bind := firstConn.RemoteAddr().String()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstConn.Context().Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	restarted, _ := sessionHost(t, key, bind)
	resumeHandler(t, restarted, payload)
	rest := fetch(len(first))
	assembled := append(first, rest...)
	if sha256.Sum256(assembled) != sha256.Sum256(payload) {
		t.Fatal("resumed data differs")
	}
	if newConn, _ := a.Connection(b.ID()); newConn == firstConn {
		t.Fatal("did not reconnect")
	}
}
func TestSessionFailuresAndBounds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a, _ := sessionHost(t, nil, "")
	b, _ := sessionHost(t, nil, "")
	wrongKey, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	wrong := wrongKey.Public().PeerID()
	sessions, err := host.NewSessions(a, host.SessionConfig{MaxSessions: 2, DialTimeout: 60 * time.Millisecond, ConnectTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	if _, err := sessions.Connect(ctx, b.ID()); !errors.Is(err, host.ErrNoAddresses) {
		t.Fatal("no address error", err)
	}
	if err := sessions.SetPeer(wrong, b.Addrs()); err != nil {
		t.Fatal(err)
	}
	_, err = sessions.Connect(ctx, wrong)
	var mismatch *wire.PeerIDMismatchError
	var dials *host.DialError
	if !errors.As(err, &mismatch) || !errors.Is(err, wire.ErrPeerIDMismatch) || !errors.As(err, &dials) || len(dials.Failures) != 1 || mismatch.Actual != b.ID() {
		t.Fatal("identity mismatch details", err)
	}
	dead, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := host.UDPAddress(dead.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	_ = dead.Close()
	if err := sessions.SetPeer(wrong, []wire.Multiaddr{addr}); err != nil {
		t.Fatal(err)
	}
	_, err = sessions.Connect(ctx, wrong)
	dials = nil
	if !errors.As(err, &dials) || len(dials.Failures) != 1 || dials.Failures[0].Err == nil {
		t.Fatal("per-address dial failure", err)
	}
	if _, err := sessions.Get(a.ID()); !errors.Is(err, host.ErrLimit) {
		t.Fatal("session resource bound", err)
	}
	sessions.Forget(b.ID(), true)
	if _, err := sessions.Get(a.ID()); err != nil {
		t.Fatal("session slot not released", err)
	}
}

type lookupSpy struct{ calls atomic.Int32 }

func (s *lookupSpy) Lookup(context.Context, wire.PeerID) <-chan lookup.Result {
	s.calls.Add(1)
	ch := make(chan lookup.Result)
	close(ch)
	return ch
}
func TestPrivateSessionsAndPartitionGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a, _ := sessionHost(t, nil, "")
	b, _ := sessionHost(t, nil, "")
	outsider, _ := sessionHost(t, nil, "")
	spy := new(lookupSpy)
	private, err := host.NewSessions(b, host.SessionConfig{Private: true, Lookup: spy, Peers: map[wire.PeerID][]wire.Multiaddr{a.ID(): a.Addrs()}})
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	if _, err := private.Connect(ctx, outsider.ID()); !errors.Is(err, host.ErrPeerDenied) {
		t.Fatal("unauthorized peer", err)
	}
	if err := private.AddHints(ctx, []lookup.Hint{{Peer: outsider.ID(), Addresses: outsider.Addrs()}}); !errors.Is(err, host.ErrPeerDenied) {
		t.Fatal("hints granted membership", err)
	}
	session, err := private.Connect(ctx, a.ID())
	if err != nil {
		t.Fatal(err)
	}
	if spy.calls.Load() != 0 {
		t.Fatal("private mode consulted discovery")
	}
	conn, _ := b.Connection(a.ID())
	b.SetConnectionGater(func(id wire.PeerID) bool { return id != a.ID() })
	select {
	case <-conn.Context().Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := session.Connect(ctx); !errors.Is(err, host.ErrPeerDenied) {
		t.Fatal("partition did not gate dial", err)
	}
	b.SetConnectionGater(nil)
	if _, err := session.Connect(ctx); err != nil {
		t.Fatal("partition recovery", err)
	}
	// Servers reject unauthorized identity before routing any application stream.
	c, err := outsider.Dial(ctx, address(t, b, b.ID()))
	if err == nil {
		s, e := c.OpenStream(ctx)
		if e == nil {
			defer s.Reset()
			_ = s.SetDeadline(time.Now().Add(200 * time.Millisecond))
			_, _ = s.Write([]byte("unauthorized"))
			var response [1]byte
			if _, e = io.ReadFull(s, response[:]); e == nil {
				t.Fatal("unauthorized traffic accepted")
			}
		}
		select {
		case <-c.Context().Done():
		case <-ctx.Done():
			t.Fatal("unauthorized connection remained open")
		}
	}
	if _, ok := b.Connection(outsider.ID()); ok {
		t.Fatal("unauthorized inbound admitted")
	}
	if err := private.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing private sessions must not restore permissive host policy.
	if _, err := b.Dial(ctx, address(t, a, a.ID())); !errors.Is(err, host.ErrPeerDenied) {
		t.Fatal("private close made host public", err)
	}
}
func TestAddressRaceOrder(t *testing.T) {
	for _, v := range []struct {
		text string
		rank int
	}{{"/ip4/192.168.1.2/udp/4171/quic-v1", 0}, {"/ip6/2001:4860::1/udp/4171/quic-v1", 1}, {"/ip4/8.8.8.8/udp/4171/quic-v1", 2}} {
		a, err := wire.ParseMultiaddr(v.text, 1024)
		if err != nil || host.AddressRank(a) != v.rank {
			t.Fatal("address ordering", v.text, err)
		}
	}
}
