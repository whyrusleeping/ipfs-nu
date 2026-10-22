package daemon

import (
	"strings"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// checkIsolated is the isolated-mode egress check (docs/cluster.md "Network modes",
// Egress safety): at startup a member of an isolated cluster refuses configuration that
// names an endpoint outside the cluster: relays, swarm peers and probe grants that are not
// members, a port-mapping gateway, a Jetstream hint source, and the public listener
// (startPublic refuses that for private clusters too). Compiled-in defaults are already
// inactive in isolated mode (the swarm runs Isolated, mDNS uses the cluster service).
func checkIsolated(cfg *repo.Config, c *cluster.Cluster) error {
	if c == nil || c.Mode() != cluster.ModeIsolated {
		return nil
	}
	v := c.View()
	var bad []string
	member := func(peer string) bool {
		id, err := wire.ParsePeerID(peer)
		return err == nil && v.IsMember(id)
	}
	if len(cfg.Swarm.Relays) > 0 {
		bad = append(bad, "swarm.relays")
	}
	for _, p := range cfg.Swarm.Peers {
		_, id, _ := strings.Cut(p, "/p2p/")
		if !member(id) {
			bad = append(bad, "swarm.peers ("+p+")")
		}
	}
	for _, p := range cfg.Swarm.ProbeAllow {
		id, _, _ := strings.Cut(p, "@")
		if !member(id) {
			bad = append(bad, "swarm.probe_allow ("+p+")")
		}
	}
	if cfg.Swarm.PortmapGateway != "" {
		bad = append(bad, "swarm.portmap_gateway")
	}
	if cfg.Follow != nil && cfg.Follow.Jetstream != "" {
		bad = append(bad, "follow.jetstream")
	}
	if cfg.Public != nil && cfg.Public.Listen != "" {
		bad = append(bad, "public.listen")
	}
	if len(bad) == 0 {
		return nil
	}
	return apiv0.Errorf(apiv0.CodeInvalidArgument, "this node is a member of an isolated cluster, which has no egress; config names endpoints outside the cluster: %s", strings.Join(bad, ", ")).
		WithAction("remove these keys from config.toml (`" + nu.Name + " config set <key> ...`); swarm.peers and probe grants may name cluster members only")
}
