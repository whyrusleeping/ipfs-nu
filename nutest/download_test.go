package nutest_test

import (
	"context"
	"crypto/tls"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/nutest/slow"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/store"
)

func downloadSize(b *testing.B) int64 {
	b.Helper()
	slow.Skip(b)
	size := int64(2 << 30)
	if s := os.Getenv("NU_DOWNLOAD_BYTES"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1<<20 {
			b.Fatal("NU_DOWNLOAD_BYTES must be at least 1 MiB")
		}
		size = n
	}
	b.SetBytes(size)
	return size
}

func downloadStore(b *testing.B) *store.Store {
	b.Helper()
	s, err := store.Open(b.TempDir(), store.Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

func downloadHost(b *testing.B) *host.Host {
	b.Helper()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		b.Fatal(err)
	}
	h, err := host.New(host.Config{Key: key})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { h.Close() })
	return h
}

// bytePacer is shared by ALL response streams of one provider. It reserves
// bandwidth before writing, with no accumulating idle credit. The maximum
// write is 64 KiB, keeping cancellation latency and burst size bounded.
// Different providers have independent pacers (not one client-side limiter).
type bytePacer struct {
	mu             sync.Mutex
	next           time.Time
	bytesPerSecond int64
}

func (p *bytePacer) wait(n int) {
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	p.next = p.next.Add(time.Duration(int64(n) * int64(time.Second) / p.bytesPerSecond))
	wait := time.Until(p.next)
	p.mu.Unlock()
	time.Sleep(wait)
}

type pacedResponse struct {
	proto.ServerStream
	pace    *bytePacer
	corrupt bool
	off     int
}

func (s *pacedResponse) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		buf := p[:min(len(p), 64<<10)]
		s.pace.wait(len(buf))
		if at := 4096 - s.off; s.corrupt && at >= 0 && at < len(buf) {
			buf = append([]byte(nil), buf...)
			buf[at] ^= 1
		}
		n, err := s.ServerStream.Write(buf)
		s.off += n
		total += n
		if err != nil {
			return total, err
		}
		if n != len(buf) {
			return total, io.ErrShortWrite
		}
		p = p[n:]
	}
	return total, nil
}

