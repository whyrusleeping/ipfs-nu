// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause
// Adapted from tailscale.com v1.104.2 net/portmapper/upnp.go; see NOTICE.
// Nu uses bounded stdlib SSDP/XML/SOAP instead of goupnp and constrains all HTTP
// requests to the explicitly selected gateway, with no DNS, proxies or redirects.
package portmap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxXML = 64 << 10

type upnpService struct{ Type, URL string }

func gatewayURL(raw string, gw netip.Addr) (*url.URL, error) {
	if len(raw) > 4096 {
		return nil, ErrProtocol
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || ip != gw || u.Scheme != "http" || u.User != nil || u.Opaque != "" {
		return nil, ErrProtocol
	}
	return u, nil
}
func (c *Client) request(ctx context.Context, method, raw, action, body string) ([]byte, error) {
	u, err := gatewayURL(raw, c.cfg.Gateway)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 8192,
		DialContext: (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(c.cfg.Local.Addr().AsSlice()), Zone: c.cfg.Local.Addr().Zone()}}).DialContext}
	defer tr.CloseIdleConnections()
	hc := http.Client{Transport: tr, Timeout: c.cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrProtocol }}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if action != "" {
		req.Header.Set("SOAPAction", strconv.Quote(action))
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxXML+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxXML {
		return nil, ErrProtocol
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: UPnP HTTP %d", ErrProtocol, resp.StatusCode)
	}
	return b, nil
}
func parseSSDP(b []byte, maxBytes int) (string, error) {
	if len(b) > maxBytes {
		return "", ErrProtocol
	}
	r, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(b)), nil)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("Location") == "" {
		return "", ErrProtocol
	}
	return r.Header.Get("Location"), nil
}

