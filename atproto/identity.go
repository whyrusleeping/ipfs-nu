package atproto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/httpsrc"
)

// DefaultPLCDirectory is the did:plc directory used when Config.PLCDirectory is empty.
const DefaultPLCDirectory = "https://plc.directory"

// Limits bounds what Nu reads from atproto services (D21 "Budgets per account and per PDS
// host"). Zero fields take the defaults.
type Limits struct {
	// DIDDocBytes bounds a DID document; HandleDocBytes a /.well-known/atproto-did body.
	DIDDocBytes, HandleDocBytes int64
	// ProofBytes bounds a getRecord proof CAR.
	ProofBytes int64
	// RepoBytes bounds a getRepo CAR.
	RepoBytes int64
	// MaxBlobs bounds the blobs fetched for one snapshot; each blob is at most
	// cid.MaxWholeObject bytes (the fixed 8 MiB whole-object limit).
	MaxBlobs int
	// HostConcurrency bounds concurrent requests to one PDS host.
	HostConcurrency int
	// Timeout bounds one small request (identity, record, blob); RepoTimeout one getRepo.
	Timeout, RepoTimeout time.Duration
}

// DefaultLimits are the defaults.
var DefaultLimits = Limits{
	DIDDocBytes: 64 << 10, HandleDocBytes: 1 << 10,
	ProofBytes: 8 << 20, RepoBytes: 4 << 30, MaxBlobs: 10_000, HostConcurrency: 4,
	Timeout: 30 * time.Second, RepoTimeout: 30 * time.Minute,
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits
	if l.DIDDocBytes > 0 {
		d.DIDDocBytes = l.DIDDocBytes
	}
	if l.HandleDocBytes > 0 {
		d.HandleDocBytes = l.HandleDocBytes
	}
	if l.ProofBytes > 0 {
		d.ProofBytes = l.ProofBytes
	}
	if l.RepoBytes > 0 {
		d.RepoBytes = l.RepoBytes
	}
	if l.MaxBlobs > 0 {
		d.MaxBlobs = l.MaxBlobs
	}
	if l.HostConcurrency > 0 {
		d.HostConcurrency = l.HostConcurrency
	}
	if l.Timeout > 0 {
		d.Timeout = l.Timeout
	}
	if l.RepoTimeout > 0 {
		d.RepoTimeout = l.RepoTimeout
	}
	return d
}

// Config is trusted application policy for the atproto client.
type Config struct {
	// HTTP is the SSRF policy of every request (DID documents, handles, PDS endpoints).
	HTTP httpsrc.Config
	// PLCDirectory is the did:plc directory base URL (DefaultPLCDirectory when empty).
	PLCDirectory string
	// LookupTXT resolves DNS TXT records for handles; nil uses net.DefaultResolver.
	LookupTXT func(ctx context.Context, name string) ([]string, error)
	// PlainHTTP is a test hook: URLs Nu builds itself (handle well-known, did:web) use
	// http:// instead of https://. HTTP.AllowHTTP must be set too.
	PlainHTTP bool
	// CacheTTL is how long resolved identities are reused (default 10 minutes). Proofs
	// establish authenticity, not freshness; stale answers are acceptable (D6).
	CacheTTL time.Duration
	Limits   Limits
	// Now is the clock (tests); nil uses time.Now.
	Now func() time.Time
}

// Identity is a resolved account: the DID document's PDS and signing key.
type Identity struct {
	DID string
	// Handle is the handle confirmed in both directions, or the document's primary
	// at:// alias when a DID was given; "" when the document names none.
	Handle string
	// PDS is the personal data server's base URL.
	PDS string
	// Key is the repository signing key; KeyMultibase its DID-document form.
	Key          atcrypto.PublicKey
	KeyMultibase string
	// Resolved is when the document was fetched (cache age = now − Resolved).
	Resolved time.Time
}

// Resolver resolves handles and DIDs with a bounded cache. It is safe for concurrent use.
type Resolver struct {
	cfg  Config
	http *httpsrc.Client
	mu   sync.Mutex
	dids map[string]*Identity // DID → identity (Resolved marks the fetch time)
	hdl  map[string]handleHit
}

type handleHit struct {
	did string
	at  time.Time
}

const maxCache = 1024