// BenchmarkDownload includes have, scheduling, Bao verification, partial writes,
// periodic durable checkpoints and commit. Setup and the independent final digest
// are untimed. Run with -benchtime=1x (each iteration downloads a fresh blob).
func BenchmarkDownload(b *testing.B) {
	for _, scenario := range []string{"single", "capped-three-one-corrupt"} {
		b.Run(scenario, func(b *testing.B) {
			size := downloadSize(b)
			b.StopTimer()
			src := downloadStore(b)
			source := filepath.Join(b.TempDir(), "source")
			f, err := os.Create(source)
			if err != nil {
				b.Fatal(err)
			}
			// Sparse deterministic input; QUIC does not compress. No size-proportional heap.
			if err := f.Truncate(size); err != nil {
				b.Fatal(err)
			}
			f.Close()
			id, err := src.AddNoCopy(source)
			if err != nil {
				b.Fatal(err)
			}
			client := downloadHost(b)
			var peers []fetch.Peer
			count := 1
			const capBytes = 40_000_000
			if scenario != "single" {
				count = 3
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			var bad wire.PeerID
			for i := 0; i < count; i++ {
				h := downloadHost(b)
				server := proto.NewServer(src, proto.ServerConfig{})
				if count == 1 {
					if err := server.Register(h); err != nil {
						b.Fatal(err)
					}
				} else {
					pace := &bytePacer{bytesPerSecond: capBytes}
					corrupt := i == 2
					if corrupt {
						bad = h.ID()
					}
					for _, protocol := range []string{proto.BytesID, proto.HaveID} {
						if err := h.Handle(protocol, server.HandlerLimit(), func(s *host.Stream) {
							if protocol == proto.BytesID {
								server.Serve(protocol, s.Conn().PeerID(), &pacedResponse{ServerStream: s, pace: pace, corrupt: corrupt})
							} else {
								server.Serve(protocol, s.Conn().PeerID(), s)
							}
						}); err != nil {
							b.Fatal(err)
						}
					}
				}
				addr, err := wire.ParseMultiaddr(h.Addrs()[0].String()+"/p2p/"+h.ID().String(), 1024)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := client.Dial(ctx, addr); err != nil {
					b.Fatal(err)
				}
				peers = append(peers, fetch.HostPeer(client, h.ID()))
			}
			for i := 0; i < b.N; i++ {
				dst := downloadStore(b)
				cfg := fetch.Config{Store: dst, Providers: fetch.Static(peers)}
				if value := os.Getenv("NU_DOWNLOAD_FLUSH_BYTES"); value != "" {
					cfg.FlushBytes, err = strconv.ParseUint(value, 10, 64)
					if err != nil || cfg.FlushBytes == 0 {
						b.Fatal("NU_DOWNLOAD_FLUSH_BYTES must be positive")
					}
				}
				fetcher := fetch.New(cfg)
				b.StartTimer()
				start := time.Now()
				result, err := fetcher.Fetch(ctx, id, fetch.Options{Size: uint64(size)})
				elapsed := time.Since(start)
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				if count == 3 {
					if len(result.Dropped) != 1 || result.Dropped[0] != bad {
						b.Fatalf("corrupt peer not rejected: %+v", result)
					}
					ratio := float64(size) / elapsed.Seconds() / (2 * capBytes)
					b.ReportMetric(ratio, "good-cap-ratio")
					// Report rather than fail on noisy/shared CI machines.
					b.Logf("good providers 2 x %d B/s; achieved %.3f of sum (target >=0.8)", capBytes, ratio)
				}
				bl, err := dst.OpenBlob(id.Multihash())
				if err != nil {
					b.Fatal(err)
				}
				hash := cid.BLAKE3.New()
				n, err := io.Copy(hash, bl.Reader())
				bl.Close()
				if err != nil || n != size || cid.FromHasher(cid.BLAKE3, hash) != id.Multihash() {
					b.Fatal("final digest/size mismatch", err)
				}
				dst.Close()
			}
		})
	}
}

// BenchmarkPlainQUIC is a single quic-go stream, with neither Nu host/streams,
// Bao, nor disk. HostWindows controls only the receive windows; DefaultWindows
// measures upstream defaults, avoiding a reference ceiling artificially limited
// by Nu's settings. Both endpoints are local, using ordinary QUIC TLS.
func BenchmarkPlainQUIC(b *testing.B) {
	for _, windows := range []string{"HostWindows", "DefaultWindows"} {
		b.Run(windows, func(b *testing.B) {
			size := downloadSize(b)
			b.StopTimer()
			key, err := wire.GenerateNodeKey()
			if err != nil {
				b.Fatal(err)
			}
			cert, err := wire.NewTLSCertificate(key, time.Now())
			if err != nil {
				b.Fatal(err)
			}
			cfg := &quic.Config{}
			if windows == "HostWindows" {
				cfg.InitialStreamReceiveWindow = 64 << 10
				cfg.MaxStreamReceiveWindow = 1 << 20
				cfg.InitialConnectionReceiveWindow = 256 << 10
				cfg.MaxConnectionReceiveWindow = 4 << 20
			}
			listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"nu-perf"}}, cfg)
			if err != nil {
				b.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			sent := make(chan error, 1)
			go func() {
				conn, err := listener.Accept(ctx)
				if err != nil {
					sent <- err
					return
				}
				for i := 0; i < b.N; i++ {
					s, err := conn.AcceptStream(ctx)
					if err != nil {
						sent <- err
						return
					}
					var trigger [1]byte
					if _, err = io.ReadFull(s, trigger[:]); err == nil {
						_, err = io.CopyN(s, zeroReader{}, size)
					}
					if err == nil {
						err = s.Close()
					}
					if err != nil {
						sent <- err
						return
					}
				}
				sent <- nil
			}()
			conn, err := quic.DialAddr(ctx, listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"nu-perf"}}, cfg) // local benchmark certificate only
			if err != nil {
				b.Fatal(err)
			}
			defer conn.CloseWithError(0, "")
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				s, err := conn.OpenStreamSync(ctx)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := s.Write([]byte{1}); err != nil {
					b.Fatal(err)
				}
				s.Close()
				n, err := io.Copy(io.Discard, s)
				if err != nil || n != size {
					b.Fatal("plain stream", n, err)
				}
			}
			b.StopTimer()
			if err := <-sent; err != nil {
				b.Fatal(err)
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
