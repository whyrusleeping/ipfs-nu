package nupath

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
	"github.com/whyrusleeping/nu/nutest/slow"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func benchTLS(b *testing.B) (*tls.Config, wire.PeerID) {
	b.Helper()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		b.Fatal(err)
	}
	cert, err := wire.NewTLSCertificate(key, time.Now())
	if err != nil {
		b.Fatal(err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{wire.TLSALPN}, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert, InsecureSkipVerify: true, VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
		_, err := wire.VerifyTLSCertificate(raw, "", time.Now(), 16<<10)
		return err
	}}, key.Public().PeerID()
}

// Opt-in because this is a throughput measurement, not a correctness gate.
// Raw / RawObserver / VirtualDirect use identical fixed 1200-byte budgets and
// flow windows. RawPMTU separately measures the stock packet-policy advantage.
// Compile/setup is outside the timer; report observer cost, not just wrapping.
func BenchmarkProductionPaths(b *testing.B) {
	slow.Skip(b)
	for _, name := range []string{"Raw", "RawObserver", "VirtualDirect", "RawPMTU"} {
		virtual := name == "VirtualDirect"
		b.Run(name, func(b *testing.B) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ta, aid := benchTLS(b)
			tb, bid := benchTLS(b)
			ua, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				b.Fatal(err)
			}
			defer ua.Close()
			ub, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				b.Fatal(err)
			}
			defer ub.Close()
			var ca, cb net.PacketConn = ua, ub
			var ma, mb *magicsock.Conn
			remote := ub.LocalAddr()
			acfg := &quic.Config{InitialPacketSize: 1200, DisablePathMTUDiscovery: true, MaxStreamReceiveWindow: 1 << 20, MaxConnectionReceiveWindow: 4 << 20}
			if name == "RawPMTU" {
				acfg.InitialPacketSize = 0
				acfg.DisablePathMTUDiscovery = false
			}
			bcfg := acfg.Clone()
			if name == "RawObserver" {
				acfg.Tracer = func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace { return discardQUICTrace{} }
				bcfg.Tracer = acfg.Tracer
			}
			if virtual {
				ma, err = magicsock.New(magicsock.Config{UDP: ua})
				if err != nil {
					b.Fatal(err)
				}
				defer ma.Close()
				mb, err = magicsock.New(magicsock.Config{UDP: ub})
				if err != nil {
					b.Fatal(err)
				}
				defer mb.Close()
				aaddr, err := ma.AddPeer(bid)
				if err != nil {
					b.Fatal(err)
				}
				baddr, err := mb.AddPeer(aid)
				if err != nil {
					b.Fatal(err)
				}
				remote = net.UDPAddrFromAddrPort(aaddr)
				ca, cb = ma, mb
				acfg = ma.QUICConfigForPeer(remote, acfg)
				bcfg = mb.QUICConfigForPeer(net.UDPAddrFromAddrPort(baddr), bcfg)
				ra, rb := net.Pipe()
				if _, err = ma.AttachRelay(bid, ra); err != nil {
					b.Fatal(err)
				}
				if _, err = mb.AttachRelay(aid, rb); err != nil {
					b.Fatal(err)
				}
			}
			at := &quic.Transport{Conn: ca, ConnectionIDLength: 8}
			bt := &quic.Transport{Conn: cb, ConnectionIDLength: 8}
			defer at.Close()
			defer bt.Close()
			listener, err := bt.Listen(tb, bcfg)
			if err != nil {
				b.Fatal(err)
			}
			client, err := at.Dial(ctx, remote, ta, acfg)
			if err != nil {
				b.Fatal(err)
			}
			defer client.CloseWithError(0, "")
			server, err := listener.Accept(ctx)
			if err != nil {
				b.Fatal(err)
			}
			defer server.CloseWithError(0, "")
			if virtual {
				if err = ma.SetDirectPaths(bid, []netip.AddrPort{ub.LocalAddr().(*net.UDPAddr).AddrPort()}); err != nil {
					b.Fatal(err)
				}
				if err = mb.SetDirectPaths(aid, []netip.AddrPort{ua.LocalAddr().(*net.UDPAddr).AddrPort()}); err != nil {
					b.Fatal(err)
				}
				if err = ma.UseDirect(bid, ub.LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
					b.Fatal(err)
				}
				if err = mb.UseDirect(aid, ua.LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
					b.Fatal(err)
				}
			}
			const size = 4 << 20
			payload := make([]byte, size)
			done := make(chan error, 1)
			go func() {
				for {
					s, err := server.AcceptUniStream(ctx)
					if err != nil {
						return
					}
					n, copyErr := io.Copy(io.Discard, s)
					if copyErr == nil && n != size {
						copyErr = io.ErrUnexpectedEOF
					}
					done <- copyErr
				}
			}()
			b.SetBytes(size)
			b.ResetTimer()
			for range b.N {
				s, err := client.OpenUniStreamSync(ctx)
				if err != nil {
					b.Fatal(err)
				}
				if _, err = s.Write(payload); err != nil {
					b.Fatal(err)
				}
				if err = s.Close(); err != nil {
					b.Fatal(err)
				}
				if err = <-done; err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if client.ConnectionState().GSO && server.ConnectionState().GSO {
				b.ReportMetric(1, "GSO")
			}
			stats := client.ConnectionStats()
			b.ReportMetric(float64(stats.BytesSent)/float64(stats.PacketsSent), "tx-bytes/pkt")
			b.ReportMetric(float64(stats.PacketsLost+server.ConnectionStats().PacketsLost)/float64(b.N), "lost/op")
			if virtual {
				b.ReportMetric(float64(ma.Stats().Dropped+mb.Stats().Dropped)/float64(b.N), "adapter-drops/op")
			}
		})
	}
}

// Control: force stock quic-go's same public qlog event construction, but do no
// CID routing, socket wrapping, channel hand-off, or serialization. There is no
// public CID-only event filter in v0.64.1; the construction cost is in quic-go.
type discardQUICTrace struct{}

func (discardQUICTrace) SupportsSchemas(s string) bool      { return s == qlog.EventSchema }
func (d discardQUICTrace) AddProducer() qlogwriter.Recorder { return d }
func (discardQUICTrace) RecordEvent(qlogwriter.Event)       {}
func (discardQUICTrace) Close() error                       { return nil }
