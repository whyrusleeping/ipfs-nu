// Command nu-interop is an opt-in wire compatibility probe, not a Nu CLI command.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/wire"
)

type result struct {
	Address      string   `json:"address,omitempty"`
	Peer         string   `json:"peer,omitempty"`
	KeyType      int      `json:"keyType"`
	Agent        string   `json:"agent,omitempty"`
	Protocols    []string `json:"protocols,omitempty"`
	Connected    bool     `json:"connected"`
	Identified   bool     `json:"identified"`
	PingProtocol string   `json:"pingProtocol,omitempty"`
	// Milllis is the upstream legacy contract's intentional spelling.
	Handshake float64 `json:"handshakePlusOneRTTMillis"`
	Ping      float64 `json:"pingRTTMilllis"`
	Error     string  `json:"error,omitempty"`
}

func main() {
	mode := flag.String("mode", "probe", "probe or transport (unified-testing)")
	addr := flag.String("addr", "", "literal QUIC-v1 multiaddr including /p2p/<id>")
	ping := flag.String("ping", "libp2p", "libp2p or none")
	timeout := flag.Duration("timeout", 30*time.Second, "probe deadline")
	flag.Parse()
	var err error
	if *mode == "transport" {
		err = transport()
	} else if *mode == "probe" {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		r := result{Address: *addr}
		err = probe(ctx, *addr, *ping, &r)
		if err != nil {
			r.Error = err.Error()
		}
		_ = json.NewEncoder(os.Stdout).Encode(r)
	} else {
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type coreService interface {
	Identify(context.Context, wire.PeerID) (proto.PeerInfo, error)
	Ping(context.Context, wire.PeerID) (time.Duration, error)
	Close() error
}

func newHost(ip string) (*host.Host, coreService, error) {
	if os.Getenv("NU_INTEROP_DAEMON") == "1" {
		return daemonHost(ip)
	}
	key, err := wire.GenerateNodeKey()
	if err != nil {
		return nil, nil, err
	}
	h, err := host.New(host.Config{Key: key, Listen: []string{net.JoinHostPort(ip, "0")}})
	if err != nil {
		return nil, nil, err
	}
	s, err := proto.New(h, proto.Config{AgentVersion: "nu-interop/1"})
	if err != nil {
		_ = h.Close()
		return nil, nil, err
	}
	return h, s, nil
}

func probe(ctx context.Context, addr, ping string, r *result) error {
	ip := "0.0.0.0"
	if strings.HasPrefix(addr, "/ip6/") {
		ip = "::"
	}
	h, s, err := newHost(ip)
	if err != nil {
		return err
	}
	defer h.Close()
	defer s.Close()
	m, err := wire.ParseMultiaddr(addr, 2048)
	if err != nil {
		return err
	}
	start := time.Now()
	c, err := h.Dial(ctx, m)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	r.Connected, r.Peer, r.KeyType = true, c.PeerID().String(), int(c.PublicKey().Type())
	info, err := s.Identify(ctx, c.PeerID())
	if err != nil {
		return fmt.Errorf("identify: %w", err)
	}
	r.Identified, r.Agent, r.Protocols = true, info.Message.AgentVersion, info.Message.Protocols
	var rtt time.Duration
	switch ping {
	case "none":
	case "libp2p":
		r.PingProtocol = proto.PingID
		rtt, err = s.Ping(ctx, c.PeerID())
	default:
		return fmt.Errorf("unknown ping mode %q", ping)
	}
	r.Handshake, r.Ping = millis(time.Since(start)), millis(rtt)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func env(modern, legacy, fallback string) string {
	if v, ok := os.LookupEnv(modern); ok {
		return v
	}
	if v, ok := os.LookupEnv(legacy); ok {
		return v
	}
	return fallback
}

// redis-cli is supplied by the test-plan image. Keeping coordination out of the
// product avoids adding a Redis client dependency to the Nu module.
func redis(ctx context.Context, addr string, args ...string) (string, error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "redis-cli", append([]string{"--raw", "-h", h, "-p", p}, args...)...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("redis: %w: %s", err, b)
	}
	return strings.TrimSpace(string(b)), nil
}

func transport() error {
	if env("TRANSPORT", "transport", "") != "quic-v1" {
		return errors.New("only quic-v1 is supported")
	}
	secs, err := strconv.Atoi(env("TEST_TIMEOUT_SECS", "test_timeout_seconds", "180"))
	if err != nil || secs < 1 || secs > 3600 {
		return errors.New("invalid test timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	isDialer, err := strconv.ParseBool(env("IS_DIALER", "is_dialer", "false"))
	if err != nil {
		return err
	}
	modern := os.Getenv("TRANSPORT") != ""
	key := "listenerAddr"
	if modern {
		if os.Getenv("TEST_KEY") == "" {
			return errors.New("missing TEST_KEY")
		}
		key = os.Getenv("TEST_KEY") + "_listener_multiaddr"
	}
	raddr := env("REDIS_ADDR", "redis_addr", "redis:6379")
	h, s, err := newHost(env("LISTENER_IP", "ip", "0.0.0.0"))
	if err != nil {
		return err
	}
	defer h.Close()
	defer s.Close()
	if !isDialer {
		addr := h.Addrs()[0].String()
		if strings.Contains(addr, "/0.0.0.0/") {
			// Redis is on the same Docker network; use the route-selected local IP.
			c, err := (&net.Dialer{}).DialContext(ctx, "udp4", raddr)
			if err != nil {
				return err
			}
			ip := c.LocalAddr().(*net.UDPAddr).IP.String()
			_ = c.Close()
			addr = strings.Replace(addr, "/0.0.0.0/", "/"+ip+"/", 1)
		}
		addr += "/p2p/" + h.ID().String()
		if reply, err := redis(ctx, raddr, "RPUSH", key, addr); err != nil || reply != "1" {
			return fmt.Errorf("publish address: %q: %v", reply, err)
		}
		fmt.Fprintln(os.Stderr, "listening", addr)
		<-ctx.Done()
		return ctx.Err() // The suite stops the listener after the dialer exits.
	}
	value, err := redis(ctx, raddr, "BLPOP", key, strconv.Itoa(secs))
	if err != nil {
		return err
	}
	parts := strings.Split(value, "\n")
	if len(parts) != 2 || parts[0] != key {
		return fmt.Errorf("invalid BLPOP reply %q", value)
	}
	addr, err := wire.ParseMultiaddr(parts[1], 2048)
	if err != nil {
		return err
	}
	start := time.Now()
	c, err := h.Dial(ctx, addr)
	if err != nil {
		return err
	}
	rtt, err := s.Ping(ctx, c.PeerID())
	if err != nil {
		return err
	}
	handshake, ping := millis(time.Since(start)), millis(rtt)
	measurement := map[string]float64{"handshakePlusOneRTTMillis": handshake, "pingRTTMilllis": ping}
	if err = json.NewEncoder(os.Stdout).Encode(measurement); err != nil {
		return err
	}
	if modern {
		fmt.Printf("latency:\n  handshake_plus_one_rtt: %f\n  ping_rtt: %f\n  unit: ms\n", handshake, ping)
	}
	return nil
}