// xmlFields extracts leaf fields and service records, with bounded bytes, nesting
// and token count. No DTD/entity extensions or recursive unmarshalling are used.
func xmlFields(b []byte, maxBytes, maxDepth int) (map[string]string, []map[string]string, error) {
	if len(b) > maxBytes {
		return nil, nil, ErrProtocol
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	fields := map[string]string{}
	var services []map[string]string
	var names []string
	var text []*strings.Builder
	var service map[string]string
	roots := 0
	for count := 0; ; count++ {
		if count > maxBytes {
			return nil, nil, ErrProtocol
		}
		tok, err := d.Token()
		if err == io.EOF {
			if len(names) != 0 || roots != 1 {
				return nil, nil, ErrProtocol
			}
			return fields, services, nil
		}
		if err != nil {
			return nil, nil, err
		}
		switch v := tok.(type) {
		case xml.StartElement:
			if len(names) == 0 {
				roots++
				if roots > 1 {
					return nil, nil, ErrProtocol
				}
			}
			if len(names) >= maxDepth {
				return nil, nil, ErrProtocol
			}
			names = append(names, v.Name.Local)
			text = append(text, &strings.Builder{})
			if v.Name.Local == "service" {
				if service != nil {
					return nil, nil, ErrProtocol
				}
				service = map[string]string{}
			}
		case xml.CharData:
			if len(text) > 0 {
				_, _ = text[len(text)-1].Write(v)
			}
		case xml.EndElement:
			i := len(names) - 1
			if i < 0 {
				return nil, nil, ErrProtocol
			}
			value := strings.TrimSpace(text[i].String())
			fields[names[i]] = value
			if service != nil {
				service[names[i]] = value
			}
			if names[i] == "service" {
				services = append(services, service)
				service = nil
			}
			names = names[:i]
			text = text[:i]
		case xml.Directive:
			return nil, nil, ErrProtocol
		}
	}
}
func (c *Client) discoverUPnP(ctx context.Context) error {
	req := []byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n\r\n")
	b, err := c.exchange(ctx, c.cfg.SSDPPort, req, 8192)
	if err != nil {
		return err
	}
	location, err := parseSSDP(b, 8192)
	if err != nil {
		return err
	}
	base, err := gatewayURL(location, c.cfg.Gateway)
	if err != nil {
		return err
	}
	b, err = c.request(ctx, http.MethodGet, base.String(), "", "")
	if err != nil {
		return err
	}
	fields, services, err := xmlFields(b, maxXML, 32)
	if err != nil {
		return err
	}
	if fields["URLBase"] != "" {
		base, err = gatewayURL(fields["URLBase"], c.cfg.Gateway)
		if err != nil {
			return err
		}
	}
	for _, s := range services {
		typ := s["serviceType"]
		if typ != "urn:schemas-upnp-org:service:WANIPConnection:1" && typ != "urn:schemas-upnp-org:service:WANIPConnection:2" && typ != "urn:schemas-upnp-org:service:WANPPPConnection:1" {
			continue
		}
		ref, err := url.Parse(s["controlURL"])
		if err != nil || s["controlURL"] == "" {
			continue
		}
		target := base.ResolveReference(ref)
		if _, err = gatewayURL(target.String(), c.cfg.Gateway); err != nil {
			return err
		}
		c.service = &upnpService{Type: typ, URL: target.String()}
		return nil
	}
	return fmt.Errorf("%w: no WAN connection service", ErrProtocol)
}

type soapArg struct{ name, value string }

func (c *Client) soap(ctx context.Context, action string, args ...soapArg) (map[string]string, error) {
	if c.service == nil {
		if err := c.discoverUPnP(ctx); err != nil {
			return nil, err
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:%s xmlns:u="%s">`, action, c.service.Type)
	for _, a := range args {
		fmt.Fprintf(&b, "<%s>", a.name)
		_ = xml.EscapeText(&b, []byte(a.value))
		fmt.Fprintf(&b, "</%s>", a.name)
	}
	fmt.Fprintf(&b, "</u:%s></s:Body></s:Envelope>", action)
	data, err := c.request(ctx, http.MethodPost, c.service.URL, c.service.Type+"#"+action, b.String())
	if err != nil {
		return nil, err
	}
	fields, _, err := xmlFields(data, maxXML, 32)
	if err != nil {
		return nil, err
	}
	if _, ok := fields[action+"Response"]; !ok {
		return nil, ErrProtocol
	}
	return fields, nil
}
func (c *Client) upnpEpoch(ctx context.Context) (uint32, error) {
	fields, err := c.soap(ctx, "GetStatusInfo")
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(fields["NewUptime"], 10, 32)
	return uint32(n), err
}
func (c *Client) mapUPnP(ctx context.Context, old Mapping, seconds uint32, start time.Time) (Mapping, error) {
	port := c.cfg.Local.Port()
	if old.Protocol == UPnP && old.External.Port() != 0 {
		port = old.External.Port()
	}
	args := []soapArg{{"NewRemoteHost", ""}, {"NewExternalPort", strconv.Itoa(int(port))}, {"NewProtocol", "UDP"}}
	if seconds == 0 {
		_, err := c.soap(ctx, "DeletePortMapping", args...)
		return Mapping{}, err
	}
	fields, err := c.soap(ctx, "GetExternalIPAddress")
	if err != nil {
		return Mapping{}, err
	}
	ip, err := netip.ParseAddr(fields["NewExternalIPAddress"])
	if err != nil || !ip.Is4() {
		return Mapping{}, ErrProtocol
	}
	// Never use external port 0 (UPnP v1 interprets that as an all-ports wildcard).
	args = append(args, soapArg{"NewInternalPort", strconv.Itoa(int(c.cfg.Local.Port()))}, soapArg{"NewInternalClient", c.cfg.Local.Addr().String()}, soapArg{"NewEnabled", "1"}, soapArg{"NewPortMappingDescription", "UDP peer mapping"}, soapArg{"NewLeaseDuration", strconv.FormatUint(uint64(seconds), 10)})
	if _, err = c.soap(ctx, "AddPortMapping", args...); err != nil {
		return Mapping{}, err
	}
	epoch, err := c.upnpEpoch(ctx)
	if err != nil {
		return Mapping{}, err
	}
	return lease(netip.AddrPortFrom(ip, port), epoch, seconds, start), nil
}
