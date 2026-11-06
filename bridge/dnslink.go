package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/whyrusleeping/nu/httpsrc"
)

// DNSLink resolution (https://dnslink.dev): TXT records `dnslink=/ipfs/<cid>` or
// `dnslink=/ipns/<domain>` on `_dnslink.<domain>` (or, failing that, on the domain itself)
// name an IPFS root. ENS names resolve the same way through a DNS-over-HTTPS endpoint that
// answers ENS contenthash records as DNSLink TXT (eth.limo), as Kubo does. The answer is
// trusted naming, not verification: the root's blocks are still verified by their CIDs.

const (
	maxDNSLinkDepth  = 8
	maxDNSLinkTXT    = 64
	maxDoHResponse   = 64 << 10
	dnsLinkTimeout   = 15 * time.Second
	dnsMessageType   = "application/dns-message"
	maxDNSNameLength = 253
)

// DefaultDoH maps domain suffixes to RFC 8484 DNS-over-HTTPS endpoints; other names use the
// system resolver.
var DefaultDoH = map[string]string{"eth": "https://dns.eth.limo/dns-query"}

// DNSLinkResolver resolves DNSLink names. LookupTXT is the system resolver (nil uses
// net.DefaultResolver); DoH maps a domain suffix to an endpoint queried through Client
// (an empty endpoint selects the system resolver for that suffix).
type DNSLinkResolver struct {
	LookupTXT func(context.Context, string) ([]string, error)
	DoH       map[string]string
	Client    *httpsrc.Client
}

// ParseImportTarget reads an import target: a CID or `/ipfs/<cid>` is a root; a domain,
// `/ipns/<domain>` or `ipns://<domain>` is a DNSLink name to resolve.
func ParseImportTarget(s string) (root IPFSCID, name string, err error) {
	switch {
	case strings.HasPrefix(s, "/ipfs/"):
		rest := strings.TrimPrefix(s, "/ipfs/")
		if c, _, _ := strings.Cut(rest, "/"); c != strings.TrimSuffix(rest, "/") {
			return IPFSCID{}, "", errors.New("import a root CID, not a path inside it")
		}
		root, err = ParseIPFSCID(strings.TrimSuffix(rest, "/"))
		return root, "", err
	case strings.HasPrefix(s, "/ipns/"), strings.HasPrefix(s, "ipns://"):
		s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "/ipns/"), "ipns://"), "/")
		if name, err = dnsLinkName(s); err != nil {
			return IPFSCID{}, "", fmt.Errorf("%q is not a DNSLink domain (IPNS keys are not supported): %w", s, err)
		}
		return IPFSCID{}, name, nil
	}
	if root, err = ParseIPFSCID(s); err == nil {
		return root, "", nil
	}
	if !strings.Contains(s, ".") {
		return IPFSCID{}, "", err
	}
	if name, err = dnsLinkName(s); err != nil {
		return IPFSCID{}, "", err
	}
	return IPFSCID{}, name, nil
}

// dnsLinkName normalizes a DNS name (lowercase, no trailing dot, at least two labels).
func dnsLinkName(s string) (string, error) {
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	labels := strings.Split(s, ".")
	if len(s) == 0 || len(s) > maxDNSNameLength || len(labels) < 2 {
		return "", errors.New("dnslink: invalid domain name")
	}
	for _, l := range labels {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", errors.New("dnslink: invalid domain name")
		}
		for _, ch := range l {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
				return "", errors.New("dnslink: invalid domain name")
			}
		}
	}
	return s, nil
}

// Resolve follows name's DNSLink records (through `/ipns/<domain>` links, at most 8) to an
// IPFS root. Among several `dnslink=` values the lexicographically first is used.
func (r *DNSLinkResolver) Resolve(ctx context.Context, name string) (IPFSCID, error) {
	ctx, cancel := context.WithTimeout(ctx, dnsLinkTimeout)
	defer cancel()
	orig := name
	for depth := 0; depth < maxDNSLinkDepth; depth++ {
		value, err := r.record(ctx, name)
		if err != nil {
			return IPFSCID{}, err
		}
		ns, rest, _ := strings.Cut(strings.TrimPrefix(value, "/"), "/")
		target, path, _ := strings.Cut(rest, "/")
		if strings.Trim(path, "/") != "" {
			return IPFSCID{}, fmt.Errorf("dnslink: %s points into a root (%s); import the root CID or ask the publisher for a root link", name, value)
		}
		switch ns {
		case "ipfs":
			c, err := ParseIPFSCID(target)
			if err != nil {
				return IPFSCID{}, fmt.Errorf("dnslink: %s: %s: %w", name, value, err)
			}
			return c, nil
		case "ipns":
			next, err := dnsLinkName(target)
			if err != nil {
				return IPFSCID{}, fmt.Errorf("dnslink: %s points to %s: IPNS keys are not supported", name, value)
			}
			name = next
		default:
			return IPFSCID{}, fmt.Errorf("dnslink: %s: unsupported value %q", name, value)
		}
	}
	return IPFSCID{}, fmt.Errorf("dnslink: %s: more than %d /ipns/ links", orig, maxDNSLinkDepth)
}

