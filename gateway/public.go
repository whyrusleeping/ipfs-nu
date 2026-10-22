package gateway

import (
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/target"
)

// Public listener (backlog 0043; docs/operations.md P1–P6). The same handler serves it
// with these differences: Host must be the configured domain or <cid>.<domain>; only
// roots the operator marked public are served (anything else reads as unheld); site
// origins are https://<cid>.<domain>/; /at/ does not exist; RASL answers add
// Access-Control-Allow-Origin: *. The control API is never on this listener (P1): the
// handler has no /api/ route at all.

// PublicOptions configures the public listener's handler.
type PublicOptions struct {
	// Domain is the public host, lowercase, optionally with :port when it is not 443
	// (ParseDomain validates it). Sites live on <cid>.<Domain>.
	Domain string
	// Serves reports whether a CID is under a root the operator made public. It must be
	// cheap: it runs on every request.
	Serves func(cid string) bool
}

// NewPublic returns the public listener's handler.
func NewPublic(content Content, opts PublicOptions) *Handler {
	h := New(content)
	h.pub = &opts
	return h
}

// ParseDomain validates a public.domain value: a DNS name with at least two labels (no
// IP literal, no localhost, no scheme or path), optionally :port; it returns the
// lowercase form without a redundant :443.
func ParseDomain(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	host, port := s, ""
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	}
	if host == "" || strings.ContainsAny(host, "/:?#@ \t") || net.ParseIP(host) != nil || !strings.Contains(host, ".") ||
		strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", fmt.Errorf("%q is not a public DNS name like seed.example", s)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("%q is not a public DNS name like seed.example", s)
		}
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("%q has an invalid port", s)
		}
		if port != "443" {
			return host + ":" + port, nil
		}
	}
	return host, nil
}

// Origin is the bare origin, https://<domain>.
func (o *PublicOptions) Origin() string { return "https://" + o.Domain }

// SiteURL is the site origin of a root with a trailing slash: https://<cid>.<domain>/.
func (o *PublicOptions) SiteURL(cid string) string {
	host, port, err := net.SplitHostPort(o.Domain)
	if err != nil {
		return "https://" + cid + "." + o.Domain + "/"
	}
	return "https://" + cid + "." + host + ":" + port + "/"
}

// parseHost accepts exactly the configured domain or <cid>.<domain> (a trailing dot
// and an explicit :443 are normalized away); anything else is 403.
func (o *PublicOptions) parseHost(r *http.Request) (request, error) {
	h := strings.ToLower(strings.TrimSuffix(r.Host, "."))
	if host, port, err := net.SplitHostPort(h); err == nil {
		h = strings.TrimSuffix(host, ".")
		if port != "443" {
			h += ":" + port
		}
	}
	rq := request{pub: o}
	if h == o.Domain {
		return rq, nil
	}
	if label, ok := strings.CutSuffix(h, "."+o.Domain); ok && !strings.Contains(label, ".") && target.IsCID(label) {
		rq.site = label
		return rq, nil
	}
	return rq, apiv0.Errorf(apiv0.CodeForbidden, "host %q is not %s or <cid>.%s", r.Host, o.Domain, o.Domain).
		WithAction("open " + o.Origin() + "/tree/<cid>/ or " + o.SiteURL("<cid>"))
}

var publicRootPage = template.Must(template.New("public").Parse(`<!doctype html>
<meta charset="utf-8">
<title>{{.Domain}}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 1em}code{background:#eee;padding:0 .3em}</style>
<h1>{{.Domain}}</h1>
<p>A {{.Name}} seed ({{.Version}}). It serves content its operator chose to publish, by
content identifier (CID); there is no index.</p>
<ul>
<li><code>/tree/&lt;cid&gt;/…</code> — listings and downloads of a published tree</li>
<li><code>/blob/&lt;cid&gt;</code> — one published blob, as a download</li>
<li><code>/.well-known/rasl/&lt;cid&gt;</code> — one published object's bytes (DASL RASL)</li>
<li><code>https://&lt;cid&gt;.{{.Domain}}/…</code> — a published tree as a site, on its own origin</li>
</ul>
`))

