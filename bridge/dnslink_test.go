package bridge_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/miekg/dns"
	"github.com/whyrusleeping/nu/bridge"
	"github.com/whyrusleeping/nu/httpsrc"
)

// DNSLink names resolve through `/ipns/` links to the lexicographically first root, `_dnslink.`
// first; names under a DoH suffix (ENS's .eth) are asked of the RFC 8484 endpoint instead.
func TestDNSLinkResolve(t *testing.T) {
	const a, b = "bafybeia5eyombgegdwmvfzwisveadaaok2vdjkv5rklfdmcljysjrjc4km", "bafkreifj7scygtmgbjlbykf4jb4uaw2uqsawglh777es6dxfmju6ipgaey"
	txt := map[string][]string{
		"_dnslink.example.org": {"dnslink=/ipns/site.example.net"},
		"example.org":          {"dnslink=/ipfs/" + a}, // shadowed by _dnslink
		"site.example.net":     {"v=spf1", "dnslink=/ipfs/" + b, "dnslink=/ipfs/" + a},
	}
	doh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		packet, err := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if err != nil || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/dns-message" || q.Unpack(packet) != nil {
			http.Error(w, "bad query", 400)
			return
		}
		reply := new(dns.Msg)
		reply.SetReply(q)
		if q.Question[0].Name == "_dnslink.vitalik.eth." {
			reply.Answer = append(reply.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60}, Txt: []string{"dnslink=/ipfs/", a}})
		} else {
			reply.Rcode = dns.RcodeNameError
		}
		out, _ := reply.Pack()
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(out)
	}))
	defer doh.Close()
	r := &bridge.DNSLinkResolver{
		LookupTXT: func(_ context.Context, name string) ([]string, error) { return txt[name], nil },
		DoH:       map[string]string{"eth": doh.URL},
		Client:    httpsrc.New(httpsrc.Config{AllowHTTP: true, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}),
	}
	for target, want := range map[string]string{"example.org": b, "/ipns/vitalik.eth": a} {
		_, name, err := bridge.ParseImportTarget(target)
		if err != nil {
			t.Fatal(target, err)
		}
		got, err := r.Resolve(context.Background(), name)
		if err != nil || got.String() != want {
			t.Fatalf("%s resolved to %s (%v), want %s", target, got, err, want)
		}
	}
	if _, err := r.Resolve(context.Background(), "nothing.example"); err == nil {
		t.Fatal("a name without dnslink records resolved")
	}
}
