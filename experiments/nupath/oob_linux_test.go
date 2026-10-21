//go:build linux

package nupath

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"golang.org/x/net/ipv4"
)

// An apparently transparent OOB wrapper is NOT transparent to quic-go: without
// ReadBatch, x/net unwraps its fd and bypasses virtual address/relay injection.
type oobWrapper struct {
	*net.UDPConn
	reads  atomic.Int64
	writes atomic.Int64
}

func (p *oobWrapper) ReadMsgUDP(b, oob []byte) (int, int, int, *net.UDPAddr, error) {
	p.reads.Add(1)
	return p.UDPConn.ReadMsgUDP(b, oob)
}
func (p *oobWrapper) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	p.writes.Add(1)
	return p.UDPConn.WriteMsgUDP(b, oob, addr)
}

type batchedWrapper struct{ *oobWrapper }

func (p *batchedWrapper) ReadBatch(ms []ipv4.Message, _ int) (int, error) {
	m := &ms[0]
	n, oobn, flags, addr, err := p.ReadMsgUDP(m.Buffers[0], m.OOB)
	if err != nil {
		return 0, err
	}
	m.N = n
	m.NN = oobn
	m.Flags = flags
	m.Addr = addr
	return 1, nil
}
func TestOOBBatchHook(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "fd-bypass"
		if batch {
			name = "explicit-ReadBatch"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			check(t, err)
			wrapped := &oobWrapper{UDPConn: udp}
			var pc net.PacketConn = wrapped
			if batch {
				pc = &batchedWrapper{wrapped}
			}
			transport := &quic.Transport{Conn: pc}
			defer transport.Close()
			defer udp.Close()
			listener, err := transport.Listen(tlsConfig(t), &quic.Config{})
			check(t, err)
			client, err := quic.DialAddr(ctx, udp.LocalAddr().String(), tlsConfig(t), &quic.Config{})
			check(t, err)
			defer client.CloseWithError(0, "")
			server, err := listener.Accept(ctx)
			check(t, err)
			defer server.CloseWithError(0, "")
			if (wrapped.reads.Load() > 0) != batch {
				t.Fatal("unexpected OOB read dispatch", wrapped.reads.Load())
			}
			if wrapped.writes.Load() == 0 {
				t.Fatal("did not exercise OOB writes")
			}
			t.Logf("ReadMsgUDP hook calls=%d WriteMsgUDP calls=%d", wrapped.reads.Load(), wrapped.writes.Load())
		})
	}
}

func TestRelayBackpressure(t *testing.T) {
	a, err := newPacketConn("127.0.0.1:0")
	check(t, err)
	defer a.Close()
	writer, blocked := net.Pipe()
	defer blocked.Close()
	a.attach(writer)
	start := time.Now()
	packet := make([]byte, 1200)
	packet[0] = 0x40
	for range 1024 {
		_, err = a.WriteTo(packet, virtual)
		check(t, err)
	}
	if a.dropped.Load() < 900 || a.maxQueue.Load() > 64 {
		t.Fatal("unbounded relay backlog")
	}
	t.Logf("blocked reliable stream: 1024 submissions=%s high_water=%d/64 drops=%d; close unblocks writer", time.Since(start), a.maxQueue.Load(), a.dropped.Load())
}
