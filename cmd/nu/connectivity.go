package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/repo"
)

func printConnectivity(w io.Writer, c *apiv0.ConnectivityStatus) {
	if c == nil {
		return
	}
	fmt.Fprintf(w, "reachability: %s\n", c.Reachability)
	if n := c.NAT; n != nil {
		fmt.Fprintf(w, "nat:        mapping %s, filtering %s (%s); %s\n", n.Mapping, n.Filtering, n.Source, n.Detail)
	}
	if c.Home != "" || c.Backup != "" {
		fmt.Fprintf(w, "relays:     home %s; backup %s\n", c.Home, c.Backup)
	}
	for _, r := range c.Relays {
		fmt.Fprintf(w, "relay:      %s %s", r.PeerID, r.Transport)
		if r.UDPError != "" {
			fmt.Fprintf(w, "; UDP attempt failed: %s", r.UDPError)
		}
		if r.LastError != "" {
			fmt.Fprintf(w, "; %s", r.LastError)
		}
		fmt.Fprintln(w)
	}
	for _, p := range c.Paths {
		fmt.Fprintf(w, "path:       %s %s", p.PeerID, p.Path)
		if p.Direct != "" {
			fmt.Fprintf(w, " %s", p.Direct)
		}
		if p.Relay != "" {
			fmt.Fprintf(w, " via %s (warm=%t)", p.Relay, p.RelayWarm)
		}
		if p.LastError != "" {
			fmt.Fprintf(w, "; %s", p.LastError)
		}
		fmt.Fprintln(w)
	}
	if c.NetworkChangeError != "" {
		fmt.Fprintf(w, "network change: %s\n", c.NetworkChangeError)
	}
}
func printDHT(w io.Writer, d *apiv0.DHTStatus) {
	if d == nil {
		return
	}
	fmt.Fprintf(w, "dht:        %s", d.Mode)
	if d.Mode == "off" {
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintf(w, ", %d routing peers", d.RoutingPeers)
	if !d.Bootstrapped.IsZero() {
		fmt.Fprintf(w, ", bootstrapped %s ago", time.Since(d.Bootstrapped).Round(time.Second))
	}
	if d.BootstrapErr != "" {
		fmt.Fprintf(w, " (%s)", d.BootstrapErr)
	}
	fmt.Fprintln(w)
	if !d.LastProvide.IsZero() {
		fmt.Fprintf(w, "provided:   %d keys %s ago", d.Provided, time.Since(d.LastProvide).Round(time.Second))
		if d.ProvideErr != "" {
			fmt.Fprintf(w, "; %s", d.ProvideErr)
		}
		fmt.Fprintln(w)
	}
}
func dhtChecks(d *apiv0.DHTStatus) []check {
	if d == nil || d.Mode == "off" {
		return nil
	}
	out := []check{{Name: "dht", Status: "ok", Detail: fmt.Sprintf("%s mode, %d routing peers", d.Mode, d.RoutingPeers)}}
	if d.BootstrapErr != "" || (d.RoutingPeers == 0 && !d.Bootstrapped.IsZero()) {
		out[0].Status, out[0].Detail = "warn", "bootstrap incomplete: "+d.BootstrapErr
		out[0].Action = "check DNS and UDP egress to the Amino bootstrap peers; content is still reachable through share links and LAN"
	}
	if d.ProvideErr != "" {
		out = append(out, check{Name: "dht provide", Status: "warn", Detail: d.ProvideErr,
			Action: "pinned content is found only through share links until this node has a public address (reachable directly or through a configured relay)"})
	}
	return out
}

// connectivityChecks turns the daemon's connectivity snapshot into doctor findings:
// UDP reachability, NAT profile, relays and reservations, port mapping, IPv6 and
// one line per session explaining why it is (not) direct, with its path history.
func connectivityChecks(c *apiv0.ConnectivityStatus) []check {
	if c == nil {
		return nil
	}
	checks := []check{{Name: "reachability", Status: "ok", Detail: c.Reachability}}
	port := strconv.Itoa(repo.DefaultSwarmPort)
	if u := c.UDP; u != nil {
		switch u.Outbound {
		case "ok":
			checks = append(checks, check{Name: "udp", Status: "ok", Detail: u.Detail})
		case "blocked":
			checks = append(checks, check{Name: "udp", Status: "fail", Detail: "UDP appears blocked: " + u.Detail,
				Action: "allow outbound UDP from this machine (QUIC to the relays' ports and to peers' port " + port + ") in the local firewall and the router; nothing can connect until UDP passes"})
		default:
			checks = append(checks, check{Name: "udp", Status: "warn", Detail: u.Detail, Action: "configure swarm.relays so the daemon can test UDP reachability"})
		}
	}
	if n := c.NAT; n != nil {
		detail := fmt.Sprintf("mapping %s, filtering %s (%s", n.Mapping, n.Filtering, n.Source)
		if n.Observers > 0 {
			detail += fmt.Sprintf(", %d observers", n.Observers)
		}
		detail += ")"
		if n.Detail != "" {
			detail += ": " + n.Detail
		}
		status, action := "ok", ""
		if n.Mapping == "endpoint-dependent" || strings.HasPrefix(n.Mapping, "address") {
			status = "warn"
			action = "direct paths need the other side to be public, IPv6 or router-mapped; enable port mapping (swarm.portmap_gateway) or expect relayed transfers"
		}
		checks = append(checks, check{Name: "nat", Status: status, Detail: detail, Action: action})
	}
	if c.Reachability != "disabled" && c.Reachability != "public" {
		status, action := "ok", ""
		if c.Home == "" || c.Backup == "" {
			status = "warn"
			action = "configure two available swarm.relays for NAT sharing; check UDP or declared WSS access and relay admission"
		}
		checks = append(checks, check{Name: "home relays", Status: status, Detail: fmt.Sprintf("home %s; backup %s", c.Home, c.Backup), Action: action})
	}
	for _, r := range c.Relays {
		name := "relay " + r.PeerID
		if r.Role != "" {
			name = "relay " + r.Role + " " + r.PeerID
		}
		if r.Transport == "wss" && r.UDPError != "" {
			checks = append(checks, check{Name: "relay fallback " + r.PeerID, Status: "ok", Detail: "WSS selected; UDP attempt failed: " + r.UDPError})
		}
		if r.LastError != "" {
			checks = append(checks, check{Name: name, Status: "warn", Detail: r.LastError, Action: "check the relay is online and allows this node"})
			continue
		}
		detail := fmt.Sprintf("rtt %.1f ms", r.RTTMillis)
		if r.Reserved {
			detail += fmt.Sprintf(", reservation held for %s, renews before %s", time.Since(r.HeldSince).Round(time.Second), r.Expires.Local().Format("15:04:05"))
		} else {
			detail += ", no reservation"
		}
		checks = append(checks, check{Name: name, Status: "ok", Detail: detail})
	}
	if pm := c.PortMapping; pm != nil {
		switch {
		case !pm.Enabled:
			checks = append(checks, check{Name: "port mapping", Status: "ok", Detail: "not configured", Action: "set swarm.portmap_gateway to your router's address to request a UPnP/NAT-PMP/PCP mapping"})
		case pm.External != "":
			checks = append(checks, check{Name: "port mapping", Status: "ok", Detail: fmt.Sprintf("%s mapped %s until %s", pm.Protocol, pm.External, pm.Expires.Local().Format("15:04:05"))})
		default:
			checks = append(checks, check{Name: "port mapping", Status: "warn", Detail: "no lease: " + pm.Error, Action: "enable UPnP/NAT-PMP/PCP on the router, or accept relayed transfers from non-EIM peers"})
		}
	}
	if v6 := c.IPv6; v6 != nil {
		switch {
		case v6.Listening:
			checks = append(checks, check{Name: "ipv6", Status: "ok", Detail: "listening on a global IPv6 address: " + strings.Join(v6.Global, " ")})
		case len(v6.Global) > 0:
			checks = append(checks, check{Name: "ipv6", Status: "warn", Detail: "global IPv6 address present but swarm.listen has no /ip6 entry: " + strings.Join(v6.Global, " "),
				Action: "add /ip6/::/udp/" + port + "/quic-v1 to swarm.listen so IPv6 peers connect directly"})
		default:
			checks = append(checks, check{Name: "ipv6", Status: "ok", Detail: "no global IPv6 address on this machine"})
		}
	}
	for _, p := range c.Paths {
		detail := p.Path
		if p.Direct != "" {
			detail += " " + p.Direct
		}
		if p.Relay != "" {
			detail += " via " + p.Relay
		}
		if p.TimeToDirectMillis > 0 {
			detail += fmt.Sprintf(", direct after %.0f ms", p.TimeToDirectMillis)
		}
		status := "ok"
		if p.Why != "" {
			status = "warn"
			detail += "; " + p.Why
		}
		if len(p.History) > 0 {
			var lines []string
			for _, e := range p.History {
				lines = append(lines, e.Time.Local().Format("15:04:05")+" "+e.Event)
			}
			detail += "\n       " + strings.Join(lines, "\n       ")
		}
		checks = append(checks, check{Name: "session " + p.PeerID, Status: status, Detail: detail})
	}
	if c.NetworkChangeError != "" {
		checks = append(checks, check{Name: "network change", Status: "warn", Detail: c.NetworkChangeError, Action: "restart the daemon if its bound address disappeared; physical socket replacement is not supported"})
	}
	return checks
}
