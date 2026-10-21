package lookup

import (
	"github.com/miekg/dns"
	"testing"
)

func FuzzDNSADDRWire(f *testing.F) {
	m := new(dns.Msg)
	m.SetQuestion("_dnsaddr.peers.example.test.", dns.TypeTXT)
	packet, _ := m.Pack()
	f.Add(packet)
	f.Add([]byte{0, 1, 2})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = parseDNSMessage(b, DefaultDNSLimits()) })
}
