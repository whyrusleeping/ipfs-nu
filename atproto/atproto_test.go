package atproto_test

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
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/httpsrc"
)

const (
	vecDir   = "../testdata/vectors/atproto-repos/"
	aliceDID = "did:plc:rpozqnrysnmae4hegfralite"
	aliceKey = "zQ3shoQrpowBZqq8SftAybKcpjnrFcvLbhaCxvnzFHRXRDiqY"
	otherKey = "zQ3shZB4ipxkuhwFnG8TqaCLYTNtUPWPXSA5CQqYaMzSY8EbR" // another account's key
	likePath = "app.bsky.feed.like/3lvwmxcdh2s27"
)

// fixture is a fake PLC directory, handle host and PDS on one httptest server, serving
// the vendored real CARs. Hostnames are routed by Host; the httpsrc hooks send every
// (SSRF-checked) dial to the server.
type fixture struct {
	t      *testing.T
	srv    *httptest.Server
	repo   []byte // getRepo CAR for alice
	proof  []byte // getRecord CAR for likePath
	key    string // key the DID document advertises
	pds    string
	tamper bool
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, key: aliceKey, pds: "http://pds.test"}
	var err error
	if f.repo, err = os.ReadFile(vecDir + "ts-pds-rpozqnrysnmae4hegfralite.car"); err != nil {
		t.Fatal(err)
	}
	if f.proof, err = os.ReadFile(vecDir + "ts-pds-rpozqnrysnmae4hegfralite.getRecord-like-3lvwmxcdh2s27.car"); err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	switch r.Host {
	case "plc.test":
		if r.URL.Path != "/"+aliceDID {
			http.Error(w, "DID not registered", 404)
			return
		}
		doc := map[string]any{
			"id":          aliceDID,
			"alsoKnownAs": []string{"at://alice.test"},
			"verificationMethod": []map[string]string{{"id": aliceDID + "#atproto", "type": "Multikey",
				"controller": aliceDID, "publicKeyMultibase": f.key}},
			"service": []map[string]string{{"id": "#atproto_pds", "type": "AtprotoPersonalDataServer", "serviceEndpoint": f.pds}},
		}
		_ = json.NewEncoder(w).Encode(doc)
	case "alice.test", "evil.test":
		if r.URL.Path == "/.well-known/atproto-did" {
			fmt.Fprintln(w, aliceDID)
			return
		}
		http.NotFound(w, r)
	case "pds.test":
		q := r.URL.Query()
		if q.Get("did") != aliceDID {
			http.Error(w, `{"error":"RepoNotFound"}`, 400)
			return
		}
		var body []byte
		switch r.URL.Path {
		case "/xrpc/com.atproto.sync.getRecord":
			if q.Get("collection")+"/"+q.Get("rkey") != likePath {
				http.Error(w, `{"error":"RecordNotFound"}`, 400)
				return
			}
			body = f.proof
		case "/xrpc/com.atproto.sync.getRepo":
			body = f.repo
		default:
			http.NotFound(w, r)
			return
		}
		if f.tamper {
			body = bytes.Clone(body)
			body[len(body)-10] ^= 0x55 // inside the last block's data
		}
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		_, _ = w.Write(body)
	default:
		http.Error(w, "unknown host "+r.Host, 421)
	}
}

func (f *fixture) config() atproto.Config {
	addr := strings.TrimPrefix(f.srv.URL, "http://")
	return atproto.Config{
		HTTP: httpsrc.Config{
			AllowHTTP:  true,
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
			LookupIP:   func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil },
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
		PLCDirectory: "http://plc.test",
		PlainHTTP:    true,
		LookupTXT: func(_ context.Context, name string) ([]string, error) {
			if name == "_atproto.bob.test" {
				return []string{"did=" + aliceDID}, nil
			}
			return nil, errors.New("no such host")
		},
		Limits: atproto.Limits{Timeout: 5 * time.Second, RepoTimeout: 5 * time.Second},
	}
}