// record returns the DNSLink value of one name: `_dnslink.<name>` first, then the name.
func (r *DNSLinkResolver) record(ctx context.Context, name string) (string, error) {
	var errs []error
	for _, q := range []string{"_dnslink." + name, name} {
		txts, err := r.txt(ctx, q)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", q, err))
			continue
		}
		var values []string
		for _, t := range txts {
			if v, ok := strings.CutPrefix(strings.TrimSpace(t), "dnslink="); ok && (strings.HasPrefix(v, "/ipfs/") || strings.HasPrefix(v, "/ipns/")) {
				values = append(values, strings.TrimSpace(v))
			}
		}
		if len(values) > 0 {
			sort.Strings(values)
			return values[0], nil
		}
	}
	if len(errs) > 0 {
		return "", fmt.Errorf("dnslink: %s: %w", name, errors.Join(errs...))
	}
	return "", fmt.Errorf("dnslink: %s has no dnslink= TXT record", name)
}

func (r *DNSLinkResolver) txt(ctx context.Context, name string) ([]string, error) {
	endpoint := ""
	best := -1
	doh := r.DoH
	if doh == nil {
		doh = DefaultDoH
	}
	for suffix, url := range doh {
		suffix = strings.Trim(strings.ToLower(suffix), ".")
		if (name == suffix || strings.HasSuffix(name, "."+suffix)) && len(suffix) > best {
			endpoint, best = url, len(suffix)
		}
	}
	if endpoint != "" {
		return r.queryDoH(ctx, endpoint, name)
	}
	lookup := r.LookupTXT
	if lookup == nil {
		lookup = net.DefaultResolver.LookupTXT
	}
	txts, err := lookup(ctx, name)
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return nil, nil
	}
	if len(txts) > maxDNSLinkTXT {
		txts = txts[:maxDNSLinkTXT]
	}
	return txts, err
}

// queryDoH asks an RFC 8484 endpoint (POST) for name's TXT records.
func (r *DNSLinkResolver) queryDoH(ctx context.Context, endpoint, name string) ([]string, error) {
	if r.Client == nil {
		return nil, errors.New("DNS-over-HTTPS needs an HTTP client")
	}
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(name), dns.TypeTXT)
	msg.Id = 0 // RFC 8484 §4.1: cache-friendly
	packet, err := msg.Pack()
	if err != nil {
		return nil, err
	}
	// POST: eth.limo refuses RFC 8484 GET queries.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(packet))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", dnsMessageType)
	req.Header.Set("Accept", dnsMessageType)
	resp, err := r.Client.Do(ctx, req, httpsrc.Options{MaxBytes: maxDoHResponse, Timeout: dnsLinkTimeout, IdleTimeout: dnsLinkTimeout})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if typ, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); resp.StatusCode != http.StatusOK || typ != dnsMessageType {
		return nil, fmt.Errorf("DNS-over-HTTPS %s: HTTP %d %s", endpoint, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	reply := new(dns.Msg)
	if err = reply.Unpack(b); err != nil {
		return nil, fmt.Errorf("DNS-over-HTTPS %s: %w", endpoint, err)
	}
	if !reply.Response || len(reply.Question) != 1 || !strings.EqualFold(reply.Question[0].Name, dns.Fqdn(name)) {
		return nil, fmt.Errorf("DNS-over-HTTPS %s: reply does not answer the question", endpoint)
	}
	switch reply.Rcode {
	case dns.RcodeSuccess:
	case dns.RcodeNameError:
		return nil, nil
	default:
		return nil, fmt.Errorf("DNS-over-HTTPS %s: %s", endpoint, dns.RcodeToString[reply.Rcode])
	}
	var out []string
	for _, rr := range reply.Answer {
		if t, ok := rr.(*dns.TXT); ok && strings.EqualFold(t.Hdr.Name, dns.Fqdn(name)) && len(out) < maxDNSLinkTXT {
			out = append(out, strings.Join(t.Txt, ""))
		}
	}
	return out, nil
}
