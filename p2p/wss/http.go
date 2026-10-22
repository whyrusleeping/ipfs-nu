package wss

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" // RFC 6455's public upgrade challenge, not a security primitive.
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const MaxHeaderBytes = 16 << 10
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}
func token(h http.Header, key, value string) bool {
	for _, s := range h.Values(key) {
		for _, part := range strings.Split(s, ",") {
			if strings.EqualFold(strings.TrimSpace(part), value) {
				return true
			}
		}
	}
	return false
}

// DialOptions must come from operator configuration, never remote peer records.
// Proxy is an explicit http:// CONNECT proxy (optional Basic credentials in URL).
// TLSConfig must preserve Web-PKI verification; custom roots are useful for a
// private operator CA but must not trust a TLS-intercepting proxy. A trusted
// interception CA cannot be detected automatically; a certificate pin can be
// enforced with VerifyConnection. No environment proxy discovery is performed.
type DialOptions struct {
	TLSConfig *tls.Config
	Proxy     *url.URL
	Dialer    *net.Dialer
}

// Dial opens a wss:// URL using HTTP/1.1. The default total setup timeout is ten
// seconds; ctx can shorten it and cancellation interrupts every setup stage.
// Errors are stage-labelled and never include proxy credentials.
func Dial(ctx context.Context, endpoint string, opts DialOptions) (conn *Conn, err error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("wss: explicit wss endpoint required")
	}
	if opts.TLSConfig != nil && opts.TLSConfig.InsecureSkipVerify {
		return nil, errors.New("wss: insecure TLS verification forbidden")
	}
	target := u.Host
	if u.Port() == "" {
		target = net.JoinHostPort(u.Hostname(), "443")
	}
	address := target
	if p := opts.Proxy; p != nil {
		if p.Scheme != "http" || p.Hostname() == "" || p.Path != "" && p.Path != "/" || p.RawQuery != "" || p.Fragment != "" {
			return nil, errors.New("wss: only explicit HTTP CONNECT proxies supported")
		}
		address = p.Host
		if p.Port() == "" {
			address = net.JoinHostPort(p.Hostname(), "80")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d := opts.Dialer
	if d == nil {
		d = &net.Dialer{}
	}
	raw, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("wss: TCP dial: %w", err)
	}
	defer func() {
		if err != nil {
			raw.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	if err = raw.SetDeadline(deadline); err != nil {
		return nil, err
	}
	canceled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { raw.SetDeadline(time.Now()); close(canceled) })
	defer func() {
		if !stop() {
			<-canceled
		}
		if ctx.Err() != nil && err == nil {
			conn = nil
			err = ctx.Err()
		}
		if err == nil {
			err = raw.SetDeadline(time.Time{})
		}
	}()
	if opts.Proxy != nil {
		req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
		if user := opts.Proxy.User; user != nil {
			password, _ := user.Password()
			req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password)))
		}
		if err = req.Write(raw); err != nil {
			return nil, fmt.Errorf("wss: CONNECT write: %w", err)
		}
		limited := &io.LimitedReader{R: raw, N: MaxHeaderBytes}
		reader := bufio.NewReader(limited)
		res, e := http.ReadResponse(reader, req)
		if e != nil {
			return nil, fmt.Errorf("wss: CONNECT response (header limit %d): %w", MaxHeaderBytes, e)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("wss: CONNECT denied: HTTP %d", res.StatusCode)
		}
		// A proxy cannot send end-to-end TLS bytes before our ClientHello. Do not
		// silently drop bytes read ahead (e.g. a captive portal body).
		if reader.Buffered() != 0 {
			return nil, errors.New("wss: CONNECT unexpected tunnel bytes")
		}
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.TLSConfig != nil {
		cfg = opts.TLSConfig.Clone()
	}
	cfg.ServerName = u.Hostname()
	cfg.NextProtos = []string{"http/1.1"}
	cfg.MinVersion = max(cfg.MinVersion, tls.VersionTLS12)
	secure := tls.Client(raw, cfg)
	if err = secure.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("wss: TLS certificate/handshake (TLS interception unsupported): %w", err)
	}
	var challenge [16]byte
	if _, err = rand.Read(challenge[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(challenge[:])
	req := &http.Request{Method: http.MethodGet, URL: u, Host: u.Host, Header: make(http.Header)}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	if err = req.Write(secure); err != nil {
		return nil, fmt.Errorf("wss: Upgrade write: %w", err)
	}
	limited := &io.LimitedReader{R: secure, N: MaxHeaderBytes}
	reader := bufio.NewReader(limited)
	res, e := http.ReadResponse(reader, req)
	if e != nil {
		return nil, fmt.Errorf("wss: Upgrade response (header limit %d): %w", MaxHeaderBytes, e)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("wss: Upgrade denied: HTTP %d (WebSocket-capable endpoint required)", res.StatusCode)
	}
	if !token(res.Header, "Connection", "upgrade") || !token(res.Header, "Upgrade", "websocket") || res.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) || res.Header.Get("Sec-WebSocket-Extensions") != "" || res.Header.Get("Sec-WebSocket-Protocol") != "" {
		return nil, errors.New("wss: invalid Upgrade response or unrequested extension")
	}
	limited.N = math.MaxInt64
	return &Conn{Conn: secure, reader: reader}, nil
}

// Upgrade accepts a TLS HTTP/1.1 WebSocket request and hijacks its connection.
// The owner MUST bound server connections (including TLS handshakes), set
// ReadHeaderTimeout/MaxHeaderBytes, and own/close the hijacked connections.
// Requests with Origin are rejected: browser-native p2p is not this profile.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	reject := func(status int, msg string) (*Conn, error) {
		http.Error(w, msg, status)
		return nil, errors.New("wss: " + msg)
	}
	if r.TLS == nil {
		return reject(http.StatusBadRequest, "TLS required")
	}
	if r.Method != http.MethodGet || r.ProtoMajor != 1 || r.ProtoMinor != 1 || !token(r.Header, "Connection", "upgrade") || !token(r.Header, "Upgrade", "websocket") || r.Header.Get("Sec-WebSocket-Version") != "13" || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		return reject(http.StatusBadRequest, "invalid WebSocket Upgrade")
	}
	if r.Header.Get("Origin") != "" {
		return reject(http.StatusForbidden, "browser origins unsupported")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(decoded) != 16 {
		return reject(http.StatusBadRequest, "invalid WebSocket challenge")
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return reject(http.StatusInternalServerError, "HTTP hijacking unavailable")
	}
	raw, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	if _, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", acceptKey(key)); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		raw.Close()
		return nil, err
	}
	return &Conn{Conn: raw, reader: rw.Reader, server: true}, nil
}
