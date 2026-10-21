package lookup_test

import (
	"testing"

	"github.com/miekg/dns"
	"github.com/whyrusleeping/nu/p2p/lookup"
)

func mdnsSeed(t testing.TB) []byte {
	t.Helper()
	service := lookup.ServiceName("")
	msg := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Authoritative: true}, Answer: []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: "instance." + service, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 120}, Txt: []string{"dnsaddr=/ip4/127.0.0.1/udp/4171/quic-v1/p2p/" + peerText}}}}
	b, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func FuzzMDNS(f *testing.F) {
	f.Add(mdnsSeed(f))
	f.Add([]byte{0, 0, 0, 0, 255, 255})
	f.Add(make([]byte, 12))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = lookup.DecodeMDNS(b, lookup.ServiceName(""), 9000, 128) })
}
