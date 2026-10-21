package lookup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/whyrusleeping/nu/p2p/wire"
)

const PublishPath = "/nu/lookup/v1/peers/"

var (
	ErrSequence  = errors.New("lookup: peer record sequence did not increase")
	ErrAdmission = errors.New("lookup: publisher not admitted")
)

func PeerRecordLimits() wire.RecordLimits {
	return wire.RecordLimits{MaxBytes: 8192, MaxAddresses: 32, MaxAddressBytes: 1024}
}

type DirectoryConfig struct {
	Domain                  string                 // e.g. peers.example.org; no project-operated default
	MaxPeers, MaxConcurrent int                    // defaults 1024, 16
	Lifetime                time.Duration          // receipt retention, default 1h, at most 24h
	AllowPeer               func(wire.PeerID) bool // optional operator admission; must not block
}
type directoryEntry struct {
	record           wire.PeerRecord
	updated, expires time.Time
}

// Directory verifies signed records before storing addresses. It never fetches,
// resolves or dials their contents. HTTP and DNS are projections, not signature
// transports. Data and sequence high-water marks are in memory for the lifetime
// of each retained entry; persistence/anti-replay across restart are not claimed.
type Directory struct {
	cfg   DirectoryConfig
	mu    sync.Mutex
	peers map[wire.PeerID]directoryEntry
	slots chan struct{}
}

