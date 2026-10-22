package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// FreshFetcher is a throwaway network client with its own empty store and new key.
type FreshFetcher interface {
	ID() wire.PeerID
	Fetch(ctx context.Context, root cid.CID, hints []link.Provider, progress func(done, total int64)) error
	Store() *store.Store
	Close() error
}

// FreshClient starts a FreshFetcher whose store lives under dir; node/swarm registers it
// (nil in builds without networking).
var FreshClient func(ctx context.Context, dir string) (FreshFetcher, error)

// cutoverNotes are the standing cutover rules (docs/migration.md), repeated in every report.
var cutoverNotes = []string{
	"Nu never stops Kubo, removes pins, deletes blocks or transfers identity/cluster authority; back up the Kubo repo and keys separately",
	"redirect readers and writers deliberately after testing scripts against the " + nu.Name + " CLI/API: there is no Kubo API emulation",
}

// ImportCutover checks the gates before an imported dataset may replace its IPFS source:
// the head version carries a signed `imported` reference to a manifest that matches it, a
// fresh client retrieves the head over the network, and the exact dataset policy is ready.
func (n *Local) ImportCutover(ctx context.Context, req apiv0.ImportCutoverRequest) (*apiv0.ImportCutoverResult, error) {
	scope := req.Scope
	if scope == "" {
		scope = ScopeLocal
	}
	c, d, err := n.importDataset(scope, req.Dataset)
	if err != nil {
		return nil, err
	}
	out := &apiv0.ImportCutoverResult{Dataset: req.Dataset, Scope: scope, Notes: append([]string(nil), cutoverNotes...)}
	hs := histFor(c, req.Dataset, d)
	if hs.head == "" {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "dataset %q has no head yet", req.Dataset).
			WithAction("import into it with `" + nu.Name + " import ipfs <cid> --dataset " + req.Dataset + "`")
	}
	out.Target, out.Version = hs.head, hs.headTID
	target, err := cid.Parse(hs.head)
	if err != nil {
		return nil, err
	}
	for _, e := range hs.hist {
		if e.TID == hs.headTID && e.Imported != nil {
			out.Imported = &apiv0.VersionImported{Source: e.Imported.Source, Manifest: e.Imported.Manifest}
		}
	}
	if out.Imported == nil {
		out.Missing = append(out.Missing, "the head version has no signed `imported` reference: import with --dataset so the version records its provenance manifest")
	} else if why := n.checkManifest(out.Imported, hs.head); why != "" {
		out.Missing = append(out.Missing, why)
	}
	out.Retrieval = n.freshRetrieval(ctx, c, target)
	if !out.Retrieval.OK {
		out.Missing = append(out.Missing, "fresh-client retrieval failed: "+out.Retrieval.Error)
	}
	res, err := n.DatasetWait(ctx, scope, req.Dataset, apiv0.DatasetWaitRequest{Target: hs.head, Condition: apiv0.WaitReady, Timeout: req.Timeout})
	if res == nil {
		return nil, err
	}
	out.Readiness = res
	if !res.Satisfied {
		why := strings.Join(res.Unmet, "; ")
		if why == "" && err != nil {
			why = err.Error()
		}
		out.Missing = append(out.Missing, "the dataset policy is not ready: "+why)
	}
	if res.Required <= 1 {
		out.Notes = append(out.Notes, "the dataset policy keeps a single copy: retiring the IPFS source leaves no second replica (raise --replicas first if that matters)")
	}
	out.Recommend = len(out.Missing) == 0
	return out, nil
}

// checkManifest confirms the referenced manifest is held and maps source to target.
func (n *Local) checkManifest(im *apiv0.VersionImported, target string) string {
	mc, err := cid.Parse(im.Manifest)
	if err != nil {
		return "the imported reference names an invalid manifest CID"
	}
	b, err := n.Store.Get(mc)
	if err != nil {
		return fmt.Sprintf("the provenance manifest %s is not held here: %v", im.Manifest, err)
	}
	v, err := drisl.Decode(b)
	m, ok := v.(map[string]any)
	if err != nil || !ok || m["$type"] != "io.ipfs.nu.import" {
		return "the provenance manifest is not an io.ipfs.nu.import record"
	}
	root, _ := m["nu_root"].(cid.CID)
	if root.String() != target || "ipfs://"+fmt.Sprint(m["ipfs_root"]) != im.Source || m["verified"] != true {
		return "the provenance manifest does not describe a verified import of this source to this head"
	}
	return ""
}

// freshRetrieval fetches target with a new client from this node and the connected
// members of c, over the network: the client never reads this node's store directly.
func (n *Local) freshRetrieval(ctx context.Context, c *cluster.Cluster, target cid.CID) *apiv0.CutoverRetrieval {
	out := &apiv0.CutoverRetrieval{}
	if FreshClient == nil || n.Net == nil {
		out.Error = "this node has no network to validate from; run the check on a node with networking (`" + nu.Name + " daemon`)"
		return out
	}
	if c.Mode() == cluster.ModePrivate || c.Mode() == cluster.ModeIsolated {
		out.Error = "a fresh client is not a member of this " + c.Mode() + " cluster and is refused by its connection gate; validate with `" + nu.Name + " get` from a new member instead"
		return out
	}
	dir, err := os.MkdirTemp("", "nu-cutover-*")
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer os.RemoveAll(dir)
	fc, err := FreshClient(ctx, dir)
	if err != nil {
		out.Error = "cannot start a fresh client: " + err.Error()
		return out
	}
	defer fc.Close()
	out.Client = fc.ID().String()
	hints := []link.Provider{{Peer: n.PeerID(), Addrs: n.Addrs()}}
	if c.Mode() != cluster.ModeLocal {
		if sw := n.clusterNetStack(); sw != nil {
			v := c.View()
			for _, conn := range sw.Host().Connections() {
				id := conn.PeerID()
				ua, ok := conn.RemoteAddr().(*net.UDPAddr)
				if !ok || !v.IsMember(id) {
					continue
				}
				fam := "ip4"
				if ua.IP.To4() == nil {
					fam = "ip6"
				}
				if a, err := wire.ParseMultiaddr(fmt.Sprintf("/%s/%s/udp/%d/quic-v1", fam, ua.IP, ua.Port), 1024); err == nil {
					hints = append(hints, link.Provider{Peer: id, Addrs: []wire.Multiaddr{a}})
				}
			}
		}
	}
	for _, h := range hints {
		out.Providers = append(out.Providers, h.Peer.String())
	}
	if err := fc.Fetch(ctx, target, hints, nil); err != nil {
		out.Error = err.Error()
		return out
	}
	b, err := fc.Store().Get(target)
	if err == nil {
		if h, herr := tree.DecodeHeader(b); herr == nil {
			out.Files, out.Bytes = int64(h.Files), int64(h.Bytes)
		} else if f, ferr := tree.DecodeFileNode(b); ferr == nil {
			out.Files, out.Bytes = 1, int64(f.Nu.Size)
		}
	} else if info, ok, herr := fc.Store().Has(target); herr == nil && ok && info.Complete {
		out.Files, out.Bytes = 1, int64(info.Size)
	} else {
		out.Error = "the fresh client's fetch returned without the target held: " + errors.Join(err, herr).Error()
		return out
	}
	out.OK = true
	return out
}

func (n *Local) clusterNetStack() ClusterNet {
	n.clusters.mu.Lock()
	defer n.clusters.mu.Unlock()
	return n.clusters.sw
}