func TestFetch(t *testing.T) {
	f := newFixture(t)
	c := atproto.New(f.config())
	ctx := context.Background()

	t.Run("record via handle", func(t *testing.T) {
		u, err := atproto.ParseURI("at://Alice.Test/" + likePath)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := c.GetRecord(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if rec.DID != aliceDID || rec.Handle != "alice.test" || rec.Rev == "" || !rec.Commit.Defined() || len(rec.Proof) < 3 {
			t.Fatalf("record %+v", rec)
		}
		v, err := drisl.Decode(rec.Value)
		if err != nil {
			t.Fatal(err)
		}
		if v.(map[string]any)["$type"] != "app.bsky.feed.like" {
			t.Fatalf("value %v", v)
		}
	})
	t.Run("record via DNS handle, cached identity", func(t *testing.T) {
		u, _ := atproto.ParseURI("at://bob.test/" + likePath)
		if _, err := c.GetRecord(ctx, u); err == nil || !strings.Contains(err.Error(), "does not list it") {
			t.Fatalf("bob.test is not in alsoKnownAs: %v", err) // DNS resolved, bidirectional check failed
		}
		u, _ = atproto.ParseURI("at://" + aliceDID + "/app.bsky.feed.like/nope")
		_, err := c.GetRecord(ctx, u)
		var ae *apiv0.Error
		if !errors.As(err, &ae) || ae.Code != apiv0.CodeNotFound {
			t.Fatalf("missing record: %v", err)
		}
	})
	t.Run("repo snapshot", func(t *testing.T) {
		id, err := c.Resolve(ctx, aliceDID)
		if err != nil {
			t.Fatal(err)
		}
		st := mst.MemStore{}
		var progressed bool
		snap, err := c.GetRepo(ctx, id, st, func(int64) { progressed = true })
		if err != nil {
			t.Fatal(err)
		}
		if snap.Stats.Records == 0 || snap.Rev == "" || snap.Bytes != int64(len(f.repo)) || !progressed {
			t.Fatalf("snapshot %+v", snap)
		}
		if _, _, ok, err := snap.Repo.Get(likePath); err != nil || !ok {
			t.Fatalf("record %s not in snapshot: %v", likePath, err)
		}
	})
	t.Run("tampered CAR", func(t *testing.T) {
		f.tamper = true
		defer func() { f.tamper = false }()
		id, _ := c.Resolve(ctx, aliceDID)
		if _, err := c.GetRepo(ctx, id, mst.MemStore{}, nil); err == nil || !strings.Contains(err.Error(), "does not verify") {
			t.Fatalf("tampered repo: %v", err)
		}
		u, _ := atproto.ParseURI("at://" + aliceDID + "/" + likePath)
		if _, err := c.GetRecord(ctx, u); err == nil || !strings.Contains(err.Error(), "does not verify") {
			t.Fatalf("tampered proof: %v", err)
		}
	})
	t.Run("wrong signing key", func(t *testing.T) {
		f.key = otherKey
		defer func() { f.key = aliceKey }()
		c := atproto.New(f.config()) // fresh cache
		u, _ := atproto.ParseURI("at://" + aliceDID + "/" + likePath)
		if _, err := c.GetRecord(ctx, u); err == nil || !errors.Is(err, atrepo.ErrSignature) && !strings.Contains(err.Error(), "signature") {
			t.Fatalf("wrong key: %v", err)
		}
		id, _ := c.Resolve(ctx, aliceDID)
		if _, err := c.GetRepo(ctx, id, mst.MemStore{}, nil); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("wrong key repo: %v", err)
		}
	})
	t.Run("PDS endpoint behind the SSRF guard", func(t *testing.T) {
		f.pds = "http://10.1.2.3"
		defer func() { f.pds = "http://pds.test" }()
		c := atproto.New(f.config())
		u, _ := atproto.ParseURI("at://" + aliceDID + "/" + likePath)
		_, err := c.GetRecord(ctx, u)
		var ae *apiv0.Error
		if !errors.As(err, &ae) || ae.Code != apiv0.CodeForbidden {
			t.Fatalf("private PDS address: %v", err)
		}
	})
	t.Run("uri forms", func(t *testing.T) {
		for _, bad := range []string{"at://", "at://alice.test/notansid", "at://alice.test/app.bsky.feed.post/a/b", "at://did:plc:x@notatid", "http://x"} {
			if _, err := atproto.ParseURI(bad); err == nil {
				t.Errorf("%q parsed", bad)
			}
		}
		u, err := atproto.ParseURI("at://" + aliceDID + "@3lvwmxcdh2s27/app.bsky.feed.like")
		if err != nil || u.Rev != "3lvwmxcdh2s27" || u.Collection != "app.bsky.feed.like" || u.IsRecord() {
			t.Fatalf("%+v %v", u, err)
		}
	})
}

// TestLive is an opt-in smoke test against a real public account (NU_LIVE=1).
func TestLive(t *testing.T) {
	if os.Getenv("NU_LIVE") == "" {
		t.Skip("set NU_LIVE=1 to fetch a real record from the network")
	}
	c := atproto.New(atproto.Config{})
	u, _ := atproto.ParseURI("at://" + aliceDID + "/" + likePath)
	rec, err := c.GetRecord(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live record %s at %s rev %s from %s (handle %s)", rec.CID, rec.Commit, rec.Rev, rec.PDS, rec.Handle)
}
