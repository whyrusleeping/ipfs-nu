//go:build linux && natlab

package qpath_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/proto/qpath"
)

// Override with NU_NATLAB_DELAY / NU_NATLAB_LOSS (tc netem syntax) when exploring.
var lossDelay, lossRate = envOr("NU_NATLAB_DELAY", "10ms"), envOr("NU_NATLAB_LOSS", "2%")

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type lossSample struct {
	label                string
	median, p95, p99     time.Duration // idle pings
	lmedian, lp95, lp99  time.Duration // pings while a bulk echo loads the same inner connection
	bulk                 time.Duration
	bulkBytes            int
	pings                int
	datagramA, datagramB uint64
}

func (s lossSample) String() string {
	return fmt.Sprintf("%-8s idle ping(n=%d) median=%v p95=%v p99=%v | loaded ping median=%v p95=%v p99=%v | %d KiB echo %v (%.0f KiB/s) | datagrams A=%d B=%d",
		s.label, s.pings, s.median.Round(100*time.Microsecond), s.p95.Round(100*time.Microsecond), s.p99.Round(100*time.Microsecond),
		s.lmedian.Round(100*time.Microsecond), s.lp95.Round(100*time.Microsecond), s.lp99.Round(100*time.Microsecond),
		s.bulkBytes>>10, s.bulk.Round(time.Millisecond), float64(s.bulkBytes>>10)/s.bulk.Seconds(), s.datagramA, s.datagramB)
}

func pingLatencies(t *testing.T, s *host.Stream, label string, pings int) (median, p95, p99 time.Duration) {
	t.Helper()
	lat := make([]time.Duration, 0, pings)
	small := []byte("ping-32-bytes-of-inner-payload!!")
	out := make([]byte, len(small))
	for range pings {
		start := time.Now()
		if _, err := s.Write(small); err != nil {
			t.Fatal(label, err)
		}
		if _, err := io.ReadFull(s, out); err != nil {
			t.Fatal(label, err)
		}
		lat = append(lat, time.Since(start))
		if os.Getenv("NU_NATLAB_TRACE") != "" {
			t.Logf("%s ping %v", label, time.Since(start))
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return lat[len(lat)/2], lat[len(lat)*95/100], lat[len(lat)*99/100]
}
func echoBulk(t *testing.T, s *host.Stream, label string, bulk int) time.Duration {
	t.Helper()
	payload := bytes.Repeat([]byte{0x5a}, bulk)
	start := time.Now()
	go func() { _, _ = s.Write(payload) }()
	echo := make([]byte, bulk)
	if _, err := io.ReadFull(s, echo); err != nil {
		t.Fatal(label, err)
	}
	return time.Since(start)
}

// measure: idle pings, a bulk echo, then pings while a second stream keeps a
// bulk echo running (the head-of-line case: lost outer packets carrying bulk
// frames stall every later frame on a circuit stream, not on datagrams).
func measure(t *testing.T, x *setup, s, load *host.Stream, label string, pings, bulk int) lossSample {
	t.Helper()
	before := [2]uint64{x.ma.Stats().DatagramSent, x.mb.Stats().DatagramSent}
	phase := time.Now()
	defer func() { t.Logf("%s phase took %v", label, time.Since(phase)) }()
	r := lossSample{label: label, pings: pings, bulkBytes: bulk}
	r.median, r.p95, r.p99 = pingLatencies(t, s, label, pings)
	r.bulk = echoBulk(t, s, label, bulk)
	stop := make(chan struct{})
	loaded := make(chan struct{})
	go func() {
		defer close(loaded)
		block := bytes.Repeat([]byte{0xa5}, 32<<10)
		echo := make([]byte, len(block))
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := load.Write(block); err != nil {
				return
			}
			if _, err := io.ReadFull(load, echo); err != nil {
				return
			}
		}
	}()
	r.lmedian, r.lp95, r.lp99 = pingLatencies(t, s, label+"/loaded", pings)
	close(stop)
	<-loaded
	r.datagramA, r.datagramB = x.ma.Stats().DatagramSent-before[0], x.mb.Stats().DatagramSent-before[1]
	t.Log(r)
	return r
}

// Two NAT'd peers, one public Nu relay; shape installs the impairment on every
// router interface (both directions). Stream relaying is measured first, then
// the same inner connection is upgraded to the datagram extension and measured
// again, then detached and measured once more as a warm-up control. Numbers are
// logged and recorded in docs/validation.md.
func runDatagramLab(t *testing.T, label string, shape func(router *natlab.Namespace, dev string) error) (stream, datagram, control lossSample) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	lab, err := natlab.New(ctx)
	check(t, err)
	t.Cleanup(lab.Close)
	public, err := lab.Namespace("public")
	check(t, err)
	wan, err := public.Segment("wan", "198.18.0.1/24")
	check(t, err)
	var peers, routers [2]*natlab.Namespace
	for i := range peers {
		routers[i], err = lab.Namespace("router")
		check(t, err)
		peers[i], err = lab.Namespace("peer")
		check(t, err)
		external, gateway, peerCIDR, inside := "198.18.0.20", "10.1.0.1", "10.1.0.2/24", "10.1.0.2:4000"
		if i == 1 {
			external, gateway, peerCIDR, inside = "198.18.0.30", "10.2.0.1", "10.2.0.2/24", "10.2.0.2:4000"
		}
		check(t, wan.Attach(routers[i], "wan", external+"/24", ""))
		lan, err := routers[i].Segment("lan", gateway+"/24")
		check(t, err)
		check(t, lan.Attach(peers[i], "eth0", peerCIDR, gateway))
		_, err = routers[i].NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr(external), Mapping: natlab.EndpointIndependent, Filtering: natlab.FilterAddressPortDependent, Inside: []netip.AddrPort{netip.MustParseAddrPort(inside)}, Destinations: []netip.AddrPort{netip.MustParseAddrPort("198.18.0.1:4000")}})
		check(t, err)
		for _, dev := range []string{"wan", "lan"} {
			check(t, shape(routers[i], dev))
		}
	}
	x := makeSetup(t, ctx, namespaceUDP(t, peers[0], "10.1.0.2:4000"), namespaceUDP(t, peers[1], "10.2.0.2:4000"), namespaceUDP(t, public, "198.18.0.1:4000"))
	x.acceptDatagram(t, ctx)
	s := x.stream(t, ctx)
	load, err := x.sessions.OpenStream(ctx, x.b.ID(), "/nu/qpath-test/1")
	check(t, err)
	t.Cleanup(func() { load.Close() })
	roundtrip(t, s, 2)
	const pings, bulk = 50, 256 << 10
	stream = measure(t, x, s, load, "stream", pings, bulk)
	_, err = qpath.UpgradeDatagram(ctx, x.ac, x.relayA, x.r.ID(), x.b.ID())
	check(t, err)
	waitFor(t, "B did not attach the datagram leg", func() bool { set, _ := x.mb.Paths(x.a.ID()); return set.Datagram })
	datagram = measure(t, x, s, load, "datagram", pings, bulk)
	x.relayA.DetachPacketLeg()
	waitFor(t, "B kept the datagram leg", func() bool { set, _ := x.mb.Paths(x.a.ID()); return !set.Datagram })
	control = measure(t, x, s, load, "stream2", pings, bulk)
	if datagram.datagramA == 0 || datagram.datagramB == 0 || stream.datagramA != 0 || control.datagramA != 0 {
		t.Fatal("phase attribution wrong", stream, datagram, control)
	}
	t.Logf("%s: loaded-ping p95 datagram %v vs stream %v / %v; idle p95 %v vs %v / %v; bulk %v vs %v / %v",
		label, datagram.lp95, stream.lp95, control.lp95, datagram.p95, stream.p95, control.p95, datagram.bulk, stream.bulk, control.bulk)
	return stream, datagram, control
}

