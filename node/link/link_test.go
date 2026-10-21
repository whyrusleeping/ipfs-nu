package link

import (
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
)

const (
	rawCID   = "bafkreiccldh766hwcnuxnf2wh6jgzepf2nlu2lvcllt63eww5p6chi4ity"
	dagPBCID = "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi"
)

func testPeer(t *testing.T) wire.PeerID {
	t.Helper()
	k, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.Public().PeerID()
}

// A link with a path, two providers (one with addresses) and a via round-trips through
// String and Parse; the formatted form keeps multiaddrs readable.
func TestRoundTrip(t *testing.T) {
	p1, p2 := testPeer(t), testPeer(t)
	a1, _ := wire.ParseMultiaddr("/ip4/192.168.1.7/udp/4171/quic-v1", MaxAddrLen)
	a2, _ := wire.ParseMultiaddr("/ip6/2001:db8::7/udp/4171/quic-v1", MaxAddrLen)
	in := &Link{
		CID:  cid.MustParse(rawCID),
		Path: "docs/intro & more.html",
		Providers: []Provider{
			{Peer: p1, Addrs: []wire.Multiaddr{a1, a2}},
			{Peer: p2},
		},
		Via: "did:plc:abc123",
	}
	s := in.String()
	if !strings.HasPrefix(s, "nu://"+rawCID+"/docs/intro%20%26%20more.html?peer="+p1.Base36()+"&addr=/ip4/192.168.1.7/udp/4171/quic-v1&addr=/ip6/2001:db8::7/udp/4171/quic-v1&peer="+p2.Base36()+"&via=did:plc:abc123") {
		t.Fatalf("formatted: %s", s)
	}
	out, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	if out.CID != in.CID || out.Path != in.Path || out.Via != in.Via || len(out.Providers) != 2 ||
		out.Providers[0].Peer != p1 || out.Providers[1].Peer != p2 || len(out.Providers[0].Addrs) != 2 ||
		out.Providers[0].Addrs[1].String() != a2.String() || len(out.Providers[1].Addrs) != 0 {
		t.Fatalf("parsed %+v", out)
	}
	if out.String() != s {
		t.Fatalf("second format differs: %s", out.String())
	}
	if out.Target() != rawCID+"/docs/intro & more.html" {
		t.Fatalf("target %q", out.Target())
	}
	// The minimal link is just the CID; scheme is case-insensitive.
	if l, err := Parse("NU://" + rawCID); err != nil || l.String() != "nu://"+rawCID || l.Target() != rawCID {
		t.Fatalf("minimal: %v %v", l, err)
	}
}

func TestRejects(t *testing.T) {
	p := testPeer(t).Base36()
	cases := []struct{ in, want string }{
		{"", "starts with nu://"},
		{"nu://", "missing CID"},
		{"http://" + rawCID, "starts with nu://"},
		{"nu://" + dagPBCID, "not a DASL CID"},
		{"nu://" + strings.ToUpper(rawCID), "not a DASL CID"},
		{"nu://" + rawCID + "/", "empty segment"},
		{"nu://" + rawCID + "//a", "empty segment"},
		{"nu://" + rawCID + "/a/../b", "not allowed"},
		{"nu://" + rawCID + "/a/%2Fb", "slash or NUL"},
		{"nu://" + rawCID + "/a/%zz", "percent-encoding"},
		{"nu://" + rawCID + " ", "whitespace"},
		{"nu://" + rawCID + "#frag", "whitespace, NUL or a fragment"},
		{"nu://" + rawCID + "?", "empty query"},
		{"nu://" + rawCID + "?peer", "key=value"},
		{"nu://" + rawCID + "?peer=nope", "not a peer ID"},
		{"nu://" + rawCID + "?peer=" + p + "&peer=" + p, "twice"},
		{"nu://" + rawCID + "?addr=/ip4/1.2.3.4/udp/1/quic-v1", "before any peer="},
		{"nu://" + rawCID + "?peer=" + p + "&addr=/ip4/1.2.3.4/udp/1/quic-v1/p2p/" + p, "names a peer"},
		{"nu://" + rawCID + "?peer=" + p + "&addr=/ip4/1.2.3/udp/1", "not a multiaddr"},
		{"nu://" + rawCID + "?peer=" + p + "&addr=" + strings.Repeat("/ip4/1.2.3.4", 30), "longer than"},
		{"nu://" + rawCID + "?via=plc:abc", "not a DID"},
		{"nu://" + rawCID + "?via=did:plc:a&via=did:plc:b", "twice"},
		{"nu://" + rawCID + "?color=blue", "unknown query parameter"},
		{"nu://" + rawCID + "?" + manyPeers(t, MaxProviders+1), "more than"},
		{"nu://" + rawCID + "/" + strings.Repeat("a", MaxLen), "longer than"},
	}
	for _, tc := range cases {
		_, err := Parse(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%q): err %v, want %q", tc.in, err, tc.want)
		}
	}
}

func manyPeers(t *testing.T, n int) string {
	var parts []string
	for range n {
		parts = append(parts, "peer="+testPeer(t).Base36())
	}
	return strings.Join(parts, "&")
}

// FuzzParse: anything that parses formats back to a link that parses to the same thing.
func FuzzParse(f *testing.F) {
	p, _ := wire.GenerateNodeKey()
	id := p.Public().PeerID().Base36()
	for _, s := range []string{
		"nu://" + rawCID,
		"nu://" + rawCID + "/docs/intro.html",
		"nu://" + rawCID + "/docs/intro%20%26%20more.html?peer=" + id + "&addr=/ip4/192.168.1.7/udp/4171/quic-v1&via=did:plc:abc",
		"nu://" + rawCID + "?peer=" + id + "&addr=/ip6/2001:db8::7/udp/4171/quic-v1&addr=/dns4/host.example/udp/4171/quic-v1",
		"nu://" + rawCID + "?peer=" + id + "&addr=/ip4/1.2.3.4/udp/1/quic-v1/p2p/" + id,
		"nu://" + dagPBCID,
		"nu://" + rawCID + "?addr=/ip4/1.2.3.4/udp/1/quic-v1",
		"nu://" + rawCID + "/a/../b",
		"nu:///",
		"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		l, err := Parse(s)
		if err != nil {
			return
		}
		out := l.String()
		if len(out) > MaxLen+MaxLen/2 {
			t.Fatalf("formatted link grew beyond bounds: %d", len(out))
		}
		l2, err := Parse(out)
		if err != nil {
			t.Fatalf("formatted %q does not parse: %v", out, err)
		}
		if l2.String() != out || l2.Target() != l.Target() || len(l2.Providers) != len(l.Providers) || l2.Via != l.Via {
			t.Fatalf("round trip changed %q -> %q", out, l2.String())
		}
	})
}
