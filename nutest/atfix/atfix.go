// Package atfix is a fake atproto world for tests: a PLC directory, a handle host and a
// PDS on one httptest server, serving the vendored real repository CARs
// (testdata/vectors/atproto-repos). Tests never touch the network: the atproto.Config it
// returns routes every SSRF-checked dial to the server and resolves DNS handles locally.
package atfix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/httpsrc"
)

// The account behind the fixture: did:plc:rpozqnrysnmae4hegfralite on blacksky.app,
// whose getRepo and one getRecord response are vendored.
const (
	DID      = "did:plc:rpozqnrysnmae4hegfralite"
	Handle   = "alice.test" // confirmed by the fake DID document
	DNSName  = "bob.test"   // resolves by DNS TXT to DID but is not in alsoKnownAs
	Key      = "zQ3shoQrpowBZqq8SftAybKcpjnrFcvLbhaCxvnzFHRXRDiqY"
	OtherKey = "zQ3shZB4ipxkuhwFnG8TqaCLYTNtUPWPXSA5CQqYaMzSY8EbR" // another account's key
	LikePath = "app.bsky.feed.like/3lvwmxcdh2s27"
	LikeURI  = "at://" + DID + "/" + LikePath
)

// Fixture is the fake world. Knobs may be changed between requests.
type Fixture struct {
	Srv *httptest.Server
	// Repo and Proof are the CAR bytes served for getRepo and getRecord(LikePath).
	Repo, Proof []byte
	mu          sync.Mutex
	// KeyMultibase is the signing key the DID document advertises (Key by default).
	KeyMultibase string
	// PDS is the endpoint the DID document names (the fake PDS by default).
	PDS string
	// Tamper flips a byte inside the last block of every CAR served.
	Tamper bool
	// Requests counts PDS requests by XRPC method.
	Requests map[string]int
}

// VectorDir is the vendored CAR directory, located from this source file.
func VectorDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "vectors", "atproto-repos")
}

// New starts the fixture; it stops with the test.
func New(t testing.TB) *Fixture {
	f := &Fixture{KeyMultibase: Key, PDS: "http://pds.test", Requests: map[string]int{}}
	var err error
	if f.Repo, err = os.ReadFile(filepath.Join(VectorDir(), "ts-pds-rpozqnrysnmae4hegfralite.car")); err != nil {
		t.Fatal(err)
	}
	if f.Proof, err = os.ReadFile(filepath.Join(VectorDir(), "ts-pds-rpozqnrysnmae4hegfralite.getRecord-like-3lvwmxcdh2s27.car")); err != nil {
		t.Fatal(err)
	}
	f.Srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Srv.Close)
	return f
}

// Set changes knobs under the lock.
func (f *Fixture) Set(fn func(f *Fixture)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *Fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Host {
	case "plc.test":
		if r.URL.Path != "/"+DID {
			http.Error(w, "DID not registered", 404)
			return
		}
		doc := map[string]any{
			"id":          DID,
			"alsoKnownAs": []string{"at://" + Handle},
			"verificationMethod": []map[string]string{{"id": DID + "#atproto", "type": "Multikey",
				"controller": DID, "publicKeyMultibase": f.KeyMultibase}},
			"service": []map[string]string{{"id": "#atproto_pds", "type": "AtprotoPersonalDataServer", "serviceEndpoint": f.PDS}},
		}
		_ = json.NewEncoder(w).Encode(doc)
	case Handle, "evil.test":
		if r.URL.Path == "/.well-known/atproto-did" {
			fmt.Fprintln(w, DID)
			return
		}
		http.NotFound(w, r)
	case "pds.test":
		method := strings.TrimPrefix(r.URL.Path, "/xrpc/")
		f.Requests[method]++
		q := r.URL.Query()
		if q.Get("did") != DID {
			http.Error(w, `{"error":"RepoNotFound"}`, 400)
			return
		}
		var body []byte
		switch method {
		case "com.atproto.sync.getRecord":
			if q.Get("collection")+"/"+q.Get("rkey") != LikePath {
				http.Error(w, `{"error":"RecordNotFound"}`, 400)
				return
			}
			body = f.Proof
		case "com.atproto.sync.getRepo":
			body = f.Repo
		case "com.atproto.sync.getBlob":
			http.Error(w, `{"error":"BlobNotFound"}`, 404)
			return
		default:
			http.NotFound(w, r)
			return
		}
		if f.Tamper {
			body = bytes.Clone(body)
			body[len(body)-10] ^= 0x55
		}
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		_, _ = w.Write(body)
	default:
		http.Error(w, "unknown host "+r.Host, 421)
	}
}

// Config is an atproto client configuration that reaches only the fixture.
func (f *Fixture) Config() atproto.Config {
	addr := strings.TrimPrefix(f.Srv.URL, "http://")
	return atproto.Config{
		HTTP: httpsrc.Config{
			AllowHTTP:  true,
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
			LookupIP: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
		PLCDirectory: "http://plc.test",
		PlainHTTP:    true,
		LookupTXT: func(_ context.Context, name string) ([]string, error) {
			if name == "_atproto."+DNSName {
				return []string{"did=" + DID}, nil
			}
			return nil, errors.New("no such host")
		},
		Limits: atproto.Limits{Timeout: 5 * time.Second, RepoTimeout: 5 * time.Second},
	}
}
