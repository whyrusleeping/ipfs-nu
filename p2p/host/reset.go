package host

import (
	"crypto/hmac"
	"crypto/sha256"

	quic "github.com/quic-go/quic-go"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// resetKey is stable across process restarts, not across node identities. A random
// per-process key makes resets from a restarted node unverifiable by survivors.
// Domain separation keeps this secret independent of identity signatures/TLS.
// As with the node identity, never run two live hosts with the same key.
func resetKey(key *wire.NodeKey, family int) quic.StatelessResetKey {
	mac := hmac.New(sha256.New, key.Marshal())
	mac.Write([]byte("nu/libp2p/quic-v1/stateless-reset/v1"))
	// Hosts have one transport per family. Separate keys prevent the other
	// family acting as a reset-token oracle for a live connection's CID.
	mac.Write([]byte{byte(family)})
	return quic.StatelessResetKey(mac.Sum(nil))
}