// NewResolver returns a resolver under cfg.
func NewResolver(cfg Config) *Resolver {
	if cfg.PLCDirectory == "" {
		cfg.PLCDirectory = DefaultPLCDirectory
	}
	if cfg.LookupTXT == nil {
		cfg.LookupTXT = net.DefaultResolver.LookupTXT
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 10 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Limits = cfg.Limits.withDefaults()
	return &Resolver{cfg: cfg, http: httpsrc.New(cfg.HTTP), dids: map[string]*Identity{}, hdl: map[string]handleHit{}}
}

func (r *Resolver) scheme() string {
	if r.cfg.PlainHTTP {
		return "http://"
	}
	return "https://"
}

// Resolve resolves a handle or DID to an identity: handle → DID (DNS TXT, then
// /.well-known/atproto-did), then the DID document, with the handle confirmed by the
// document's alsoKnownAs. Cached answers are returned within CacheTTL.
func (r *Resolver) Resolve(ctx context.Context, authority string) (*Identity, error) {
	if strings.HasPrefix(authority, "did:") {
		return r.ResolveDID(ctx, authority)
	}
	h, err := syntax.ParseHandle(authority)
	if err != nil {
		return nil, invalid("%q: %v", authority, err)
	}
	handle := h.Normalize().String()
	did, err := r.resolveHandle(ctx, handle)
	if err != nil {
		return nil, err
	}
	id, err := r.ResolveDID(ctx, did)
	if err != nil {
		return nil, err
	}
	if id.Handle != handle {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "handle %s points to %s, whose DID document does not list it (it names %q)", handle, did, id.Handle).
			WithAction("the handle may have moved; use the DID directly if you trust it")
	}
	return id, nil
}

func (r *Resolver) resolveHandle(ctx context.Context, handle string) (string, error) {
	r.mu.Lock()
	if hit, ok := r.hdl[handle]; ok && r.cfg.Now().Sub(hit.at) < r.cfg.CacheTTL {
		r.mu.Unlock()
		return hit.did, nil
	}
	r.mu.Unlock()
	did, dnsErr := r.handleDNS(ctx, handle)
	if did == "" {
		var httpErr error
		if did, httpErr = r.handleHTTP(ctx, handle); did == "" {
			return "", apiv0.Errorf(apiv0.CodeNotFound, "handle %s does not resolve: DNS: %v; HTTPS: %v", handle, dnsErr, httpErr).
				WithAction("check the handle, or use the account's DID")
		}
	}
	r.mu.Lock()
	if len(r.hdl) >= maxCache {
		r.hdl = map[string]handleHit{}
	}
	r.hdl[handle] = handleHit{did: did, at: r.cfg.Now()}
	r.mu.Unlock()
	return did, nil
}

// handleDNS reads _atproto.<handle> TXT records; exactly one did=<did> entry is accepted.
func (r *Resolver) handleDNS(ctx context.Context, handle string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Limits.Timeout)
	defer cancel()
	txts, err := r.cfg.LookupTXT(ctx, "_atproto."+handle)
	if err != nil {
		return "", err
	}
	var found string
	for _, t := range txts {
		v, ok := strings.CutPrefix(t, "did=")
		if !ok {
			continue
		}
		if _, err := syntax.ParseDID(v); err != nil {
			return "", fmt.Errorf("TXT record %q is not a DID", t)
		}
		if found != "" && found != v {
			return "", errors.New("conflicting did= TXT records")
		}
		found = v
	}
	if found == "" {
		return "", errors.New("no did= TXT record")
	}
	return found, nil
}

func (r *Resolver) handleHTTP(ctx context.Context, handle string) (string, error) {
	body, err := r.get(ctx, r.scheme()+handle+"/.well-known/atproto-did", r.cfg.Limits.HandleDocBytes, []string{handle})
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(body))
	if _, err := syntax.ParseDID(s); err != nil {
		return "", fmt.Errorf("/.well-known/atproto-did is not a DID")
	}
	return s, nil
}

