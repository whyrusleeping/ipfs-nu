package fisl_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fisl"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/rasl"
	"github.com/whyrusleeping/nu/store"
)

var now = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)

// Decoding follows the spec's record shape under Nu's bounds: both formats, unknown
// fields ignored, record expiry as a ceiling on addr expiry, expired records/addrs
// dropped, malformed or non-DID publishers treated as absent, size unverified and
// optional, hostile bodies (wrong shape, huge lines, too many records) bounded.
func TestDecode(t *testing.T) {
	const spec = `{"publisher":"did:plc:ewvi7nxzyoun6zhxrhs64oiz","addrs":[{"url":"https://berjon.com/.well-known/rasl/bafkreifn5yxi7nkftsn46b6x26grda57ict7md2xuvfbsgkiahe2e7vnq4"},{"url":"https://mirror.example.com/kitten.jpg","expires":"2026-09-01T00:00:00Z"}],"size":94201,"expires":"2026-12-18T00:00:00Z","x-extra":1}
{"publisher":"did:web:mirror.example.com","addrs":[{"url":"https://mirror.example.com/kitten.jpg","expires":"2027-08-30T00:00:00Z"},{"url":"radiroh://25igmk4u75iqsbosfvep5sxoozazizgoxfnuqzsnl22pbddig5ra"}],"expires":"2027-01-01T00:00:00Z"}
{"addrs":[{"url":"https://cache.example.net/kitten.jpg"}],"publisher":5,"size":"big"}
{"addrs":[{"url":"https://gone.example/x"}],"expires":"2020-01-01T00:00:00Z"}
{"addrs":[{"url":"https://bad-date.example/x"}],"expires":"yesterday"}
{"addrs":[{"url":"relative/path"},{"url":"https://ok.example/y","expires":"nope"},{"url":""}]}
{"publisher":"did:key:z6Mk","addrs":[]}
{"publisher":"did:key:z6Mk"}
`
	locs, err := fisl.DecodeJSONL(strings.NewReader(spec), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) != 3 {
		t.Fatalf("got %d records: %+v", len(locs), locs)
	}
	first := locs[0]
	if first.Publisher != "did:plc:ewvi7nxzyoun6zhxrhs64oiz" || first.Size != 94201 || len(first.Addrs) != 1 || first.Addrs[0].Expires != first.Expires {
		t.Fatalf("first record %+v", first) // the expired mirror addr is dropped; the RASL addr inherits the record expiry
	}
	second := locs[1]
	if second.Publisher != "did:web:mirror.example.com" || len(second.Addrs) != 2 || !second.Addrs[0].Expires.Equal(second.Expires) || !second.Addrs[1].Expires.Equal(second.Expires) {
		t.Fatalf("second record %+v", second) // record expiry is the ceiling; the unknown scheme is kept for the client to skip
	}
	if third := locs[2]; third.Publisher != "" || third.Size != -1 || len(third.Addrs) != 1 {
		t.Fatalf("third record %+v", third)
	}

	// The json format carries the same records; other members are ignored.
	var lines []json.RawMessage
	for _, l := range strings.Split(strings.TrimSpace(spec), "\n") {
		lines = append(lines, json.RawMessage(l))
	}
	body, _ := json.Marshal(map[string]any{"extra": map[string]any{"a": []int{1}}, "locations": lines, "z": nil})
	jl, err := fisl.DecodeJSON(bytes.NewReader(body), now)
	if err != nil || len(jl) != 3 || jl[0].Publisher != first.Publisher || len(jl[1].Addrs) != 2 {
		t.Fatalf("json: %v %+v", err, jl)
	}
	if _, err := fisl.DecodeJSON(strings.NewReader(`{"locations":[]}`), now); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`[]`, `{"providers":[]}`, `{"locations":{}}`, `{"locations":[1]}`, `<!doctype html>`, `{"locations":[{"addrs":[{"url":"https://a.example/"}]}`} {
		if _, err := fisl.DecodeJSON(strings.NewReader(bad), now); !errors.Is(err, fisl.ErrFormat) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	// JSONL: records before a malformed line survive with ErrFormat; a huge line and a
	// flood of records are bounded.
	locs, err = fisl.DecodeJSONL(strings.NewReader(`{"addrs":[{"url":"https://a.example/"}]}`+"\n{broken\n"), now)
	if !errors.Is(err, fisl.ErrFormat) || len(locs) != 1 {
		t.Fatalf("partial: %v %d", err, len(locs))
	}
	huge := `{"addrs":[{"url":"https://a.example/"}],"pad":"` + strings.Repeat("x", fisl.MaxLineBytes) + `"}` + "\n"
	if _, err := fisl.DecodeJSONL(strings.NewReader(huge), now); !errors.Is(err, fisl.ErrFormat) {
		t.Fatalf("huge line: %v", err)
	}
	var flood strings.Builder
	for i := 0; i < 3*fisl.MaxRecords; i++ {
		fmt.Fprintf(&flood, `{"addrs":[{"url":"https://h%d.example/"}]}`+"\n", i)
	}
	if locs, err := fisl.DecodeJSONL(strings.NewReader(flood.String()), now); err != nil || len(locs) != fisl.MaxRecords {
		t.Fatalf("flood: %v %d", err, len(locs))
	}
	var manyAddrs []map[string]string
	for i := 0; i < 3*fisl.MaxAddrs; i++ {
		manyAddrs = append(manyAddrs, map[string]string{"url": fmt.Sprintf("https://h%d.example/", i)})
	}
	rec, _ := json.Marshal(map[string]any{"addrs": manyAddrs})
	if locs, err := fisl.DecodeJSONL(bytes.NewReader(append(rec, '\n')), now); err != nil || len(locs[0].Addrs) != fisl.MaxAddrs {
		t.Fatalf("many addrs: %v %+v", err, locs)
	}

	for in, want := range map[string]string{
		"https://idx.example":       "https://idx.example",
		"https://idx.example/v1/":   "https://idx.example/v1",
		"http://127.0.0.1:8080":     "http://127.0.0.1:8080",
		"idx.example":               "",
		"https://u:p@idx.example":   "",
		"https://idx.example/?x=1":  "",
		"ftp://idx.example":         "",
		"https://idx.example/#frag": "",
	} {
		got, err := fisl.ParseIndexer(in)
		if (err == nil) != (want != "") || got != want {
			t.Errorf("ParseIndexer(%q) = %q, %v", in, got, err)
		}
	}
	if u := fisl.LookupURL("https://idx.example/v1", "bafy", "jsonl"); u != "https://idx.example/v1/routing/locations/bafy/jsonl" {
		t.Fatal(u)
	}
}

