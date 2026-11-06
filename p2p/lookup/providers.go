package lookup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"time"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Delegated provider routing (`GET /routing/v1/providers/{cid}`, IPIP-337; IPNI's
// cid.contact serves it for every multihash it indexes) bounds.
const (
	MaxProviderRecords   = 64        // provider records parsed per lookup
	MaxProvidersResponse = 256 << 10 // response bytes
	ProvidersTimeout     = 15 * time.Second
	maxProviderAddrs     = 16 // QUIC addresses kept per provider
)

// DefaultProvidersURL is the default delegated provider-routing endpoint (IPNI).
const DefaultProvidersURL = "https://cid.contact"

// BitswapTransport is the routing/v1 protocol name of Bitswap providers.
const BitswapTransport = "transport-bitswap"

// ProviderRecord is one routing/v1 `peer` provider record. ID is kept as announced: HTTP
// gateway records may carry IDs that are not usable libp2p peer IDs.
type ProviderRecord struct {
	ID        string
	Protocols []string
	Addrs     []string
}

// Has reports whether the record lists protocol.
func (r ProviderRecord) Has(protocol string) bool {
	for _, p := range r.Protocols {
		if p == protocol {
			return true
		}
	}
	return false
}

// QUIC returns the record's peer ID and its literal-IP QUIC-v1 addresses
// (`/ip4|ip6/<ip>/udp/<port>/quic-v1`, bound to the peer with a terminal /p2p), the only
// libp2p transport Nu dials. ok is false when the ID is not a libp2p peer ID or no
// address qualifies.
func (r ProviderRecord) QUIC() (wire.PeerID, []wire.Multiaddr, bool) {
	id, err := wire.ParsePeerID(r.ID)
	if err != nil {
		return "", nil, false
	}
	var out []wire.Multiaddr
	for _, s := range r.Addrs {
		a, err := wire.ParseMultiaddr(s, 1024)
		if err != nil {
			continue
		}
		m, err := ma.NewMultiaddrBytes(a.Bytes())
		if err != nil {
			continue
		}
		parts := ma.Split(m)
		if n := len(parts); n < 3 || n > 4 || (parts[0].Protocol().Code != ma.P_IP4 && parts[0].Protocol().Code != ma.P_IP6) ||
			parts[1].Protocol().Code != ma.P_UDP || parts[2].Protocol().Code != ma.P_QUIC_V1 || (n == 4 && parts[3].Protocol().Code != ma.P_P2P) {
			continue
		}
		if a, err = NormalizeAddress(id, a); err == nil && len(out) < maxProviderAddrs {
			out = append(out, a)
		}
	}
	return id, out, len(out) > 0
}

// Providers is a bounded delegated provider-routing client.
type Providers struct {
	Client  *httpsrc.Client
	BaseURL string
}

// NewProviders validates the endpoint base URL (http or https, no query).
func NewProviders(client *httpsrc.Client, baseURL string) (*Providers, error) {
	if client == nil {
		return nil, wire.ErrFormat
	}
	base, err := endpointBase(baseURL)
	if err != nil {
		return nil, err
	}
	return &Providers{Client: client, BaseURL: base}, nil
}

// Find asks for providers of the CID named by cidText (CIDv1; IPNI keys by multihash, so
// any codec names the same records). A 404 is no providers.
func (p *Providers) Find(ctx context.Context, cidText string) ([]ProviderRecord, error) {
	var body bytes.Buffer
	res, err := p.Client.Fetch(ctx, p.BaseURL+"/routing/v1/providers/"+cidText, func(int64) (io.Writer, error) { body.Reset(); return &body, nil },
		httpsrc.Options{MaxBytes: MaxProvidersResponse, Timeout: ProvidersTimeout, IdleTimeout: ProvidersTimeout})
	if err != nil {
		var status *httpsrc.StatusError
		if errors.As(err, &status) && status.Code == 404 {
			return nil, nil
		}
		return nil, err
	}
	return ParseProviders(body.Bytes(), res.ContentType)
}

// ParseProviders reads a routing/v1 providers response (`application/json`
// `{"Providers":[…]}` or `application/x-ndjson` records) and returns its `peer` records,
// at most MaxProviderRecords. Other schemas and unknown fields are ignored.
func ParseProviders(b []byte, contentType string) ([]ProviderRecord, error) {
	typ, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("routing: content type %q: %w", contentType, err)
	}
	type record struct {
		Schema string
		ProviderRecord
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
		for len(recs) < MaxProviderRecords {
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
	if len(recs) > MaxProviderRecords {
		recs = recs[:MaxProviderRecords]
	}
	var out []ProviderRecord
	for _, r := range recs {
		if r.Schema == "peer" {
			out = append(out, r.ProviderRecord)
		}
	}
	return out, nil
}
