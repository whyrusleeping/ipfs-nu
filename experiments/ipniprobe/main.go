// Command ipniprobe checks whether an IPNI indexer (cid.contact) ingests and
// serves BLAKE3 (0x1e) multihashes (backlog 0087/0095).
//
// It publishes a small DAG-CBOR ad chain from a throwaway identity, announces it,
// waits for random BLAKE3 and SHA-256 control entries to appear through /multihash/
// and /routing/v1/providers/, then publishes IsRm ads and waits for the entries to
// disappear. Two contexts are used: one with transport-bitswap metadata (a known
// code) and one with an unregistered private-use code, so a missing result can be
// told apart from a metadata rejection.
//
// The indexer must reach the publisher over HTTPS, for example through a
// cloudflared quick tunnel:
//
//	cloudflared tunnel --url http://127.0.0.1:8090 &
//	go run . -public https://<name>.trycloudflare.com -provider-addr /ip4/<public IPv4>/udp/4001/quic-v1
//
// Indexers ignore providers whose addresses are all private or documentation
// ranges. The CDN in front of cid.contact caches results for 20+ minutes, so the
// removal phase can time out even after the indexer has dropped the entries.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime"
	"github.com/ipld/go-ipld-prime/linking"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/ipld/go-ipld-prime/storage/memstore"
	"github.com/ipni/go-libipni/announce/httpsender"
	"github.com/ipni/go-libipni/announce/message"
	"github.com/ipni/go-libipni/dagsync/ipnisync"
	headschema "github.com/ipni/go-libipni/dagsync/ipnisync/head"
	"github.com/ipni/go-libipni/ingest/schema"
	"github.com/ipni/go-libipni/metadata"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multicodec"
	mh "github.com/multiformats/go-multihash"
	_ "github.com/multiformats/go-multihash/register/blake3"
)

// privateUseTransport is in the multicodec private-use range (0x300000–0x3fffff),
// standing in for a not-yet-registered Nu transport code.
const privateUseTransport = 0x3f0087

var (
	listen   = flag.String("listen", "127.0.0.1:8090", "local publisher HTTP address")
	public   = flag.String("public", "", "public https:// URL that reaches -listen (required)")
	indexer  = flag.String("indexer", "https://cid.contact", "indexer base URL")
	provAddr = flag.String("provider-addr", "", "public provider multiaddr put in the ads (required; indexers drop private and documentation addresses)")
	wait     = flag.Duration("wait", 30*time.Minute, "how long to wait for each phase")
	remove   = flag.Bool("remove", true, "publish IsRm ads after the entries appear")
	settle   = flag.Duration("settle", 30*time.Second, "wait after an announce before the first lookup")
	codec    = flag.String("codec", "dag-cbor", "ad and entry-chunk codec: dag-cbor (DRISL-compatible) or dag-json")
)

// serve answers the ipnisync HTTP protocol: GET /ipni/v1/ad/head returns the
// signed head and GET /ipni/v1/ad/<cid> the stored block bytes.
func serve(w http.ResponseWriter, r *http.Request, store *memstore.Store, priv crypto.PrivKey, mu *sync.Mutex, root *cid.Cid) int {
	ask, ok := strings.CutPrefix(r.URL.Path, ipnisync.IPNIPath+"/")
	if !ok || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return http.StatusNotFound
	}
	if ask == "head" {
		mu.Lock()
		c := *root
		mu.Unlock()
		sh, err := headschema.NewSignedHead(c, "", priv)
		check(err)
		b, err := sh.Encode()
		check(err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
		return http.StatusOK
	}
	c, err := cid.Parse(ask)
	if err != nil {
		http.Error(w, "not a cid", http.StatusBadRequest)
		return http.StatusBadRequest
	}
	b, err := store.Get(r.Context(), string(c.Bytes()))
	if err != nil {
		http.NotFound(w, r)
		return http.StatusNotFound
	}
	_, _ = w.Write(b)
	return http.StatusOK
}

