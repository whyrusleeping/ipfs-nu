//go:build linux && natlab

package collection_test

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/p2p/natlab"
)

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

// A private-mode receiving member fetches item URLs only from the collection's
// ingest.hosts (docs/collections.md "Egress"): in a fresh network namespace with a
// default route out of a dummy interface, nftables counts packets to the listed host,
// to any other non-loopback address, and DNS queries. Pushes naming an unlisted address
// or name, and redirects from the listed host to them, must leave no packet and no
// query; the listed host is reached.
func TestPrivateIngestEgress(t *testing.T) {
	if !natlab.Isolate(t) {
		return
	}
	sh(t, "ip", "link", "set", "lo", "up")
	sh(t, "ip", "link", "add", "egress0", "type", "dummy")
	sh(t, "ip", "addr", "add", "10.99.0.1/24", "dev", "egress0")
	sh(t, "ip", "link", "set", "egress0", "up")
	sh(t, "ip", "route", "add", "default", "dev", "egress0")
	sh(t, "nft", "add table inet egress")
	sh(t, "nft", "add chain inet egress out { type filter hook output priority 0 ; }")
	sh(t, "nft", "add rule inet egress out ip daddr 10.99.0.1 counter comment \"listed\"")
	sh(t, "nft", "add rule inet egress out ip daddr != { 127.0.0.0/8, 10.99.0.1 } counter comment \"other\"")
	sh(t, "nft", "add rule inet egress out ip6 daddr != ::1 counter comment \"other6\"")
	sh(t, "nft", "add rule inet egress out udp dport 53 counter comment \"dns\"")
	sh(t, "nft", "add rule inet egress out tcp dport 53 counter comment \"dnstcp\"")

	ln, err := net.Listen("tcp", "10.99.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redir-ip":
			http.Redirect(w, r, "http://10.99.0.2/x", http.StatusFound)
		case "/redir-name":
			http.Redirect(w, r, "http://secret.example/x", http.StatusFound)
		default:
			w.Header().Set("ETag", `"v"`)
			_, _ = w.Write([]byte(strings.Repeat("scene ", 1000)))
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()
	base := "http://" + ln.Addr().String()
	// Trusted local config: plain HTTP and the lab subnet (a producer's LAN web seed);
	// the resolver and dialer are the real ones.
	node.IngestHTTP = func(c *httpsrc.Config) {
		c.AllowHTTP, c.AllowedIPs = true, []netip.Prefix{netip.MustParsePrefix("10.99.0.0/24")}
	}
	defer func() { node.IngestHTTP = nil }()

	n := nutest.New(t, 1)
	a := n.Nodes[0]
	if _, err := a.Local.ClusterInit(ctx(t), apiv0.ClusterInitRequest{Name: "collgs", Mode: cluster.ModePrivate}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "s2", Collection: true,
		Ingest: &apiv0.DatasetIngest{Hosts: []string{"10.99.0.1"}, Attempts: 1}}); err != nil {
		t.Fatal(err)
	}
	push := func(path, url string) (*apiv0.CollectionPush, error) {
		return a.Local.CollectionPush(context.Background(), "s2", apiv0.CollectionPushRequest{Path: path, Meta: []byte(`{}`),
			Files: map[string]apiv0.CollectionPushFile{"f": {URL: url}}}, nutest.Timeout)
	}
	if p, err := push("ok", base+"/ok"); err != nil || p.State != apiv0.PushWritten {
		t.Fatalf("listed host: %+v %v", p, err)
	}
	for _, u := range []string{"http://10.99.0.2/x", "http://secret.example/x"} {
		if _, err := push("unlisted", u); apiCode(err) != apiv0.CodeForbidden {
			t.Fatalf("%s accepted: %v", u, err)
		}
	}
	for _, p := range []string{"/redir-ip", "/redir-name"} {
		if res, err := push("redirect"+strings.ReplaceAll(p, "/", "-"), base+p); err != nil || res.State != apiv0.PushFailed {
			t.Fatalf("%s: %+v %v", p, res, err)
		}
	}

	counters := sh(t, "nft", "list table inet egress")
	re := regexp.MustCompile(`packets (\d+) bytes \d+ comment "(\w+)"`)
	m := re.FindAllStringSubmatch(counters, -1)
	if len(m) != 5 {
		t.Fatalf("cannot read counters:\n%s", counters)
	}
	for _, c := range m {
		p, _ := strconv.Atoi(c[1])
		if (c[2] == "listed") != (p > 0) {
			t.Errorf("%s packets: %d\n%s", c[2], p, counters)
		}
	}
}
