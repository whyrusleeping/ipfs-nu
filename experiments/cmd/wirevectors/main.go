// Command wirevectors reproduces the signed-peer-record golden vector using
// go-libp2p, independently of Nu's encoders. Run from the experiments directory.
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/record"
	ma "github.com/multiformats/go-multiaddr"
)

func main() {
	b, err := os.ReadFile("../p2p/wire/testdata/vectors/keys.json")
	check(err)
	var keys struct{ Ed25519Private string }
	check(json.Unmarshal(b, &keys))
	b, err = hex.DecodeString(keys.Ed25519Private)
	check(err)
	k, err := crypto.UnmarshalPrivateKey(b)
	check(err)
	id, err := peer.IDFromPrivateKey(k)
	check(err)
	addr, err := ma.NewMultiaddr("/ip4/127.0.0.1/udp/1234/quic-v1")
	check(err)
	r := &peer.PeerRecord{PeerID: id, Seq: 1, Addrs: []ma.Multiaddr{addr}}
	payload, err := r.MarshalRecord()
	check(err)
	env, err := record.Seal(r, k)
	check(err)
	envelope, err := env.Marshal()
	check(err)
	out, err := json.MarshalIndent(map[string]string{"peerID": id.String(), "base32": peer.ToCid(id).String(), "record": hex.EncodeToString(payload), "envelope": hex.EncodeToString(envelope)}, "", "  ")
	check(err)
	fmt.Println(string(out))
}
func check(err error) {
	if err != nil {
		panic(err)
	}
}