// ResolveDID fetches and parses the DID document of a did:plc or did:web identity.
func (r *Resolver) ResolveDID(ctx context.Context, did string) (*Identity, error) {
	d, err := syntax.ParseDID(did)
	if err != nil {
		return nil, invalid("%q: %v", did, err)
	}
	did = d.String()
	r.mu.Lock()
	if id, ok := r.dids[did]; ok && r.cfg.Now().Sub(id.Resolved) < r.cfg.CacheTTL {
		r.mu.Unlock()
		return id, nil
	}
	r.mu.Unlock()
	var u string
	var hosts []string
	switch d.Method() {
	case "plc":
		u = strings.TrimSuffix(r.cfg.PLCDirectory, "/") + "/" + did
	case "web":
		host, err := url.PathUnescape(strings.TrimPrefix(did, "did:web:"))
		if err != nil || host == "" || strings.ContainsAny(host, "/?#@ ") {
			return nil, invalid("%q is not a resolvable did:web", did)
		}
		// did:web:host:path:segments → https://host/path/segments/did.json
		parts := strings.Split(host, ":")
		if len(parts) == 1 {
			u = r.scheme() + host + "/.well-known/did.json"
		} else {
			u = r.scheme() + strings.Join(parts, "/") + "/did.json"
		}
		hosts = []string{parts[0]}
	default:
		return nil, apiv0.Errorf(apiv0.CodeNotSupported, "DID method %q is not supported", d.Method()).
			WithAction("Nu resolves did:plc and did:web")
	}
	body, err := r.get(ctx, u, r.cfg.Limits.DIDDocBytes, hosts)
	if err != nil {
		code := apiv0.CodeUnavailable
		if strings.Contains(err.Error(), "HTTP status 404") {
			code = apiv0.CodeNotFound
		}
		return nil, apiv0.Errorf(code, "cannot resolve %s: %v", did, err).WithAction("check the DID and your network")
	}
	id, err := parseDIDDocument(did, body, r.cfg.HTTP.AllowHTTP)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "DID document of %s: %v", did, err)
	}
	id.Resolved = r.cfg.Now()
	r.mu.Lock()
	if len(r.dids) >= maxCache {
		r.dids = map[string]*Identity{}
	}
	r.dids[did] = id
	r.mu.Unlock()
	return id, nil
}

// Forget drops a cached identity so the next lookup re-resolves (after a signature
// failure the key may have rotated).
func (r *Resolver) Forget(did string) {
	r.mu.Lock()
	delete(r.dids, did)
	r.mu.Unlock()
}

type didDocument struct {
	ID                 string   `json:"id"`
	AlsoKnownAs        []string `json:"alsoKnownAs"`
	VerificationMethod []struct {
		ID                 string `json:"id"`
		Type               string `json:"type"`
		PublicKeyMultibase string `json:"publicKeyMultibase"`
	} `json:"verificationMethod"`
	Service []struct {
		ID              string `json:"id"`
		Type            string `json:"type"`
		ServiceEndpoint any    `json:"serviceEndpoint"`
	} `json:"service"`
}

// parseDIDDocument extracts the #atproto signing key and #atproto_pds endpoint.
func parseDIDDocument(did string, body []byte, allowHTTP bool) (*Identity, error) {
	var doc didDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("not a JSON document: %v", err)
	}
	if doc.ID != did {
		return nil, fmt.Errorf("document id %q is not %s", doc.ID, did)
	}
	id := &Identity{DID: did}
	for _, aka := range doc.AlsoKnownAs {
		if h, ok := strings.CutPrefix(aka, "at://"); ok {
			if ph, err := syntax.ParseHandle(h); err == nil {
				id.Handle = ph.Normalize().String()
				break // the first at:// alias is the primary handle
			}
		}
	}
	for _, vm := range doc.VerificationMethod {
		if vm.ID != "#atproto" && vm.ID != did+"#atproto" {
			continue
		}
		key, err := atrepo.ParseKey(vm.PublicKeyMultibase)
		if err != nil {
			return nil, fmt.Errorf("signing key: %v", err)
		}
		id.Key, id.KeyMultibase = key, vm.PublicKeyMultibase
		break
	}
	if id.Key == nil {
		return nil, errors.New("no #atproto signing key")
	}
	for _, s := range doc.Service {
		if (s.ID != "#atproto_pds" && s.ID != did+"#atproto_pds") || s.Type != "AtprotoPersonalDataServer" {
			continue
		}
		ep, _ := s.ServiceEndpoint.(string)
		u, err := url.Parse(ep)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			(u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
			return nil, fmt.Errorf("PDS endpoint %q is not an https origin", ep)
		}
		id.PDS = u.Scheme + "://" + u.Host
		break
	}
	if id.PDS == "" {
		return nil, errors.New("no #atproto_pds service")
	}
	return id, nil
}

// get fetches a small document through the SSRF guards into memory.
func (r *Resolver) get(ctx context.Context, u string, max int64, hosts []string) ([]byte, error) {
	var buf bytes.Buffer
	_, err := r.http.Fetch(ctx, u, func(int64) (io.Writer, error) { buf.Reset(); return &buf, nil },
		httpsrc.Options{MaxBytes: max, Timeout: r.cfg.Limits.Timeout, IdleTimeout: r.cfg.Limits.Timeout, Hosts: hosts})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// xrpc builds a PDS XRPC URL.
func xrpc(pds, method string, q url.Values) string {
	return pds + "/xrpc/" + method + "?" + q.Encode()
}
