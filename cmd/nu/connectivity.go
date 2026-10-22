package main

import (
	"fmt"
	"io"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
)

func printConnectivity(w io.Writer, c *apiv0.ConnectivityStatus) {
	if c == nil {
		return
	}
	fmt.Fprintf(w, "reachability: %s\n", c.Reachability)
	if c.Home != "" || c.Backup != "" {
		fmt.Fprintf(w, "relays:     home %s; backup %s\n", c.Home, c.Backup)
	}
	for _, p := range c.Paths {
		fmt.Fprintf(w, "path:       %s %s", p.PeerID, p.Path)
		if p.Direct != "" {
			fmt.Fprintf(w, " %s", p.Direct)
		}
		if p.Relay != "" {
			fmt.Fprintf(w, " via %s (warm=%t", p.Relay, p.RelayWarm)
			if p.RelayMode != "" {
				fmt.Fprintf(w, ", %s", p.RelayMode) // 0019: stream or datagram leg
			}
			fmt.Fprint(w, ")")
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
func connectivityChecks(c *apiv0.ConnectivityStatus) []check {
	if c == nil {
		return nil
	}
	checks := []check{{Name: "reachability", Status: "ok", Detail: c.Reachability}}
	if c.Reachability != "disabled" && c.Reachability != "public" {
		status, action := "ok", ""
		if c.Home == "" || c.Backup == "" {
			status = "warn"
			action = "configure two available swarm.relays for NAT sharing; check UDP access and relay admission"
		}
		checks = append(checks, check{Name: "home relays", Status: status, Detail: fmt.Sprintf("home %s; backup %s", c.Home, c.Backup), Action: action})
	}
	for _, r := range c.Relays {
		if r.LastError != "" {
			checks = append(checks, check{Name: "relay " + r.PeerID, Status: "warn", Detail: r.LastError, Action: "check the relay is online and allows this node"})
		}
	}
	if c.NetworkChangeError != "" {
		checks = append(checks, check{Name: "network change", Status: "warn", Detail: c.NetworkChangeError, Action: "restart the daemon if its bound address disappeared; physical socket replacement is not supported"})
	}
	return checks
}
