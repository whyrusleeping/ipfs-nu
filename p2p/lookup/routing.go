package lookup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// PeerJSON is the IPIP-417 peer schema. Protocols are deliberately omitted by
// our directory: a signed peer record does not authenticate supported protocols.
type PeerJSON struct {
	Schema string   `json:"Schema"`
	ID     string   `json:"ID"`
	Addrs  []string `json:"Addrs"`
}
type RoutingLimits struct{ MaxBytes, MaxPeers, MaxAddresses, MaxAddressBytes, MaxDepth int }

func DefaultRoutingLimits() RoutingLimits { return RoutingLimits{64 << 10, 32, 32, 1024, 16} }

type HTTPSource struct {
	Client  *httpsrc.Client
	BaseURL string
	Hosts   []string
	Limits  RoutingLimits
	Timeout time.Duration
}

func NewHTTPSource(client *httpsrc.Client, baseURL string) (*HTTPSource, error) {
	if client == nil || len(baseURL) > httpsrc.MaxURLBytes {
		return nil, wire.ErrFormat
	}
	baseURL, err := endpointBase(baseURL)
	if err != nil {
		return nil, err
	}
	return &HTTPSource{Client: client, BaseURL: baseURL, Limits: DefaultRoutingLimits(), Timeout: 3 * time.Second}, nil
}
func endpointBase(raw string) (string, error) {
	if len(raw) > httpsrc.MaxURLBytes {
		return "", wire.ErrLimit
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", wire.ErrFormat
	}
	return strings.TrimRight(u.String(), "/"), nil
}
func (h *HTTPSource) Lookup(ctx context.Context, id wire.PeerID, emit func(Address) bool) error {
	if h.Client == nil {
		return wire.ErrFormat
	}
	if _, err := wire.PeerIDFromBytes(id.Bytes()); err != nil {
		return err
	}
	var body bytes.Buffer
	result, err := h.Client.Fetch(ctx, h.BaseURL+"/routing/v1/peers/"+id.String(), func(int64) (io.Writer, error) { return &body, nil }, httpsrc.Options{MaxBytes: int64(h.Limits.MaxBytes), Timeout: h.Timeout, IdleTimeout: h.Timeout, Hosts: h.Hosts})
	if err != nil {
		var status *httpsrc.StatusError
		if errors.As(err, &status) && status.Code == 404 {
			return nil
		} // IPIP-417 legacy empty result
		return err
	}
	addrs, err := ParseRoutingResponse(body.Bytes(), result.ContentType, id, h.Limits)
	if err != nil {
		return err
	}
	now := time.Now()
	ttl := 30 * time.Second
	for _, directive := range strings.Split(result.CacheControl, ",") {
		directive = strings.ToLower(strings.TrimSpace(directive))
		if directive == "no-store" || directive == "no-cache" {
			ttl = 0
		}
		if value, ok := strings.CutPrefix(directive, "max-age="); ok {
			if seconds, err := strconv.ParseUint(strings.Trim(value, `"`), 10, 32); err == nil {
				ttl = min(ttl, time.Duration(seconds)*time.Second)
			}
		}
	}
	// Conservative source freshness; these unsigned HTTP hints still require
	// authenticated libp2p dialing. No signed-record trust is inferred from JSON.
	for _, a := range addrs {
		if !emit(Address{Peer: id, Addr: a, Source: "routing/v1", Seen: now, Expires: now.Add(ttl), Confidence: 0.6}) {
			break
		}
	}
	return nil
}
func jsonDepth(b []byte, maxBytes, maxDepth int) error {
	if len(b) > maxBytes || maxDepth <= 0 {
		return wire.ErrLimit
	}
	d := json.NewDecoder(bytes.NewReader(b))
	depth := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			if depth != 0 {
				return wire.ErrFormat
			}
			return nil
		}
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
				if depth > maxDepth {
					return wire.ErrLimit
				}
			case '}', ']':
				depth--
			}
		}
	}
}
func ParseRoutingResponse(b []byte, contentType string, id wire.PeerID, l RoutingLimits) ([]wire.Multiaddr, error) {
	if l.MaxPeers <= 0 || l.MaxAddresses <= 0 || l.MaxAddressBytes <= 0 {
		return nil, wire.ErrLimit
	}
	if err := jsonDepth(b, l.MaxBytes, l.MaxDepth); err != nil {
		return nil, err
	}
	typ, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, err
	}
	var peers []json.RawMessage
	switch typ {
	case "application/json":
		var response struct{ Peers []json.RawMessage }
		if err = json.Unmarshal(b, &response); err != nil {
			return nil, err
		}
		peers = response.Peers
	case "application/x-ndjson":
		d := json.NewDecoder(bytes.NewReader(b))
		for {
			var p json.RawMessage
			err = d.Decode(&p)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			peers = append(peers, p)
			if len(peers) > l.MaxPeers {
				return nil, wire.ErrLimit
			}
		}
	default:
		return nil, wire.ErrFormat
	}
	if len(peers) > l.MaxPeers {
		return nil, wire.ErrLimit
	}
	var out []wire.Multiaddr
	seen := map[string]bool{}
	total := 0
	for _, raw := range peers {
		var schema struct{ Schema string }
		if err := json.Unmarshal(raw, &schema); err != nil {
			return nil, err
		}
		if schema.Schema != "peer" {
			continue
		}
		var p PeerJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		peer, err := wire.ParsePeerID(p.ID)
		if err != nil {
			return nil, err
		}
		if peer != id {
			continue
		}
		for _, s := range p.Addrs {
			total++
			if total > l.MaxAddresses {
				return nil, wire.ErrLimit
			}
			a, err := wire.ParseMultiaddr(s, l.MaxAddressBytes)
			if err != nil {
				return nil, err
			}
			a, err = NormalizeAddress(id, a)
			if err != nil {
				return nil, err
			}
			if !seen[a.String()] {
				seen[a.String()] = true
				out = append(out, a)
			}
		}
	}
	return out, nil
}