// Limit bounds concurrent requests on the public listener: total in flight and per
// client address (from RemoteAddr; behind a reverse proxy that is the proxy's address,
// so the per-address bound then acts as a second global one). Excess requests are
// answered 503 with Retry-After before any content work. A response whose client stops
// reading is cut off after StallTimeout. Zero values use the defaults.
func Limit(h http.Handler, total, perIP int) http.Handler {
	if total <= 0 {
		total = DefaultMaxRequests
	}
	if perIP <= 0 {
		perIP = DefaultPerIP
	}
	l := &limiter{h: h, sem: make(chan struct{}, total), perIP: perIP, ips: map[string]int{}}
	return l
}

// Default bounds of Limit.
const (
	DefaultMaxRequests = 256
	DefaultPerIP       = 32
)

type limiter struct {
	h     http.Handler
	sem   chan struct{}
	perIP int
	mu    sync.Mutex
	ips   map[string]int
}

func (l *limiter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	select {
	case l.sem <- struct{}{}:
	default:
		busy(w)
		return
	}
	defer func() { <-l.sem }()
	l.mu.Lock()
	n := l.ips[ip]
	if n >= l.perIP {
		l.mu.Unlock()
		busy(w)
		return
	}
	l.ips[ip] = n + 1
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.ips[ip] <= 1 {
			delete(l.ips, ip)
		} else {
			l.ips[ip]--
		}
		l.mu.Unlock()
	}()
	l.h.ServeHTTP(&stallWriter{ResponseWriter: w, rc: http.NewResponseController(w)}, r)
}

// StallTimeout is how long a public response may wait for the client to take the next
// write before the connection is closed (a minimum read rate of one ~32 KiB write per
// StallTimeout), so slow readers cannot hold the limiter's slots indefinitely (0077).
var StallTimeout = 30 * time.Second

// stallWriter extends the write deadline before each write.
type stallWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (s *stallWriter) Write(b []byte) (int, error) {
	_ = s.rc.SetWriteDeadline(time.Now().Add(StallTimeout))
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection's writer.
func (s *stallWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeError(w, apiv0.Errorf(apiv0.CodeUnavailable, "too many requests in flight").WithAction("retry shortly"))
}

// CertFiles serves a certificate from PEM files, re-reading them when either changes
// on disk (checked at most once per CertRecheck), so renewals apply without a restart.
// A failed reload keeps the last good certificate.
type CertFiles struct {
	CertPath, KeyPath string

	mu      sync.Mutex
	cert    *tls.Certificate
	checked time.Time
	mtime   time.Time
}

// CertRecheck is how often CertFiles stats its files.
const CertRecheck = time.Minute

// LoadCertFiles loads the pair once (failing early on a bad configuration).
func LoadCertFiles(certPath, keyPath string) (*CertFiles, error) {
	c := &CertFiles{CertPath: certPath, KeyPath: keyPath}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *CertFiles) stamp() (time.Time, error) {
	var latest time.Time
	for _, p := range []string{c.CertPath, c.KeyPath} {
		fi, err := os.Stat(p)
		if err != nil {
			return latest, err
		}
		if fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest, nil
}

func (c *CertFiles) reload() error {
	mt, err := c.stamp()
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(c.CertPath, c.KeyPath)
	if err != nil {
		return err
	}
	c.cert, c.mtime, c.checked = &cert, mt, time.Now()
	return nil
}

// GetCertificate is for tls.Config.GetCertificate.
func (c *CertFiles) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) >= CertRecheck {
		c.checked = time.Now()
		if mt, err := c.stamp(); err == nil && mt.After(c.mtime) {
			_ = c.reload() // keep the last good pair on failure
		}
	}
	if c.cert == nil {
		return nil, errors.New("no certificate loaded")
	}
	return c.cert, nil
}

// TLSConfig is the public listener's TLS configuration: TLS 1.2+, HTTP/2 and HTTP/1.1.
func (c *CertFiles) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: c.GetCertificate, NextProtos: []string{"h2", "http/1.1"}}
}
