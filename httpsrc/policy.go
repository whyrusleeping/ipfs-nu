package httpsrc

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

func hostKey(s string) string { return strings.ToLower(strings.TrimSuffix(s, ".")) }

func (c *Client) checkURL(u *url.URL, hosts []string) error {
	if len(u.String()) > MaxURLBytes {
		return fmt.Errorf("%w: URL too long", ErrPolicy)
	}
	if (u.Scheme != "https" && !(c.cfg.AllowHTTP && u.Scheme == "http")) ||
		u.Hostname() == "" || u.User != nil || u.Opaque != "" || strings.Contains(u.Hostname(), "%") {
		return fmt.Errorf("%w: HTTPS URL without credentials or address zones required", ErrPolicy)
	}
	if hosts != nil {
		found := false
		for _, host := range hosts {
			if hostKey(host) == hostKey(u.Hostname()) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: host not allowlisted", ErrPolicy)
		}
	}
	// Reject forbidden literals before even entering the dial path.
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !c.allowedIP(ip) {
		return fmt.Errorf("%w: forbidden IP", ErrPolicy)
	}
	return nil
}

// Conservative treatment of special-use and transition ranges. IPv4-mapped
// addresses are rejected rather than unwrapped; NAT64's well-known /96 is checked
// by its embedded IPv4. The local-use NAT64 /48 and tunneling ranges are denied
// entirely because their mapping depends on network configuration.
var denied = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"), // deprecated IPv6 site-local addresses
}
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

func (c *Client) allowedIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	for _, prefix := range c.cfg.AllowedIPs {
		if prefix.Contains(ip) {
			return true
		}
	}
	return publicIP(ip)
}

func publicIP(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Is4In6() {
		return false
	}
	for _, prefix := range denied {
		if prefix.Contains(ip) {
			return false
		}
	}
	if nat64.Contains(ip) {
		b := ip.As16()
		return publicIP(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	return true
}

func (c *Client) dial(ctx context.Context, network, address string, idle time.Duration) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = c.cfg.LookupIP(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: no destination addresses", ErrPolicy)
	}
	// Fail closed on mixed public/private answers, before trying any connection.
	for _, ip := range ips {
		if !c.allowedIP(ip) {
			return nil, fmt.Errorf("%w: resolved IP %s", ErrPolicy, ip)
		}
	}
	for _, ip := range ips {
		var conn net.Conn
		dialCtx, cancel := context.WithTimeout(ctx, idle)
		conn, err = c.cfg.DialContext(dialCtx, network, net.JoinHostPort(ip.String(), port))
		cancel()
		if err == nil {
			return &idleConn{Conn: conn, idle: idle}, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, err
}

// Deadlines refresh per network operation, bounding TLS, headers and body stalls.
// HTTP/2 is intentionally not enabled: the deadline belongs to this one request.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}
func (c *idleConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

func origin(raw string) string {
	if len(raw) > MaxURLBytes {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Scheme + "://" + net.JoinHostPort(hostKey(u.Hostname()), port)
}

// ReportVerificationFailure demotes the final response origin for five minutes.
// At most 256 origins are retained; the earliest expiry is evicted on overflow.
// Callers must report Result.FinalURL, not the original redirecting URL.
func (c *Client) ReportVerificationFailure(rawURL string) {
	key := origin(rawURL)
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, until := range c.failures {
		if !until.After(now) {
			delete(c.failures, k)
		}
	}
	if _, exists := c.failures[key]; !exists && len(c.failures) >= 256 {
		var oldest string
		var earliest time.Time
		for k, until := range c.failures {
			if oldest == "" || until.Before(earliest) {
				oldest, earliest = k, until
			}
		}
		delete(c.failures, oldest)
	}
	c.failures[key] = now.Add(5 * time.Minute)
}

// Demoted is a scheduler hint: prefer other sources, but Fetch itself is not
// blocked. Successful downloads do not erase verification failures.
func (c *Client) Demoted(rawURL string) bool {
	key := origin(rawURL)
	c.mu.Lock()
	defer c.mu.Unlock()
	until := c.failures[key]
	if !until.After(time.Now()) {
		delete(c.failures, key)
		return false
	}
	return true
}