// indexer is an httptest FISL indexer answering from a map of CID → records, in both
// formats, with the spec's status codes.
type indexer struct {
	srv     *httptest.Server
	records map[string][]map[string]any
	hits    atomic.Int32
	alter   func(w http.ResponseWriter, r *http.Request) bool
}

func newIndexer(t *testing.T) *indexer {
	ix := &indexer{records: map[string][]map[string]any{}}
	ix.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ix.hits.Add(1)
		if ix.alter != nil && ix.alter(w, r) {
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, fisl.PathPrefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		c, format, _ := strings.Cut(rest, "/")
		if _, err := cid.Parse(c); err != nil {
			http.Error(w, "bad cid", http.StatusBadRequest)
			return
		}
		recs := ix.records[c]
		w.Header().Set("Cache-Control", "public, max-age=5")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		switch format {
		case "json":
			w.Header().Set("Content-Type", "application/json")
			if recs == nil {
				recs = []map[string]any{}
			}
			json.NewEncoder(w).Encode(map[string]any{"locations": recs})
		case "jsonl":
			w.Header().Set("Content-Type", "application/jsonl")
			for _, rec := range recs {
				b, _ := json.Marshal(rec)
				w.Write(append(b, '\n'))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ix.srv.Close)
	return ix
}

func (ix *indexer) add(c cid.CID, rec map[string]any) {
	ix.records[c.String()] = append(ix.records[c.String()], rec)
}

func addrs(urls ...string) []map[string]any {
	var out []map[string]any
	for _, u := range urls {
		out = append(out, map[string]any{"url": u})
	}
	return out
}

// mirror serves whatever bytes it is given at any path (a location URL), optionally
// corrupting them.
func mirror(t *testing.T, data []byte, lie bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lie {
			w.Write(append([]byte("not "), data...))
			return
		}
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testSource(t *testing.T, client *httpsrc.Client, indexers ...string) (*fisl.Source, *store.Store) {
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if client == nil {
		client = httpsrc.New(httpsrc.Config{AllowHTTP: true, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	}
	src := fisl.NewSource(fisl.Config{
		Indexers:  indexers,
		Retrieve:  rasl.Config{Client: client, Store: st, Dir: t.TempDir(), Scheme: "http", Timeout: 10 * time.Second, IdleTimeout: 5 * time.Second},
		AllowHTTP: true, LookupTimeout: 5 * time.Second, IdleTimeout: 2 * time.Second,
	})
	return src, st
}

// A Source asks the configured indexers, skips unsupported schemes, expired addrs and
// (by default) http URLs, fetches from the located URLs with RASL's verified race, and
// demotes the publisher whose location served wrong bytes; an indexer that answers
// something other than FISL (an SPA fallback) or is down does not stop the others.
func TestSource(t *testing.T) {
	ctx := context.Background()
	small := []byte("hello FISL")
	rawSHA := cid.New(cid.Raw, cid.Sum(cid.SHA256, small))
	big := bytes.Repeat([]byte("blake3 blob "), 100<<10) // 1.2 MiB
	rawB3 := cid.New(cid.Raw, cid.Sum(cid.BLAKE3, big))

	good := mirror(t, small, false)
	liar := mirror(t, small, true)
	bigSrv := mirror(t, big, false)
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // a RASL server announced as an ordinary URL
		if r.URL.Path == rasl.WellKnown+rawSHA.String() {
			w.Write(small)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(rs.Close)

	ix, down, html := newIndexer(t), newIndexer(t), newIndexer(t)
	down.srv.Close()
	html.alter = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<!doctype html><title>overview</title>"))
		return true
	}
	ix.add(rawSHA, map[string]any{"publisher": "did:web:liar.example", "addrs": addrs(liar.URL+"/kitten", "radiroh://25igmk4u75iqsbosfvep5sxoozazizgoxfnuqzsnl22pbddig5ra"), "size": 3})
	ix.add(rawSHA, map[string]any{"publisher": "did:plc:good", "addrs": append(addrs(good.URL+"/kitten"), map[string]any{"url": good.URL + "/old", "expires": "2020-01-01T00:00:00Z"}), "expires": "2099-01-01T00:00:00Z"})
	ix.add(rawSHA, map[string]any{"addrs": addrs(rs.URL + rasl.WellKnown + rawSHA.String())})
	ix.add(rawB3, map[string]any{"publisher": "did:plc:good", "addrs": addrs(bigSrv.URL + "/big.bin")})

	src, st := testSource(t, nil, html.srv.URL, down.srv.URL, ix.srv.URL)
	locs, err := src.Lookup(ctx, rawSHA)
	if err != nil || len(locs) != 3 {
		t.Fatalf("lookup: %v %+v", err, locs)
	}
	fresh, demoted := src.Candidates(locs)
	if len(fresh) != 3 || len(demoted) != 0 { // the unknown scheme and the expired addr are skipped
		t.Fatalf("candidates %v %v", fresh, demoted)
	}
	// Fetch: the liar is raced with the good ones; the first verified copy wins.
	if err := src.FetchObject(ctx, rawSHA, 0); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if data, err := st.Get(rawSHA); err != nil || !bytes.Equal(data, small) {
		t.Fatalf("stored %q %v", data, err)
	}
	// A liar-only record: mismatch, nothing stored, publisher and origin demoted.
	onlyCID := cid.New(cid.Raw, cid.Sum(cid.SHA256, []byte("liar only")))
	ix.add(onlyCID, map[string]any{"publisher": "did:web:liar.example", "addrs": addrs(liar.URL + "/only")})
	err = src.FetchObject(ctx, onlyCID, 0)
	if !errors.Is(err, rasl.ErrMismatch) {
		t.Fatalf("liar only: %v", err)
	}
	if _, ok, _ := st.Has(onlyCID); ok {
		t.Fatal("mismatched bytes were stored")
	}
	if !src.Demoted("did:web:liar.example") {
		t.Fatal("publisher not demoted")
	}
	if fresh, demoted := src.Candidates(locs); len(demoted) != 1 || !strings.HasPrefix(demoted[0], liar.URL) || len(fresh) != 2 {
		t.Fatalf("after demotion: %v %v", fresh, demoted)
	}
	// A large BLAKE3 blob goes through the staged, Bao-verified path.
	if err := src.FetchObject(ctx, rawB3, 0); err != nil {
		t.Fatal(err)
	}
	if info, ok, _ := st.Has(rawB3); !ok || !info.Complete {
		t.Fatal("big blob not held")
	}
	// Unknown CID: every indexer answers, none has a location.
	missing := cid.New(cid.Raw, cid.Sum(cid.SHA256, []byte("missing")))
	if err := src.FetchObject(ctx, missing, 0); !errors.Is(err, fisl.ErrNoLocation) {
		t.Fatalf("missing: %v", err)
	}
	// Only unusable indexers: a LookupError naming each.
	var le *fisl.LookupError
	bad, _ := testSource(t, nil, html.srv.URL, down.srv.URL)
	if err := bad.FetchObject(ctx, missing, 0); !errors.As(err, &le) || len(le.Attempts) != 2 || !errors.Is(err, fisl.ErrFormat) {
		t.Fatalf("bad indexers: %v", err)
	}
	// An indexer without jsonl is asked for json; a huge response is cut off.
	jsonOnly := newIndexer(t)
	jsonOnly.alter = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/jsonl") {
			http.NotFound(w, r)
			return true
		}
		return false
	}
	jsonOnly.add(rawSHA, map[string]any{"addrs": addrs(good.URL + "/k")})
	js, _ := testSource(t, nil, jsonOnly.srv.URL)
	if locs, err := js.Lookup(ctx, rawSHA); err != nil || len(locs) != 1 || jsonOnly.hits.Load() != 2 {
		t.Fatalf("json fallback: %v %+v %d", err, locs, jsonOnly.hits.Load())
	}
	flood := newIndexer(t)
	flood.alter = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/jsonl")
		line := []byte(`{"addrs":[{"url":"https://h.example/"}],"pad":"` + strings.Repeat("x", 1000) + `"}` + "\n")
		for i := 0; i < 2*fisl.MaxResponseBytes/len(line); i++ {
			if _, err := w.Write(line); err != nil {
				break
			}
		}
		return true
	}
	fl, _ := testSource(t, nil, flood.srv.URL)
	if locs, err := fl.Lookup(ctx, rawSHA); err != nil || len(locs) != fisl.MaxRecords {
		t.Fatalf("flood: %v %d", err, len(locs))
	}
	// http locations are skipped unless configured.
	strict := fisl.NewSource(fisl.Config{Indexers: []string{ix.srv.URL}, Retrieve: rasl.Config{Client: httpsrc.New(httpsrc.Config{}), Store: st}})
	if fresh, demoted := strict.Candidates(locs); len(fresh)+len(demoted) != 0 {
		t.Fatalf("http kept: %v %v", fresh, demoted)
	}
}

// Locations (and indexers) at private, loopback or link-local addresses never reach a
// dial, before and after DNS.
func TestSSRF(t *testing.T) {
	var dials atomic.Int32
	client := httpsrc.New(httpsrc.Config{
		LookupIP: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("unexpected connection")
		}})
	c := cid.New(cid.Raw, cid.Sum(cid.SHA256, []byte("x")))
	src, _ := testSource(t, client, "https://10.0.0.1", "https://[::1]:8443", "https://rebinds.example")
	var le *fisl.LookupError
	if err := src.FetchObject(context.Background(), c, 0); !errors.As(err, &le) || len(le.Attempts) != 3 {
		t.Fatalf("%v", err)
	}
	for _, a := range le.Attempts {
		if !errors.Is(a.Err, httpsrc.ErrPolicy) {
			t.Fatalf("%s: %v", a.URL, a.Err)
		}
	}
	locs := []fisl.Location{{Addrs: []fisl.Addr{{URL: "https://169.254.169.254/latest"}, {URL: "https://127.0.0.1:8171/x"}, {URL: "https://rebinds.example/x"}}}}
	fresh, _ := src.Candidates(locs)
	err := rasl.NewRetriever(rasl.Config{Client: client, Store: nil, Timeout: time.Second, IdleTimeout: time.Second}).Fetch(context.Background(), c, 0, fresh)
	var se *rasl.SourceError
	if !errors.As(err, &se) || len(se.Attempts) != 3 {
		t.Fatalf("%v", err)
	}
	for _, a := range se.Attempts {
		if !errors.Is(a.Err, httpsrc.ErrPolicy) {
			t.Fatalf("%s: %v", a.URL, a.Err)
		}
	}
	if dials.Load() != 0 {
		t.Fatal("a denied destination was dialed")
	}
}

// Opt-in live check: NU_LIVE_FISL=<indexer-url>,<cid> looks the CID up on a real
// indexer and fetches it, verified. (No public FISL indexer was known when this test was
// written.)
func TestLive(t *testing.T) {
	raw := os.Getenv("NU_LIVE_FISL")
	if raw == "" {
		t.Skip("set NU_LIVE_FISL=<indexer-url>,<cid> to run")
	}
	idx, cs, _ := strings.Cut(raw, ",")
	base, err := fisl.ParseIndexer(idx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cid.Parse(cs)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := fisl.NewSource(fisl.Config{Indexers: []string{base}, Retrieve: rasl.Config{Client: httpsrc.New(httpsrc.Config{}), Store: st, Timeout: time.Minute, IdleTimeout: 20 * time.Second}})
	locs, err := src.Lookup(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d records: %+v", len(locs), locs)
	if err := src.FetchObject(context.Background(), c, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := st.OpenBlob(c.Multihash()); err == nil {
		defer f.Close()
		n, _ := io.Copy(io.Discard, io.NewSectionReader(f, 0, int64(f.Size)))
		t.Logf("fetched %d bytes", n)
	}
}
