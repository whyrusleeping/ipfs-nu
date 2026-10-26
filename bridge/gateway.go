package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/httpsrc"
)

// GatewayProtocol is the routing/v1 protocol name (and IPNI transport) of HTTP trustless
// gateways, as announced by Filecoin storage providers.
const GatewayProtocol = "transport-ipfs-gateway-http"

const (
	maxGatewayProviders = 8         // gateways tried per lookup
	maxRoutingRecords   = 64        // provider records parsed per lookup
	maxRoutingResponse  = 256 << 10 // routing/v1 response bytes
	routingTimeout      = 15 * time.Second
	gatewayIdleTimeout  = 10 * time.Second
	gatewayBlockTimeout = 60 * time.Second
)

// GatewaySource fetches original blocks from HTTP trustless gateways
// (`GET <gateway>/ipfs/<cid>?format=raw`) found through a delegated-routing endpoint
// (`GET <routing>/routing/v1/providers/<cid>`, records announcing GatewayProtocol). Gateways
// that already served a block are asked first, so a DAG held by one provider costs one
// lookup. Every block is verified by its CID before it is returned; a gateway that serves
// wrong bytes is demoted on the shared client and the next one is tried.
//
// Gateways answer one block per request, so the source reads ahead in the importer's walk
// order: when a dag-pb node arrives its children are queued in front of everything queued
// earlier (depth first, links in order), and queued blocks are fetched in the background,
// gatewayFetches at a time, while fewer than Readahead fetched blocks wait to be asked for.
// Dry runs, which skip raw leaves, set Readahead to 0.
type GatewaySource struct {
	Client    *httpsrc.Client
	Routing   string // routing/v1 base URL
	Readahead int
	mu        sync.Mutex
	known     []string // gateway base URLs, in order of first success
	queue     []IPFSCID
	ahead     map[IPFSCID]*aheadBlock // started, not yet asked for
	running   int
	ctx       context.Context
	cancel    context.CancelFunc
}

type aheadBlock struct {
	done chan struct{}
	b    []byte
	err  error
}

const (
	DefaultReadahead = 32
	gatewayFetches   = 8
	maxReadQueue     = 4096
)

func NewGatewaySource(c *httpsrc.Client, routing string) *GatewaySource {
	ctx, cancel := context.WithCancel(context.Background())
	return &GatewaySource{Client: c, Routing: strings.TrimRight(routing, "/"), Readahead: DefaultReadahead,
		ahead: map[IPFSCID]*aheadBlock{}, ctx: ctx, cancel: cancel}
}

// Close stops reading ahead.
func (s *GatewaySource) Close() { s.cancel() }

