//go:build linux && natlab

package natlab

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func (n *Node) ReserveCircuit(relay string) error {
	_, err := n.call(control{Op: "reserve-circuit", Address: relay})
	return err
}
func (n *Node) MaintainCircuit(relay string) error {
	_, err := n.call(control{Op: "maintain-circuit", Address: relay})
	return err
}
func (n *Node) EchoCircuit(relay, target, payload string) (string, error) {
	r, err := n.call(control{Op: "echo-circuit", Address: relay, Peer: target, Candidate: payload})
	return r.Observed, err
}
func (n *Node) IdleCircuit(relay, target string) error {
	_, err := n.call(control{Op: "idle-circuit", Address: relay, Peer: target})
	return err
}

type circuitHelper struct {
	h      *host.Host
	client *circuit.Client
	server *circuit.Server
}

func startCircuits(h *host.Host, key *wire.NodeKey, cfg HostConfig) (*circuitHelper, error) {
	helper := &circuitHelper{h: h}
	if cfg.CircuitServer {
		limits := circuit.Limit{}
		if cfg.CircuitDuration != 0 {
			limits = circuit.Limit{Duration: cfg.CircuitDuration, Data: 128 << 10}
		}
		server, err := circuit.NewServer(h, key, circuit.ServerConfig{ReservationTTL: cfg.CircuitTTL, Limited: limits})
		if err != nil {
			return nil, err
		}
		helper.server = server
	}
	if cfg.CircuitClient {
		client, err := circuit.NewClient(h, circuit.ClientConfig{})
		if err != nil {
			helper.close()
			return nil, err
		}
		helper.client = client
		// Echo raw circuit bytes only: this is not a content handler or an emulated
		// secure libp2p connection. The future qpath consumer supplies inner security.
		go func() {
			for {
				stream, err := client.Accept(h.Context())
				if err != nil {
					return
				}
				go func() { defer stream.Close(); _, _ = io.Copy(stream, stream) }()
			}
		}()
	}
	return helper, nil
}
func (h *circuitHelper) close() {
	if h.client != nil {
		h.client.Close()
	}
	if h.server != nil {
		h.server.Close()
	}
}
func (h *circuitHelper) handle(ctx context.Context, c control, r *reply) error {
	if h.client == nil {
		return fmt.Errorf("circuit client not enabled")
	}
	addr, err := wire.ParseMultiaddr(c.Address, 1024)
	if err != nil {
		return err
	}
	conn, err := h.h.Dial(ctx, addr)
	if err != nil {
		return err
	}
	switch c.Op {
	case "reserve-circuit":
		_, err = h.client.Reserve(ctx, conn.PeerID())
		return err
	case "maintain-circuit":
		renewalCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		updates := 0
		err = h.client.Maintain(renewalCtx, conn.PeerID(), func(circuit.Reservation) {
			updates++
			if updates == 2 {
				cancel()
			}
		})
		if updates == 2 {
			return nil
		}
		return err
	}
	peer, err := wire.ParsePeerID(c.Peer)
	if err != nil {
		return err
	}
	stream, err := h.client.Connect(ctx, conn.PeerID(), peer)
	if err != nil {
		return err
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(2 * time.Second))
	if c.Op == "idle-circuit" {
		var b [1]byte
		_, err = stream.Read(b[:])
		return err
	}
	if len(c.Candidate) > 4096 {
		return wire.ErrLimit
	}
	if _, err = stream.Write([]byte(c.Candidate)); err != nil {
		return err
	}
	if err = stream.CloseWrite(); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(stream, 4097))
	if len(data) > 4096 {
		return wire.ErrLimit
	}
	r.Observed = string(data)
	return err
}
