package main

import (
	"fmt"
	"io"

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
