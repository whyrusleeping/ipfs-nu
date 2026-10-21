package cluster

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// ProtocolID is the cluster protocol (docs/cluster.md "/nu/cluster/1").
const ProtocolID = "/nu/cluster/1"

// Wire limits.
const (
	MaxMessage = 64 << 10
	MaxHeads   = 1024
	// MaxDiffBytes bounds a pull response body.
	MaxDiffBytes = 64 << 20
)

// Request ops.
const (
	OpHeads     = "heads"
	OpPull      = "pull"
	OpHeartbeat = "heartbeat"
	OpJoin      = "join"
	OpAdmit     = "admit"
)

// Response statuses (those of /nu/bytes/1, plus forbidden).
const (
	StatusOK         = 0
	StatusNotFound   = 1
	StatusBadRequest = 3
	StatusBusy       = 4
	StatusInternal   = 5
	StatusForbidden  = 6
)

type headWire struct {
	DID    string  `drisl:"did"`
	Rev    string  `drisl:"rev"`
	Commit cid.CID `drisl:"commit"`
}

type request struct {
	Op      string     `drisl:"op"`
	Cluster cid.CID    `drisl:"cluster"`
	Heads   []headWire `drisl:"heads,omitempty"`
	DID     string     `drisl:"did,omitempty"`
	Since   cid.CID    `drisl:"since,omitempty"`
	Commit  cid.CID    `drisl:"commit,omitempty"`
	HB      *Heartbeat `drisl:"hb,omitempty"`
	Invite  string     `drisl:"invite,omitempty"`
	Proof   []byte     `drisl:"proof,omitempty"`
	Peer    []byte     `drisl:"peer,omitempty"`
	Addrs   []string   `drisl:"addrs,omitempty"`
}

type response struct {
	Status int64      `drisl:"status"`
	Error  string     `drisl:"error,omitempty"`
	Heads  []headWire `drisl:"heads,omitempty"`
}

// Heartbeat is a member's signed liveness report. Time is Unix milliseconds.
type Heartbeat struct {
	Peer  []byte   `drisl:"peer"`
	Seq   int64    `drisl:"seq"`
	Time  int64    `drisl:"time"`
	Free  int64    `drisl:"free"`
	Used  int64    `drisl:"used"`
	Addrs []string `drisl:"addrs,omitempty"`
	Sig   []byte   `drisl:"sig,omitempty"`
}

func (hb Heartbeat) message() ([]byte, error) {
	hb.Sig = nil
	b, err := drisl.Marshal(hb)
	if err != nil {
		return nil, err
	}
	return append([]byte("nu.cluster.heartbeat.v1\x00"), b...), nil
}

// Sign sets Sig with the node key.
func (hb *Heartbeat) Sign(k *wire.NodeKey) error {
	m, err := hb.message()
	if err != nil {
		return err
	}
	hb.Sig = k.Sign(m)
	return nil
}

// Verify checks Sig against the sender's public key, which must match hb.Peer.
func (hb *Heartbeat) Verify(pub wire.PublicKey) error {
	if string(pub.PeerID().Bytes()) != string(hb.Peer) {
		return errors.New("cluster: heartbeat names another peer")
	}
	m, err := hb.message()
	if err != nil {
		return err
	}
	return pub.Verify(m, hb.Sig)
}

func headsWire(hs []HeadInfo) []headWire {
	out := make([]headWire, 0, len(hs))
	for _, h := range hs {
		out = append(out, headWire(h))
	}
	return out
}

func headsFromWire(ws []headWire) []HeadInfo {
	out := make([]HeadInfo, 0, len(ws))
	for _, w := range ws {
		if _, err := ParseRev(w.Rev); err != nil || !w.Commit.Defined() {
			continue
		}
		out = append(out, HeadInfo(w))
	}
	return out
}

func parseRequest(b []byte) (*request, error) {
	var r request
	if err := (drisl.DecOptions{MaxSize: MaxMessage, MaxDepth: 6}).Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if len(r.Heads) > MaxHeads || len(r.Addrs) > 16 {
		return nil, errors.New("too many heads or addresses")
	}
	return &r, nil
}

// StatusError is a refusal from a peer.
type StatusError struct {
	Status int64
	Msg    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("cluster: peer refused (status %d): %s", e.Status, e.Msg)
}

// call sends one request to peer and reads the response header; body, if non-nil,
// receives the rest of the stream.
func call(ctx context.Context, h *host.Host, peer wire.PeerID, req *request, body func(io.Reader) error) (*response, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s, err := h.OpenStream(ctx, peer, ProtocolID)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, s.Reset)
	defer stop()
	defer s.Close()
	b, err := drisl.Marshal(req)
	if err != nil {
		s.Reset()
		return nil, err
	}
	if err := wire.WriteMessage(s, b, MaxMessage); err != nil {
		s.Reset()
		return nil, err
	}
	_ = s.CloseWrite()
	br := bufio.NewReader(s)
	rb, err := wire.ReadMessage(br, MaxMessage)
	if err != nil {
		s.Reset()
		return nil, err
	}
	var res response
	if err := (drisl.DecOptions{MaxSize: MaxMessage, MaxDepth: 6}).Unmarshal(rb, &res); err != nil {
		s.Reset()
		return nil, err
	}
	if res.Status != StatusOK {
		return &res, &StatusError{res.Status, res.Error}
	}
	if len(res.Heads) > MaxHeads {
		return nil, errors.New("cluster: too many heads")
	}
	if body != nil {
		if err := body(io.LimitReader(br, MaxDiffBytes)); err != nil {
			s.Reset()
			return nil, err
		}
	}
	return &res, nil
}

// HeadsCall exchanges heads with peer and discards the answer (diagnostics: it reports
// whether the peer accepts this node on /nu/cluster/1).
func HeadsCall(ctx context.Context, h *host.Host, peer wire.PeerID, cluster cid.CID) error {
	_, err := call(ctx, h, peer, &request{Op: OpHeads, Cluster: cluster}, nil)
	return err
}