type probeCtx struct {
	name     string
	md       []byte
	b3, sha  mh.Multihash
	b3c, shc cid.Cid
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if *public == "" || *provAddr == "" {
		log.Fatal("-public and -provider-addr are required")
	}
	pub, err := url.Parse(*public)
	if err != nil || pub.Scheme != "https" {
		log.Fatalf("-public must be an https URL: %v", err)
	}

	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	check(err)
	pid, err := peer.IDFromPrivateKey(priv)
	check(err)
	log.Printf("provider/publisher %s", pid)

	store := &memstore.Store{}
	lsys := cidlink.DefaultLinkSystem()
	lsys.SetReadStorage(store)
	lsys.SetWriteStorage(store)
	codecs := map[string]multicodec.Code{"dag-cbor": multicodec.DagCbor, "dag-json": multicodec.DagJson}
	code, ok := codecs[*codec]
	if !ok {
		log.Fatalf("-codec must be dag-cbor or dag-json")
	}
	lp := cidlink.LinkPrototype{Prefix: cid.Prefix{Version: 1, Codec: uint64(code), MhType: mh.SHA2_256, MhLength: -1}}
	put := func(n ipld.Node) cidlink.Link {
		l, err := lsys.Store(linking.LinkContext{}, lp, n)
		check(err)
		return l.(cidlink.Link)
	}

	// go-libipni's Publisher re-encodes every block as dag-json, so it can only
	// serve dag-json chains. This minimal ipnisync server returns the stored
	// bytes, which the indexer hashes against the requested CID.
	var mu sync.Mutex
	var root cid.Cid
	srv := &http.Server{Addr: *listen, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := serve(w, r, store, priv, &mu, &root)
		log.Printf("http %s %s -> %d (%s)", r.Method, r.URL.Path, status, r.UserAgent())
	})}
	go func() { log.Fatal(srv.ListenAndServe()) }()

	bsmd := metadata.Default.New(metadata.Bitswap{})
	bitswap, err := bsmd.MarshalBinary()
	check(err)
	unknown := binary.AppendUvarint(nil, privateUseTransport)
	unknown = append(unknown, []byte("nu-probe")...)
	ctxs := []*probeCtx{newCtx("nu-0087-probe-bitswap", bitswap), newCtx("nu-0087-probe-unknown", unknown)}

	addr, err := multiaddr.NewMultiaddr(*provAddr)
	check(err)
	pubAddr, err := multiaddr.NewMultiaddr("/dns/" + pub.Hostname() + "/tcp/443/https")
	check(err)
	iu, err := url.Parse(*indexer)
	check(err)
	sender, err := httpsender.New([]*url.URL{iu}, pid)
	check(err)

	var prev ipld.Link
	publish := func(ad schema.Advertisement) {
		ad.PreviousID = prev
		ad.Provider = pid.String()
		ad.Addresses = []string{addr.String()}
		check(ad.Sign(priv))
		n, err := ad.ToNode()
		check(err)
		l := put(n)
		prev = l
		mu.Lock()
		root = l.Cid
		mu.Unlock()
		log.Printf("ad %s context=%s rm=%v", l.Cid, ad.ContextID, ad.IsRm)
	}
	announce := func() {
		msg := message.Message{Cid: prev.(cidlink.Link).Cid}
		msg.SetAddrs([]multiaddr.Multiaddr{pubAddr})
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		check(sender.Send(ctx, msg))
		log.Printf("announced head %s to %s via %s", msg.Cid, *indexer, pubAddr)
	}

	for _, c := range ctxs {
		chunk, err := schema.EntryChunk{Entries: []mh.Multihash{c.b3, c.sha}}.ToNode()
		check(err)
		publish(schema.Advertisement{Entries: put(chunk), ContextID: []byte(c.name), Metadata: c.md})
		log.Printf("%s: blake3 %s (%s), sha256 %s (%s)", c.name, c.b3.B58String(), c.b3c, c.sha.B58String(), c.shc)
	}
	announce()
	poll(ctxs, true)

	if !*remove {
		return
	}
	for _, c := range ctxs {
		publish(schema.Advertisement{Entries: schema.NoEntries, ContextID: []byte(c.name), IsRm: true})
	}
	announce()
	poll(ctxs, false)
	os.Exit(exitCode)
}

// exitCode is 1 when the removal phase timed out (the entries may still be
// cached by the CDN, so this is inconclusive rather than a failed removal).
var exitCode int

func newCtx(name string, md []byte) *probeCtx {
	buf := make([]byte, 64)
	_, err := rand.Read(buf)
	check(err)
	b3, err := mh.Sum(buf, mh.BLAKE3, 32)
	check(err)
	sha, err := mh.Sum(buf, mh.SHA2_256, -1)
	check(err)
	return &probeCtx{name: name, md: md, b3: b3, sha: sha, b3c: cid.NewCidV1(cid.Raw, b3), shc: cid.NewCidV1(cid.Raw, sha)}
}

// poll waits until every probe key is found (want=true) or gone (want=false)
// through both lookup paths, logging each change.
func poll(ctxs []*probeCtx, want bool) {
	time.Sleep(*settle)
	start := time.Now()
	last := map[string]string{}
	for {
		done := true
		for _, c := range ctxs {
			for _, k := range []struct {
				label string
				m     mh.Multihash
				c     cid.Cid
			}{{"blake3", c.b3, c.b3c}, {"sha256", c.sha, c.shc}} {
				for _, path := range []string{"/multihash/" + k.m.B58String(), "/routing/v1/providers/" + k.c.String()} {
					code, body := get(*indexer + path)
					found := code == http.StatusOK && strings.Contains(body, "Provider")
					key := c.name + " " + k.label + " " + path
					state := fmt.Sprintf("%d %s", code, body)
					if last[key] != state {
						log.Printf("+%s %s %s: %d %s", time.Since(start).Round(time.Second), c.name, k.label, code, trim(body))
						last[key] = state
					}
					// Absence needs an explicit 404; errors and 5xx are inconclusive.
					if (want && !found) || (!want && code != http.StatusNotFound) {
						done = false
					}
				}
			}
		}
		if done {
			log.Printf("phase complete (found=%v) after %s", want, time.Since(start).Round(time.Second))
			return
		}
		if time.Since(start) > *wait {
			log.Printf("phase timed out (want found=%v) after %s", want, *wait)
			if want {
				os.Exit(1)
			}
			exitCode = 1
			return
		}
		time.Sleep(20 * time.Second)
	}
}

// client bounds each lookup: a stalled HTTP/2 stream to the CDN otherwise hangs
// the poll loop forever.
var client = &http.Client{Timeout: 30 * time.Second}

// get fetches u. cid.contact's CDN caches lookups, and a 404 for a key looked up
// before ingest was still served after 15 minutes (per Accept-Encoding
// variant), so poll waits -settle after each announce before its first lookup.
func get(u string) (int, string) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	check(err)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(bytes.TrimSpace(b))
}

func trim(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func check(err error) {
	if err != nil {
		log.Output(2, err.Error())
		os.Exit(1)
	}
}
