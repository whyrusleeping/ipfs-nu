package proto_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	nuproto "github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func vector(t testing.TB) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors/identify.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Identify string }
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	b, err = hex.DecodeString(v.Identify)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestIdentifyVectorAndParts(t *testing.T) {
	raw := vector(t)
	m, err := nuproto.ParseIdentify(raw, nuproto.DefaultIdentifyLimits())
	if err != nil {
		t.Fatal(err)
	}
	if m.AgentVersion != "vector/1" || m.ProtocolVersion != "ipfs/0.1.0" || len(m.ListenAddrs) != 1 || len(m.Protocols) != 2 || m.ObservedAddr == nil || len(m.SignedPeerRecord) == 0 {
		t.Fatal("incomplete identify")
	}
	encoded, err := m.Marshal(nuproto.DefaultIdentifyLimits())
	if err != nil || !bytes.Equal(raw, encoded) {
		t.Fatal("upstream protobuf vector", err)
	}
	var stream bytes.Buffer
	first := wire.AppendBytes(nil, 3, []byte("/first/1"))
	if err := wire.WriteMessage(&stream, first, 8192); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteMessage(&stream, raw, 8192); err != nil {
		t.Fatal(err)
	}
	combined, err := nuproto.ReadIdentify(&stream, nuproto.DefaultIdentifyLimits())
	if err != nil || len(combined.Protocols) != 3 || combined.Protocols[0] != "/first/1" {
		t.Fatal("chunked identify", err)
	}
	limits := nuproto.DefaultIdentifyLimits()
	limits.MaxProtocols = 1
	if _, err := nuproto.ParseIdentify(raw, limits); !errors.Is(err, wire.ErrLimit) {
		t.Fatal("protocol count bound", err)
	}
	for range 11 {
		if err := wire.WriteMessage(&stream, nil, 8192); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := nuproto.ReadIdentify(&stream, nuproto.DefaultIdentifyLimits()); !errors.Is(err, wire.ErrLimit) {
		t.Fatal("parts bound", err)
	}
}
func TestIdentifyConnectionBinding(t *testing.T) {
	makeHost := func() *host.Host {
		k, err := wire.GenerateNodeKey()
		if err != nil {
			t.Fatal(err)
		}
		h, err := host.New(host.Config{Key: k})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		return h
	}
	a, b, other := makeHost(), makeHost(), makeHost()
	service, err := nuproto.New(a, nuproto.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	signed, err := other.SealPeerRecord(1, other.Addrs(), wire.RecordLimits{MaxBytes: 8192, MaxAddresses: 16, MaxAddressBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	m := nuproto.IdentifyMessage{PublicKey: b.PublicKey().Marshal(), ListenAddrs: b.Addrs(), Protocols: []string{nuproto.IdentifyID}, SignedPeerRecord: signed}
	raw, err := m.Marshal(nuproto.DefaultIdentifyLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Handle(nuproto.IdentifyID, 32, func(s *host.Stream) { _ = wire.WriteMessage(s, raw, 8192) }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addr, err := wire.ParseMultiaddr(b.Addrs()[0].String()+"/p2p/"+b.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Dial(ctx, addr); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Identify(ctx, b.ID()); !errors.Is(err, wire.ErrSignature) {
		t.Fatal("accepted third-party signed peer record", err)
	}
	if _, ok := service.Info(b.ID()); ok {
		t.Fatal("invalid identify entered peer cache")
	}
	if len(service.Observations()) != 0 {
		t.Fatal("invalid observation entered cache")
	}
}
func FuzzIdentify(f *testing.F) {
	f.Add(vector(f))
	f.Add([]byte{0x0a, 0xff})
	f.Add([]byte{0x1a, 0x04, 'b', 'a', 'd', '!'})
	f.Fuzz(func(t *testing.T, b []byte) {
		limits := nuproto.DefaultIdentifyLimits()
		if m, err := nuproto.ParseIdentify(b, limits); err == nil {
			if _, err := m.Marshal(limits); err != nil {
				t.Fatal(err)
			}
		}
		_, _ = nuproto.ReadIdentify(bytes.NewReader(b), limits)
	})
}
