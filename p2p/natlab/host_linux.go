//go:build linux && natlab

package natlab

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/autonat"
	"github.com/whyrusleeping/nu/p2p/wire"
	"golang.org/x/sys/unix"
)

const helperEnv = "NU_NATLAB_HELPER"

var ErrTimeout = errors.New("natlab: UDP receive timed out")

// HostConfig starts one real Nu QUIC host per helper process. Echo addresses
// provide tiny UDP endpoint observers; ProbeListen reserves a persistent source
// socket for mapping smoke tests. All traffic stays in the isolated topology.
type HostConfig struct {
	Listen        string
	AutoNATServer bool
	Echo          []string
	ProbeListen   string
}
type Node struct {
	ID      string
	Address string
	enc     *json.Encoder
	dec     *json.Decoder
}
type control struct {
	Op, Address, Peer, Candidate string
	Config                       HostConfig
}
type reply struct {
	ID, Address, Observed, Error string
	Reachability                 autonat.Reachability
	Timeout                      bool
}

// StartHost re-execs the current executable. Importing natlab installs the
// helper entrypoint; callers do not need a TestMain or separate binary.
func (n *Namespace) StartHost(cfg HostConfig) (*Node, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	in, out, err := n.Start("env", helperEnv+"=host", exe)
	if err != nil {
		return nil, err
	}
	node := &Node{enc: json.NewEncoder(in), dec: json.NewDecoder(out)}
	r, err := node.call(control{Op: "start", Config: cfg})
	if err != nil {
		return nil, err
	}
	node.ID, node.Address = r.ID, r.Address
	return node, nil
}
func (n *Node) call(c control) (reply, error) {
	var r reply
	if err := n.enc.Encode(c); err != nil {
		return r, err
	}
	if err := n.dec.Decode(&r); err != nil {
		return r, err
	}
	if r.Error != "" {
		if r.Timeout {
			return r, fmt.Errorf("%w: %s", ErrTimeout, r.Error)
		}
		return r, errors.New(r.Error)
	}
	return r, nil
}
func (n *Node) Dial(address string) error {
	_, err := n.call(control{Op: "dial", Address: address})
	return err
}
func (n *Node) Remote(peer string) (string, error) {
	r, err := n.call(control{Op: "remote", Peer: peer})
	return r.Observed, err
}
func (n *Node) Check(observer, candidate string) (autonat.Reachability, error) {
	r, err := n.call(control{Op: "check", Address: observer, Candidate: candidate})
	return r.Reachability, err
}
func (n *Node) Probe(destination string) (string, error) {
	r, err := n.call(control{Op: "probe", Address: destination})
	return r.Observed, err
}

// SendFrom emits an unsolicited packet from one of this node's echo sockets.
func (n *Node) SendFrom(source, destination string) error {
	_, err := n.call(control{Op: "send", Peer: source, Address: destination})
	return err
}
func (n *Node) Receive() (string, error) {
	r, err := n.call(control{Op: "receive"})
	return r.Observed, err
}

