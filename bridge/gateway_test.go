package bridge_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/whyrusleeping/nu/bridge"
	"github.com/whyrusleeping/nu/bridge/ipfsfix"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/httpsrc"
)

// gateway serves dag's blocks as a trustless gateway (`/ipfs/<cid>?format=raw`); a liar
// serves every block with one byte flipped.
func gateway(t *testing.T, dag *ipfsfix.DAG, liar bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := bridge.ParseIPFSCID(strings.TrimPrefix(r.URL.Path, "/ipfs/"))
		b, ok := dag.Blocks[c]
		if err != nil || !ok || r.URL.Query().Get("format") != "raw" {
			http.NotFound(w, r)
			return
		}
		b = append([]byte(nil), b...)
		if liar {
			b[len(b)-1] ^= 1
		}
		w.Header().Set("Content-Type", "application/vnd.ipld.raw")
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A UnixFS DAG is imported from HTTP trustless gateways found through a routing/v1
// endpoint: the gateway serving wrong bytes is rejected and demoted, the honest one is
// reused for later blocks without another lookup, and fil.pub's signature field (an
// unknown UnixFS field) is dropped and reported instead of failing the root.
func TestGatewayImport(t *testing.T) {
	dag := ipfsfix.New()
	a, b := dag.File(make([]byte, 5<<10), 1<<10), dag.Raw([]byte("readme"))
	unix := binary.AppendUvarint([]byte{0x08, 0x01}, 1001<<3|2) // Type=Directory, field 1001
	sig := []byte(`{"v":1,"alg":"ed25519"}`)
	unix = append(binary.AppendUvarint(unix, uint64(len(sig))), sig...)
	root := dag.Node(unix, []ipfsfix.Link{{Name: "a.bin", CID: a}, {Name: "README.md", CID: b}})

	liar, honest := gateway(t, dag, true), gateway(t, dag, false)
	var lookups atomic.Int32
	routing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		var providers []map[string]any
		for _, s := range []*httptest.Server{liar, honest} {
			port := s.Listener.Addr().(*net.TCPAddr).Port
			providers = append(providers, map[string]any{"Schema": "peer", "ID": "12D3KooWfake", "Protocols": []string{bridge.GatewayProtocol},
				"Addrs": []string{fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/http", port)}})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"Providers": providers})
	}))
	defer routing.Close()

	client := httpsrc.New(httpsrc.Config{AllowHTTP: true, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	src := bridge.NewGatewaySource(client, routing.URL)
	got, err := bridge.Import(context.Background(), root, src, tree.HashSink{}, mst.MemStore{}, bridge.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := bridge.Import(context.Background(), root, mapSource(dag.Blocks), tree.HashSink{}, mst.MemStore{}, bridge.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != want.Root || got.Files != 2 {
		t.Fatalf("gateway import root %s (%d files), want %s", got.Root, got.Files, want.Root)
	}
	if n := lookups.Load(); n != 1 {
		t.Fatalf("%d routing lookups, want 1 (later blocks from the gateway that served)", n)
	}
	if !client.Demoted(liar.URL + "/") {
		t.Fatal("gateway serving wrong bytes was not demoted")
	}
	dropped := false
	for _, x := range got.Transformations {
		dropped = dropped || x.CID == root.String() && x.Reason == "unknown UnixFS field 1001 dropped"
	}
	if !dropped {
		t.Fatalf("unknown field not reported: %+v", got.Transformations)
	}
}
