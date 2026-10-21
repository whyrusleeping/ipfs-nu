// Command wirevectors reproduces the signed-peer-record golden vector using
// go-libp2p, independently of Nu's encoders. Run from the experiments directory.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/record"
	idpb "github.com/libp2p/go-libp2p/p2p/protocol/identify/pb"
	ma "github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/proto"
)

func main() {
	identify := flag.Bool("identify", false, "emit identify protobuf vector")
	flag.Parse()
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
	data := map[string]string{"peerID": id.String(), "base32": peer.ToCid(id).String(), "record": hex.EncodeToString(payload), "envelope": hex.EncodeToString(envelope)}
	if *identify {
		public, err := crypto.MarshalPublicKey(k.GetPublic())
		check(err)
		version, agent := "ipfs/0.1.0", "vector/1"
		message := &idpb.Identify{PublicKey: public, ListenAddrs: [][]byte{addr.Bytes()}, Protocols: []string{"/ipfs/id/1.0.0", "/ipfs/ping/1.0.0"}, ObservedAddr: addr.Bytes(), ProtocolVersion: &version, AgentVersion: &agent, SignedPeerRecord: envelope}
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
		check(err)
		data = map[string]string{"identify": hex.EncodeToString(b)}
	}
	out, err := json.MarshalIndent(data, "", "  ")
	check(err)
	fmt.Println(string(out))
}
func check(err error) {
	if err != nil {
		panic(err)
	}
}
