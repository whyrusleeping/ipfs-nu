package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/client"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/repo"
)

// testCA writes a CA and a leaf certificate for domain and *.domain to dir and returns
// the cert/key paths and a pool trusting the CA.
func testCA(t *testing.T, dir, domain string) (certPath, keyPath string, pool *x509.CertPool) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "nu test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain, "*." + domain},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	keyDER, _ := x509.MarshalECPrivateKey(leafKey)
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	pool = x509.NewCertPool()
	pool.AddCert(caCert)
	return certPath, keyPath, pool
}

// The public listener end to end (0043): configured with cert/key files for a domain, it
// serves over TLS only roots marked public, share links name it, status shows it, and
// the daemon refuses the listener on a private-cluster member (N28), without TLS, or
// with behind_proxy off loopback.
func TestPublicListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	r, _, err := repo.Open(filepath.Join(dir, "nu"))
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath, pool := testCA(t, dir, "seed.example")
	for k, v := range map[string]string{"public.listen": "127.0.0.1:0", "public.domain": "seed.example", "public.cert": certPath, "public.key": keyPath} {
		if err := r.ConfigSet(k, v); err != nil {
			t.Fatal(err)
		}
	}
	opts := func() Options {
		return Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0", NoNetwork: true, Logger: slog.New(slog.DiscardHandler)}
	}
	d, err := Start(ctx, opts())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, err := client.Discover(r)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := c.Status(ctx); err != nil || st.Daemon.Public != "https://seed.example" {
		t.Fatalf("status public: %+v %v", st, err)
	}

	// An HTTPS client that resolves seed.example and *.seed.example to the listener and
	// trusts the test CA (what a browser does once DNS and the certificate are in place).
	hc := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, d.PublicAddr())
		},
	}}
	fetch := func(u string) (*http.Response, string) {
		t.Helper()
		res, err := hc.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}

	// Add a tree (pinned, private by default) and a lone file.
	site := filepath.Join(dir, "site")
	os.MkdirAll(filepath.Join(site, "sub"), 0o755)
	os.WriteFile(filepath.Join(site, "index.html"), []byte("<h1>public</h1>"), 0o644)
	os.WriteFile(filepath.Join(site, "sub", "notes.txt"), []byte("0123456789"), 0o644)
	add := func(path string, rec bool) string {
		o, err := c.Add(ctx, apiv0.AddRequest{Path: path, Recursive: rec, Pin: true})
		if err != nil {
			t.Fatal(err)
		}
		fin, err := c.WaitOperation(ctx, o.ID, 0)
		if err != nil || fin.State != apiv0.StateSucceeded {
			t.Fatalf("add: %+v %v", fin, err)
		}
		var ar apiv0.AddResult
		json.Unmarshal(fin.Result, &ar)
		return ar.CID
	}
	tree := add(site, true)
	secret := filepath.Join(dir, "secret.txt")
	os.WriteFile(secret, []byte("members only"), 0o644)
	private := add(secret, false)

	// P2: pinned is not public. Nothing is served until marked, and share has no https link.
	if res, _ := fetch("https://seed.example/tree/" + tree + "/"); res.StatusCode != 404 {
		t.Fatalf("unmarked tree served: %d", res.StatusCode)
	}
	if sl, err := c.Share(ctx, apiv0.ShareRequest{Target: tree}); err != nil || sl.HTTPS != "" || sl.RASL != "" {
		t.Fatalf("share before public: %+v %v", sl, err)
	}
	pr, err := c.PublicAdd(ctx, apiv0.PublicRequest{CID: tree})
	if err != nil || pr.HTTPS != "https://seed.example/tree/"+tree+"/" || pr.Site != "https://"+tree+".seed.example/" || !strings.HasPrefix(pr.RASL, "rasl://"+tree+"/?hint=seed.example") {
		t.Fatalf("public add: %+v %v", pr, err)
	}
	// Served now: listing, file with Range, the site origin rendering HTML, RASL with CORS.
	if res, body := fetch(pr.HTTPS); res.StatusCode != 200 || !strings.Contains(body, "index.html") || !strings.Contains(body, pr.Site) {
		t.Fatalf("listing: %d %q", res.StatusCode, body)
	}
	req, _ := http.NewRequest(http.MethodGet, pr.HTTPS+"sub/notes.txt", nil)
	req.Header.Set("Range", "bytes=0-2")
	if res, err := hc.Do(req); err != nil || res.StatusCode != 206 {
		t.Fatalf("range: %v %v", res, err)
	} else {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(b) != "012" || res.Header.Get("ETag") == "" || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
			t.Fatalf("range body %q %s", b, res.Header)
		}
	}
	if res, body := fetch(pr.Site); res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") || body != "<h1>public</h1>" {
		t.Fatalf("site: %d %s %q", res.StatusCode, res.Header, body)
	}
	if res, _ := fetch("https://seed.example/tree/" + tree + "/index.html"); res.Header.Get("Content-Disposition") == "" || res.Header.Get("Link") == "" {
		t.Fatalf("bare-origin html must be a download with a site Link: %s", res.Header)
	}
	if res, _ := fetch("https://seed.example/.well-known/rasl/" + tree); res.StatusCode != 200 || res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("rasl: %d %s", res.StatusCode, res.Header)
	}
	// The private file stays private on every route (including RASL and its site origin),
	// and the API does not exist here (P1/N28).
	for _, u := range []string{"/tree/" + private, "/blob/" + private, "/.well-known/rasl/" + private, "/api/v0/status", "/api/v0/public"} {
		if res, _ := fetch("https://seed.example" + u); res.StatusCode != 404 {
			t.Fatalf("%s: %d", u, res.StatusCode)
		}
	}
	if res, _ := fetch("https://" + private + ".seed.example/"); res.StatusCode != 404 {
		t.Fatalf("private site origin: %d", res.StatusCode)
	}
	// Share now names the seed; a path inside keeps the root public URL shape.
	sl, err := c.Share(ctx, apiv0.ShareRequest{Target: tree + "/sub/notes.txt"})
	if err != nil || sl.HTTPS != "https://seed.example/tree/"+tree+"/sub/notes.txt" || sl.Site != "" || sl.RASL != pr.RASL {
		t.Fatalf("share: %+v %v", sl, err)
	}
	if page, err := c.PublicList(ctx, "", 0); err != nil || len(page.Items) != 1 || page.Items[0].CID != tree {
		t.Fatalf("public ls: %+v %v", page, err)
	}
	// Removing the mark stops serving at once; the explicit pin stays.
	if err := c.PublicRm(ctx, tree); err != nil {
		t.Fatal(err)
	}
	if res, _ := fetch(pr.HTTPS); res.StatusCode != 404 {
		t.Fatalf("after rm: %d", res.StatusCode)
	}
	if err := c.PublicRm(ctx, tree); !isCode(err, apiv0.CodeNotFound) {
		t.Fatalf("second rm: %v", err)
	}
	if page, _ := c.Pins(ctx, "", 0); len(page.Items) != 2 {
		t.Fatalf("pins after public rm: %+v", page.Items)
	}
	d.Close()

	// Refusals name the key. Missing TLS; behind_proxy off loopback; a members-only node.
	set := func(kv map[string]string) {
		for k, v := range kv {
			if err := r.ConfigSet(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	refuse := func(what string) {
		t.Helper()
		if _, err := Start(ctx, opts()); !isCode(err, apiv0.CodeInvalidArgument) || !strings.Contains(err.Error(), "public.") {
			t.Fatalf("%s: %v", what, err)
		}
	}
	set(map[string]string{"public.cert": `""`, "public.key": `""`})
	refuse("no tls")
	set(map[string]string{"public.behind_proxy": "true", "public.listen": "0.0.0.0:0"})
	refuse("proxy off loopback")
	set(map[string]string{"public.listen": "127.0.0.1:0", "public.domain": "localhost"})
	refuse("localhost domain")
	set(map[string]string{"public.domain": "seed.example"})
	// behind_proxy on loopback starts (plain HTTP) …
	d2, err := Start(ctx, opts())
	if err != nil {
		t.Fatalf("behind proxy: %v", err)
	}
	if res, err := http.Get("http://" + d2.PublicAddr() + "/"); err != nil || res.StatusCode != 403 { // Host is the IP, not the domain
		t.Fatalf("proxy mode host check: %v %v", res, err)
	}
	d2.Close()
	// … but not on a member of a private cluster (N28).
	n, err := node.NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.ClusterInit(ctx, apiv0.ClusterInitRequest{Name: "lab", Mode: "private"}); err != nil {
		t.Fatal(err)
	}
	n.Close()
	refuse("private cluster member")
}