func init() {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	if os.Getenv(isolatedEnv) != "1" {
		fmt.Fprintln(os.Stderr, "natlab helper requires isolated namespace")
		os.Exit(2)
	}
	var err error
	switch mode {
	case "host":
		err = serveHost(os.Stdin, os.Stdout)
	case "flush":
		err = flushConntrack()
	default:
		err = fmt.Errorf("unknown natlab helper %q", mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
func serveHost(in io.Reader, out io.Writer) error {
	dec, enc := json.NewDecoder(in), json.NewEncoder(out)
	var first control
	if err := dec.Decode(&first); err != nil {
		return err
	}
	cfg := first.Config
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	key, err := wire.GenerateNodeKey()
	if err != nil {
		return err
	}
	h, err := host.New(host.Config{Key: key, Listen: []string{cfg.Listen}})
	if err != nil {
		return err
	}
	defer h.Close()
	svc, err := autonat.New(h, autonat.Config{Server: cfg.AutoNATServer, AllowPrivateAddresses: true, Timeout: 2 * time.Second, DialTimeout: 300 * time.Millisecond})
	if err != nil {
		return err
	}
	defer svc.Close()
	var probe net.PacketConn
	if cfg.ProbeListen != "" {
		probe, err = net.ListenPacket("udp4", cfg.ProbeListen)
		if err != nil {
			return err
		}
		defer probe.Close()
	}
	echoes := make(map[string]net.PacketConn)
	for _, addr := range cfg.Echo {
		pc, err := net.ListenPacket("udp4", addr)
		if err != nil {
			return err
		}
		defer pc.Close()
		echoes[addr] = pc
		go func() {
			buf := make([]byte, 256)
			for {
				size, from, err := pc.ReadFrom(buf)
				if err != nil {
					return
				}
				if string(buf[:size]) == "endpoint" {
					_, _ = pc.WriteTo([]byte(from.String()), from)
				}
			}
		}()
	}
	if err = enc.Encode(reply{ID: h.ID().String(), Address: h.Addrs()[0].String() + "/p2p/" + h.ID().String()}); err != nil {
		return err
	}
	for {
		var c control
		if err := dec.Decode(&c); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		r := reply{}
		err := func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			switch c.Op {
			case "dial", "check":
				a, err := wire.ParseMultiaddr(c.Address, 1024)
				if err != nil {
					return err
				}
				conn, err := h.Dial(ctx, a)
				if err != nil {
					return err
				}
				if c.Op == "dial" {
					return nil
				}
				candidate, err := wire.ParseMultiaddr(c.Candidate, 1024)
				if err != nil {
					return err
				}
				result, err := svc.Check(ctx, conn.PeerID(), []autonat.Request{{Address: candidate, SendDialData: true}})
				r.Reachability = result.Reachability
				return err
			case "remote":
				id, err := wire.ParsePeerID(c.Peer)
				if err != nil {
					return err
				}
				changes, stop := h.WatchChanges()
				defer stop()
				for {
					if conn, ok := h.Connection(id); ok {
						r.Observed = conn.RemoteAddr().String()
						return nil
					}
					select {
					case <-changes:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			case "send":
				pc := echoes[c.Peer]
				if pc == nil {
					return fmt.Errorf("unknown echo socket")
				}
				remote, err := net.ResolveUDPAddr("udp4", c.Address)
				if err != nil {
					return err
				}
				_, err = pc.WriteTo([]byte("unsolicited"), remote)
				return err
			case "receive":
				if probe == nil {
					return fmt.Errorf("no probe socket")
				}
				_ = probe.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
				buf := make([]byte, 256)
				size, _, err := probe.ReadFrom(buf)
				r.Observed = string(buf[:size])
				return err
			case "probe":
				if probe == nil {
					return fmt.Errorf("no probe socket")
				}
				remote, err := net.ResolveUDPAddr("udp4", c.Address)
				if err != nil {
					return err
				}
				_ = probe.SetDeadline(time.Now().Add(300 * time.Millisecond))
				if _, err = probe.WriteTo([]byte("endpoint"), remote); err != nil {
					return err
				}
				buf := make([]byte, 256)
				size, _, err := probe.ReadFrom(buf)
				r.Observed = string(buf[:size])
				return err
			default:
				return fmt.Errorf("unknown host op %q", c.Op)
			}
		}()
		if err != nil {
			r.Error = err.Error()
			var timeout net.Error
			r.Timeout = errors.As(err, &timeout) && timeout.Timeout()
		}
		if err = enc.Encode(r); err != nil {
			return err
		}
	}
}
func (n *Namespace) flushConntrack() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	_, err = n.Run("env", helperEnv+"=flush", exe)
	return err
}

// CT_DELETE without a tuple flushes the namespace's IPv4 conntrack table.
// A direct netlink request avoids making conntrack-tools a lab dependency.
func flushConntrack() error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_NETFILTER)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 2}); err != nil {
		return err
	}
	msg := make([]byte, 20)
	binary.NativeEndian.PutUint32(msg, 20)
	binary.NativeEndian.PutUint16(msg[4:], 1<<8|2) // NFNL_SUBSYS_CTNETLINK, IPCTNL_MSG_CT_DELETE
	binary.NativeEndian.PutUint16(msg[6:], unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	binary.NativeEndian.PutUint32(msg[8:], 1)
	msg[16] = unix.AF_INET // nfgenmsg
	if err = unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	size, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	if size < 20 || binary.NativeEndian.Uint16(buf[4:]) != unix.NLMSG_ERROR {
		return fmt.Errorf("unexpected conntrack netlink reply")
	}
	code := int32(binary.NativeEndian.Uint32(buf[16:]))
	if code != 0 {
		return unix.Errno(-code)
	}
	return nil
}