func (s *GatewaySource) GetBlock(ctx context.Context, id IPFSCID) ([]byte, error) {
	s.mu.Lock()
	a := s.ahead[id]
	delete(s.ahead, id)
	for i, q := range s.queue {
		if q == id {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			break
		}
	}
	s.pumpLocked()
	s.mu.Unlock()
	if a != nil {
		select {
		case <-a.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if a.err == nil {
			s.readahead(id, a.b)
			return a.b, nil
		}
	}
	b, err := s.get(ctx, id)
	if err == nil {
		s.readahead(id, b)
	}
	return b, err
}

// readahead queues the children of a dag-pb block ahead of earlier queued blocks.
func (s *GatewaySource) readahead(id IPFSCID, b []byte) {
	if s.Readahead <= 0 || id.Codec != codecDagPB {
		return
	}
	pb, err := DecodePBNode(b)
	if err != nil || len(pb.Links) == 0 {
		return
	}
	var kids []IPFSCID
	for _, l := range pb.Links {
		if c, err := ParseIPFSBytes(l.CID); err == nil {
			kids = append(kids, c)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(kids, s.queue...)
	if len(s.queue) > maxReadQueue {
		s.queue = s.queue[:maxReadQueue]
	}
	s.pumpLocked()
}

// pumpLocked starts queued fetches while the concurrency and readahead budgets allow.
func (s *GatewaySource) pumpLocked() {
	for len(s.queue) > 0 && s.running < gatewayFetches && len(s.ahead) < s.Readahead && s.ctx.Err() == nil {
		c := s.queue[0]
		s.queue = s.queue[1:]
		if s.ahead[c] != nil {
			continue
		}
		a := &aheadBlock{done: make(chan struct{})}
		s.ahead[c] = a
		s.running++
		go func() {
			a.b, a.err = s.get(s.ctx, c)
			close(a.done)
			s.mu.Lock()
			s.running--
			s.pumpLocked()
			s.mu.Unlock()
		}()
	}
}

func (s *GatewaySource) get(ctx context.Context, id IPFSCID) ([]byte, error) {
	if s.Client == nil {
		return nil, errors.New("gateway: HTTP client required")
	}
	tried := map[string]bool{}
	var errs []string
	try := func(gws []string) []byte {
		var demoted []string
		for pass := 0; pass < 2; pass++ {
			for _, gw := range gws {
				if tried[gw] || ctx.Err() != nil {
					continue
				}
				if pass == 0 && s.Client.Demoted(gw+"/") {
					demoted = append(demoted, gw)
					continue
				}
				tried[gw] = true
				b, err := s.fetch(ctx, gw, id)
				if err == nil {
					s.remember(gw)
					return b
				}
				errs = append(errs, fmt.Sprintf("%s: %v", gw, err))
			}
			gws = demoted
		}
		return nil
	}
	s.mu.Lock()
	known := append([]string(nil), s.known...)
	s.mu.Unlock()
	if b := try(known); b != nil {
		return b, nil
	}
	if s.Routing != "" && ctx.Err() == nil {
		gws, err := s.FindGateways(ctx, id)
		if err != nil {
			errs = append(errs, "routing: "+err.Error())
		}
		if b := try(gws); b != nil {
			return b, nil
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(errs) == 0 {
		return nil, &MissingBlockError{CID: id, Err: errors.New("gateway: no providers found")}
	}
	return nil, &MissingBlockError{CID: id, Err: fmt.Errorf("gateway: no provider supplied the block: %s", strings.Join(errs, "; "))}
}

func (s *GatewaySource) remember(gw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.known {
		if k == gw {
			return
		}
	}
	if len(s.known) < maxGatewayProviders {
		s.known = append(s.known, gw)
	}
}

// v1String names a block the way every trustless gateway and routing endpoint accepts:
// CIDv1, base32 (a CIDv0 is the same dag-pb block).
func v1String(id IPFSCID) string {
	id.Version = 1
	return id.String()
}

func (s *GatewaySource) fetch(ctx context.Context, gw string, id IPFSCID) ([]byte, error) {
	var body bytes.Buffer
	res, err := s.Client.Fetch(ctx, gw+"/ipfs/"+v1String(id)+"?format=raw", func(int64) (io.Writer, error) { body.Reset(); return &body, nil },
		httpsrc.Options{MaxBytes: maxBitswapBlock, Timeout: gatewayBlockTimeout, IdleTimeout: gatewayIdleTimeout})
	if err != nil {
		return nil, err
	}
	if err = id.Verify(body.Bytes()); err != nil {
		s.Client.ReportVerificationFailure(res.FinalURL)
		return nil, err
	}
	return body.Bytes(), nil
}

// FindGateways asks the routing endpoint for providers of id and returns the base URLs of
// those announcing GatewayProtocol (at most maxGatewayProviders). A 404 is no providers.
func (s *GatewaySource) FindGateways(ctx context.Context, id IPFSCID) ([]string, error) {
	var body bytes.Buffer
	res, err := s.Client.Fetch(ctx, s.Routing+"/routing/v1/providers/"+v1String(id), func(int64) (io.Writer, error) { body.Reset(); return &body, nil },
		httpsrc.Options{MaxBytes: maxRoutingResponse, Timeout: routingTimeout, IdleTimeout: routingTimeout})
	if err != nil {
		var status *httpsrc.StatusError
		if errors.As(err, &status) && status.Code == 404 {
			return nil, nil
		}
		return nil, err
	}
	return ParseGatewayProviders(body.Bytes(), res.ContentType)
}

// ParseGatewayProviders reads a routing/v1 providers response (JSON or NDJSON) and returns
// the distinct gateway base URLs of `peer` records that list GatewayProtocol. Addresses
// that are not HTTP(S) multiaddrs are skipped.
func ParseGatewayProviders(b []byte, contentType string) ([]string, error) {
	typ, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("routing: content type %q: %w", contentType, err)
	}
	type record struct {
		Schema    string
		Protocols []string
		Addrs     []string
	}
	var recs []record
	switch typ {
	case "application/json":
		var r struct{ Providers []record }
		if err = json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("routing: %w", err)
		}
		recs = r.Providers
	case "application/x-ndjson":
		d := json.NewDecoder(bytes.NewReader(b))
		for len(recs) < maxRoutingRecords {
			var r record
			if err = d.Decode(&r); err == io.EOF {
				break
			} else if err != nil {
				return nil, fmt.Errorf("routing: %w", err)
			}
			recs = append(recs, r)
		}
	default:
		return nil, fmt.Errorf("routing: unexpected content type %q", typ)
	}
	if len(recs) > maxRoutingRecords {
		recs = recs[:maxRoutingRecords]
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range recs {
		gateway := false
		for _, p := range r.Protocols {
			gateway = gateway || p == GatewayProtocol
		}
		if r.Schema != "peer" || !gateway {
			continue
		}
		for _, a := range r.Addrs {
			if u, ok := GatewayURL(a); ok && !seen[u] && len(out) < maxGatewayProviders {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	return out, nil
}

// GatewayURL converts an HTTP(S) multiaddr (`/dns/h/tcp/443/https`, `/ip4/a/tcp/p/tls/http`,
// `/ip6/a/tcp/p/http`, with dns4/dns6 and an optional `/tls/sni/<h>`) to a base URL.
func GatewayURL(addr string) (string, bool) {
	p := strings.Split(strings.TrimPrefix(addr, "/"), "/")
	if len(p) < 5 || p[2] != "tcp" || p[1] == "" {
		return "", false
	}
	switch p[0] {
	case "dns", "dns4", "dns6", "ip4", "ip6":
	default:
		return "", false
	}
	host, port, rest := p[1], p[3], p[4:]
	if net.ParseIP(host) == nil && strings.ContainsAny(host, ":[]@") {
		return "", false
	}
	scheme := ""
	switch {
	case len(rest) == 1 && rest[0] == "https":
		scheme = "https"
	case len(rest) == 1 && rest[0] == "http":
		scheme = "http"
	case len(rest) == 2 && rest[0] == "tls" && rest[1] == "http":
		scheme = "https"
	case len(rest) == 4 && rest[0] == "tls" && rest[1] == "sni" && rest[3] == "http":
		scheme = "https"
	default:
		return "", false
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		if strings.Contains(host, ":") {
			return scheme + "://[" + host + "]", true
		}
		return scheme + "://" + host, true
	}
	return scheme + "://" + net.JoinHostPort(host, port), true
}

// FirstSource asks the source that supplied the previous block first; when it fails (or
// before any block) it asks every source at once and returns the first block that verifies,
// cancelling the others, so a slow transport (an Amino lookup per block) is only paid for
// while no other source is serving. When all fail the block is missing.
type FirstSource struct {
	Sources []BlockSource
	mu      sync.Mutex
	last    int // index+1 of the source that supplied the previous block
}

func NewFirstSource(srcs ...BlockSource) *FirstSource { return &FirstSource{Sources: srcs} }

func (f *FirstSource) GetBlock(ctx context.Context, id IPFSCID) ([]byte, error) {
	f.mu.Lock()
	last := f.last
	f.mu.Unlock()
	var errs []string
	if last > 0 {
		b, err := f.Sources[last-1].GetBlock(ctx, id)
		if err == nil {
			if err = id.Verify(b); err == nil {
				return b, nil
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, err.Error())
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type reply struct {
		i   int
		b   []byte
		err error
	}
	ch := make(chan reply, len(f.Sources))
	asked := 0
	for i, s := range f.Sources {
		if i+1 == last {
			continue
		}
		asked++
		go func(i int, s BlockSource) {
			b, err := s.GetBlock(rctx, id)
			if err == nil {
				err = id.Verify(b)
			}
			ch <- reply{i, b, err}
		}(i, s)
	}
	for ; asked > 0; asked-- {
		r := <-ch
		if r.err == nil {
			f.mu.Lock()
			f.last = r.i + 1
			f.mu.Unlock()
			return r.b, nil
		}
		errs = append(errs, r.err.Error())
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, &MissingBlockError{CID: id, Err: errors.New(strings.Join(errs, "; "))}
}
