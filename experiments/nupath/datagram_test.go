package nupath

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

func TestDatagramBudget(t *testing.T) {
	for _, size := range []uint16{1200, 1400} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			cfg := &quic.Config{EnableDatagrams: true, InitialPacketSize: size, DisablePathMTUDiscovery: true}
			listener, err := quic.ListenAddr("127.0.0.1:0", tlsConfig(t), cfg)
			check(t, err)
			defer listener.Close()
			client, err := quic.DialAddr(ctx, listener.Addr().String(), tlsConfig(t), cfg)
			check(t, err)
			defer client.CloseWithError(0, "")
			server, err := listener.Accept(ctx)
			check(t, err)
			defer server.CloseWithError(0, "")
			payload := make([]byte, 1200)
			err = client.SendDatagram(payload)
			if size == 1200 {
				var tooLarge *quic.DatagramTooLargeError
				if !errors.As(err, &tooLarge) {
					t.Fatal("expected DATAGRAM budget error", err)
				}
				t.Logf("outer packet budget=%d: inner1200 rejected, max DATAGRAM payload=%d", size, tooLarge.MaxDatagramPayloadSize)
			} else {
				check(t, err)
				got, err := server.ReceiveDatagram(ctx)
				check(t, err)
				if len(got) != 1200 {
					t.Fatal(len(got))
				}
				t.Logf("outer packet budget=%d: inner1200 delivered", size)
			}
		})
	}
}