func NewDirectory(cfg DirectoryConfig) (*Directory, error) {
	var err error
	cfg.Domain, err = peerDomain(cfg.Domain)
	if err != nil {
		return nil, err
	}
	if cfg.MaxPeers == 0 {
		cfg.MaxPeers = 1024
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 16
	}
	if cfg.Lifetime == 0 {
		cfg.Lifetime = time.Hour
	}
	if cfg.MaxPeers <= 0 || cfg.MaxConcurrent <= 0 || cfg.Lifetime < time.Second || cfg.Lifetime > 24*time.Hour {
		return nil, wire.ErrLimit
	}
	return &Directory{cfg: cfg, peers: make(map[wire.PeerID]directoryEntry), slots: make(chan struct{}, cfg.MaxConcurrent)}, nil
}
func (d *Directory) Publish(ctx context.Context, id wire.PeerID, envelope []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	default:
		return wire.ErrLimit
	}
	if d.cfg.AllowPeer != nil && !d.cfg.AllowPeer(id) {
		return ErrAdmission
	}
	r, err := wire.OpenPeerRecord(envelope, PeerRecordLimits())
	if err != nil {
		return err
	}
	if r.PeerID != id {
		return ErrPeerMismatch
	}
	var normalized []wire.Multiaddr
	seen := map[string]bool{}
	for _, a := range r.Addresses {
		a, err = NormalizeAddress(id, a)
		if err != nil {
			return err
		}
		if len(a.String()) > 1024 {
			return wire.ErrLimit
		}
		if !seen[a.String()] {
			seen[a.String()] = true
			normalized = append(normalized, a)
		}
	}
	r.Addresses = normalized
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	// Preserve resident sequence checks even on the first update after expiry;
	// other expired identities can be removed to bound memory.
	if old, ok := d.peers[id]; ok && r.Seq <= old.record.Seq {
		return ErrSequence
	}
	for peer, old := range d.peers {
		if peer != id && !old.expires.After(now) {
			delete(d.peers, peer)
		}
	}
	if _, ok := d.peers[id]; !ok && len(d.peers) >= d.cfg.MaxPeers {
		return wire.ErrLimit
	}
	d.peers[id] = directoryEntry{r, now, now.Add(d.cfg.Lifetime)}
	return nil
}
func (d *Directory) get(id wire.PeerID) (directoryEntry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.peers[id]
	return r, ok && time.Now().Before(r.expires)
}
func peerJSON(r wire.PeerRecord) PeerJSON {
	p := PeerJSON{Schema: "peer", ID: r.PeerID.String(), Addrs: []string{}}
	for _, a := range r.Addresses {
		p.Addrs = append(p.Addrs, a.String())
	}
	return p
}
func (d *Directory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if len(r.URL.RequestURI()) > 2048 {
		http.Error(w, "request too large", http.StatusRequestURITooLong)
		return
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/routing/v1/peers/") {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Accept")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if r.Method == http.MethodPut && strings.HasPrefix(path, PublishPath) {
		id, err := wire.ParsePeerID(strings.TrimPrefix(path, PublishPath))
		if err != nil {
			http.Error(w, "invalid peer", http.StatusUnprocessableEntity)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "record too large", http.StatusRequestEntityTooLarge)
			return
		}
		err = d.Publish(r.Context(), id, body)
		if err != nil {
			code := http.StatusUnprocessableEntity
			if errors.Is(err, ErrSequence) {
				code = http.StatusConflict
			}
			if errors.Is(err, ErrAdmission) {
				code = http.StatusForbidden
			}
			if errors.Is(err, wire.ErrLimit) {
				code = http.StatusTooManyRequests
			}
			http.Error(w, err.Error(), code)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet || !strings.HasPrefix(path, "/routing/v1/peers/") {
		http.NotFound(w, r)
		return
	}
	id, err := wire.ParsePeerID(strings.TrimPrefix(path, "/routing/v1/peers/"))
	if err != nil {
		http.Error(w, "invalid peer", http.StatusUnprocessableEntity)
		return
	}
	entry, ok := d.get(id)
	peers := []PeerJSON{}
	ttl := 15
	if ok {
		peers = append(peers, peerJSON(entry.record))
		ttl = int(min(30*time.Second, time.Until(entry.expires)) / time.Second)
		w.Header().Set("Last-Modified", entry.updated.UTC().Format(http.TimeFormat))
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", max(ttl, 0)))
	w.Header().Set("Vary", "Accept")
	if strings.Contains(r.Header.Get("Accept"), "application/x-ndjson") {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, p := range peers {
			_ = json.NewEncoder(w).Encode(p)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct{ Peers []PeerJSON }{peers})
}
func (d *Directory) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	reply := new(dns.Msg)
	reply.SetReply(r)
	if r.Opcode != dns.OpcodeQuery || len(r.Question) != 1 {
		reply.Rcode = dns.RcodeFormatError
		_ = w.WriteMsg(reply)
		return
	}
	q := r.Question[0]
	name := strings.ToLower(q.Name)
	suffix := "." + d.cfg.Domain + "."
	if !strings.HasPrefix(name, "_dnsaddr.") || !strings.HasSuffix(name, suffix) {
		reply.Rcode = dns.RcodeRefused
		_ = w.WriteMsg(reply)
		return
	}
	label := strings.TrimSuffix(strings.TrimPrefix(name, "_dnsaddr."), suffix)
	id, err := wire.ParsePeerID(label)
	if err != nil || id.Base36() != label {
		reply.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(reply)
		return
	}
	reply.Authoritative = true
	entry, ok := d.get(id)
	if !ok {
		reply.Rcode = dns.RcodeNameError
	} else if q.Qtype == dns.TypeTXT && q.Qclass == dns.ClassINET {
		ttl := uint32(max(0, min(5*time.Minute, time.Until(entry.expires))/time.Second))
		for _, a := range entry.record.Addresses {
			text := "dnsaddr=" + a.String()
			var parts []string
			for len(text) > 0 {
				n := min(len(text), 255)
				parts = append(parts, text[:n])
				text = text[n:]
			}
			reply.Answer = append(reply.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: ttl}, Txt: parts})
		}
	}
	if w.RemoteAddr().Network() == "udp" {
		size := 512
		if opt := r.IsEdns0(); opt != nil {
			size = max(512, min(1232, int(opt.UDPSize())))
		}
		reply.Truncate(size)
	}
	_ = w.WriteMsg(reply)
}
