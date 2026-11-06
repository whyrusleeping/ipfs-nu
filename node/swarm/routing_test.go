package swarm

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/nutest/timeouts"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// A fresh node gets a bare Nu CID from a provider it learns only from a delegated-routing
// (IPNI) record: no link hints, no connection, no mDNS, no DHT (0094).
func TestBareCIDFromRoutedProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Ceiling())
	defer cancel()
	var holder *Swarm
	routing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text, _ := holder.ListenAddrs()[0].Text()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Providers":[{"Schema":"peer","ID":"%s","Protocols":["unknown"],"Addrs":["%s"]}]}`, holder.ID(), text)
	}))
	defer routing.Close()
	start := func(providers string) *Swarm {
		k, err := wire.GenerateNodeKey()
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(t.TempDir(), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		s, err := Start(ctx, Config{Key: k, Store: st, Listen: []string{"/ip4/127.0.0.1/udp/0/quic-v1"}, Logger: discard,
			DHT: DHTConfig{Mode: "off", AllowPrivateAddresses: true, ProvidersURL: providers,
				ProvidersHTTP: httpsrc.Config{AllowHTTP: true, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	holder = start("off")
	getter := start(routing.URL)
	data := []byte("found through IPNI")
	c, err := holder.cfg.Store.AddBlob(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := getter.Fetch(ctx, c, nil, nil); err != nil {
		t.Fatalf("bare-CID fetch from a routed provider: %v", err)
	}
	if got, err := getter.cfg.Store.Get(c); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("fetched %q, %v", got, err)
	}
}
