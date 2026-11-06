package bridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"testing"
	"time"
)

type testProvider struct{ Peer kad.Peer }

func (p testProvider) FindProviders(context.Context, []byte) ([]kad.Peer, error) {
	return []kad.Peer{p.Peer}, nil
}

func TestBitswapSourceOverNuHost(t *testing.T) {
	payload := []byte("bitswap 1.2 payload")
	id, server, addr := bitswapServer(t, payload)
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	clientHost, err := host.New(host.Config{Key: key, Listen: []string{"127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer clientHost.Close()
	src := NewBitswapSource(clientHost, testProvider{Peer: kad.Peer{ID: server.ID(), Addrs: [][]byte{addr.Bytes()}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := src.GetBlock(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q", got)
	}
}

type laterProvider struct {
	testProvider
	delay time.Duration
}

func (p laterProvider) FindProviders(ctx context.Context, key []byte) ([]kad.Peer, error) {
	time.Sleep(p.delay)
	return p.testProvider.FindProviders(ctx, key)
}

// A peer one finder names at a stale address is dialed again at the address a later
// finder names (a stale IPNI record, then Amino's current one).
func TestBitswapSourceMergesProviderAddrs(t *testing.T) {
	payload := []byte("merged provider addresses")
	id, server, addr := bitswapServer(t, payload)
	stale, err := wire.ParseMultiaddr("/ip4/127.0.0.1/udp/1/quic-v1/p2p/"+server.ID().String(), 2048)
	if err != nil {
		t.Fatal(err)
	}
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	client, err := host.New(host.Config{Key: key, Listen: []string{"127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	src := NewBitswapSource(client, testProvider{Peer: kad.Peer{ID: server.ID(), Addrs: [][]byte{stale.Bytes()}}},
		laterProvider{testProvider{Peer: kad.Peer{ID: server.ID(), Addrs: [][]byte{stale.Bytes(), addr.Bytes()}}}, 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got, err := src.GetBlock(ctx, id); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("got %q, %v", got, err)
	}
}

// bitswapServer serves payload over Bitswap 1.2 and returns its CID, host and address.
func bitswapServer(t *testing.T, payload []byte) (IPFSCID, *host.Host, wire.Multiaddr) {
	t.Helper()
	id := IPFSCID{Version: 1, Codec: codecRaw, Hash: hashSHA256, Digest: sha256.Sum256(payload)}
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	server, err := host.New(host.Config{Key: key, Listen: []string{"127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	if err := server.Handle(BitswapProtocol, 4, func(st *host.Stream) {
		remote := st.Conn().PeerID()
		req, err := readDelimited(bufio.NewReader(st), maxBitswapMessage)
		_ = st.Close()
		if err != nil {
			return
		}
		want, err := parseBitswapWant(req)
		if err != nil || want != id {
			return
		}
		reply, err := server.OpenStream(context.Background(), remote, BitswapProtocol)
		if err != nil {
			return
		}
		defer reply.Close()
		prefix := appendUvarint(nil, 1)
		prefix = appendUvarint(prefix, codecRaw)
		prefix = appendUvarint(prefix, hashSHA256)
		prefix = appendUvarint(prefix, 32)
		block := pbBytes(nil, 1, prefix)
		block = pbBytes(block, 2, payload)
		response := pbBytes(nil, 3, block)
		_ = writeDelimited(reply, response)
	}); err != nil {
		t.Fatal(err)
	}
	addrs := server.Addrs()
	if len(addrs) == 0 {
		t.Fatal("server has no address")
	}
	text, err := addrs[0].Text()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := wire.ParseMultiaddr(fmt.Sprintf("%s/p2p/%s", text, server.ID().String()), 2048)
	if err != nil {
		t.Fatal(err)
	}
	return id, server, addr
}
func parseBitswapWant(message []byte) (IPFSCID, error) {
	for len(message) > 0 {
		f, w, value, rest, err := pbField(message)
		if err != nil {
			return IPFSCID{}, err
		}
		message = rest
		if f != 1 || w != 2 {
			continue
		}
		want := value
		for len(want) > 0 {
			wf, ww, entry, next, err := pbField(want)
			if err != nil {
				return IPFSCID{}, err
			}
			want = next
			if wf != 1 || ww != 2 {
				continue
			}
			for len(entry) > 0 {
				ef, ew, block, tail, err := pbField(entry)
				if err != nil {
					return IPFSCID{}, err
				}
				entry = tail
				if ef == 1 && ew == 2 {
					return ParseIPFSBytes(block)
				}
			}
		}
	}
	return IPFSCID{}, fmt.Errorf("Bitswap wantlist missing CID")
}