// Random loss: 2% each direction on both legs, 10 ms one-way per interface
// (~20 ms RTT per leg, ~40 ms end to end). Recorded, not asserted: with stock
// quic-go hop-by-hop outer retransmission recovers a random loss in one leg
// RTT while end-to-end inner recovery needs the full path RTT, so datagram
// mode is NOT expected to win here (see docs/validation.md).
func TestDatagramRelayLossNATLab(t *testing.T) {
	if !natlab.Isolate(t) {
		return
	}
	runDatagramLab(t, lossRate+" loss + "+lossDelay+"/hop", func(r *natlab.Namespace, dev string) error {
		_, err := r.Run("tc", "qdisc", "add", "dev", dev, "root", "netem", "delay", lossDelay, "loss", lossRate)
		return err
	})
}

// Congestion: a 4 Mbit/s bottleneck with a deep (300 ms) queue on every
// interface and 10 ms one-way delay, no random loss. Circuit streams let the
// relay buffer megabytes (outer stream/connection windows) ahead of every later
// inner packet; datagram legs queue at most 32+64 packets, so loaded pings must
// stay markedly lower in datagram mode.
func TestDatagramRelayBottleneckNATLab(t *testing.T) {
	if !natlab.Isolate(t) {
		return
	}
	_, datagram, control := runDatagramLab(t, "4 Mbit/s bottleneck, 300 ms queue, 10 ms/hop", func(r *natlab.Namespace, dev string) error {
		if _, err := r.Run("tc", "qdisc", "add", "dev", dev, "root", "handle", "1:", "netem", "delay", lossDelay); err != nil {
			return err
		}
		_, err := r.Run("tc", "qdisc", "add", "dev", dev, "parent", "1:", "handle", "2:", "tbf", "rate", "4mbit", "burst", "16kb", "latency", "300ms")
		return err
	})
	if datagram.lp95 >= control.lp95 {
		t.Errorf("datagram loaded-ping p95 %v not below stream control %v under congestion", datagram.lp95, control.lp95)
	}
}
