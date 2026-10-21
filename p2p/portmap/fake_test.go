package portmap

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// The fake is also used in the opt-in netns lab, where install changes real nft
// DNAT rules before replying. It is not a substitute for third-party router CI.
type fakeGateway struct {
	udp, ssdp *net.UDPConn
	http      *http.Server
	listener  net.Listener
	epoch     atomic.Uint32
	maps      atomic.Int32
	corrupt   atomic.Bool
	lifetime  atomic.Uint32
	wg        sync.WaitGroup
	install   func(netip.AddrPort, uint16, uint32) error
	public    netip.Addr
}

func newFake(ip string, pxp, ssdp uint16, install func(netip.AddrPort, uint16, uint32) error) (*fakeGateway, error) {
	f := &fakeGateway{install: install, public: netip.MustParseAddr("198.18.0.20")}
	f.epoch.Store(1000)
	var err error
	f.udp, err = net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr(ip), pxp)))
	if err != nil {
		return nil, err
	}
	f.ssdp, err = net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr(ip), ssdp)))
	if err != nil {
		f.udp.Close()
		return nil, err
	}
	f.listener, err = net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		f.udp.Close()
		f.ssdp.Close()
		return nil, err
	}
	f.http = &http.Server{Handler: http.HandlerFunc(f.serveHTTP)}
	f.wg.Add(3)
	go func() { defer f.wg.Done(); _ = f.http.Serve(f.listener) }()
	go func() { defer f.wg.Done(); f.serveUDP() }()
	go func() {
		defer f.wg.Done()
		b := make([]byte, 8192)
		for {
			_, from, err := f.ssdp.ReadFromUDP(b)
			if err != nil {
				return
			}
			reply := fmt.Sprintf("HTTP/1.1 200 OK\r\nLOCATION: http://%s/root.xml\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n\r\n", f.listener.Addr())
			_, _ = f.ssdp.WriteToUDP([]byte(reply), from)
		}
	}()
	return f, nil
}
func (f *fakeGateway) close() { _ = f.udp.Close(); _ = f.ssdp.Close(); _ = f.http.Close(); f.wg.Wait() }
func (f *fakeGateway) config() Config {
	return Config{Gateway: netip.MustParseAddr("127.0.0.1"), Local: netip.MustParseAddrPort("127.0.0.1:4001"), ServerPort: f.udp.LocalAddr().(*net.UDPAddr).AddrPort().Port(), SSDPPort: f.ssdp.LocalAddr().(*net.UDPAddr).AddrPort().Port()}
}
func (f *fakeGateway) apply(local netip.AddrPort, external uint16, seconds uint32) error {
	if f.install != nil {
		if err := f.install(local, external, seconds); err != nil {
			return err
		}
	}
	if seconds > 0 {
		f.maps.Add(1)
	}
	return nil
}
func (f *fakeGateway) serveUDP() {
	b := make([]byte, maxPacket)
	for {
		n, from, err := f.udp.ReadFromUDPAddrPort(b)
		if err != nil {
			return
		}
		req := b[:n]
		var reply []byte
		if n >= 24 && req[0] == 2 {
			if req[1] == 0 {
				reply = make([]byte, 24)
			} else {
				want := 60
				if req[1] == 2 {
					want = 80
				}
				if n != want {
					continue
				}
				reply = append([]byte(nil), req...)
				local := netip.AddrPortFrom(netip.AddrFrom16([16]byte(req[8:24])).Unmap(), be.Uint16(req[40:42]))
				port := be.Uint16(req[42:44])
				if port == 0 {
					port = 40001
				}
				seconds := be.Uint32(req[4:8])
				if life := f.lifetime.Load(); life != 0 && seconds != 0 {
					seconds = life
				}
				if err = f.apply(local, port, seconds); err != nil {
					reply[3] = 2
				}
				be.PutUint32(reply[4:8], seconds)
				be.PutUint16(reply[42:44], port)
				external := f.public
				if local.Addr().Is6() {
					external = local.Addr()
				}
				ip := external.As16()
				copy(reply[44:60], ip[:])
				if f.corrupt.Load() {
					reply[24] ^= 1
				}
			}
			reply[0], reply[1] = 2, req[1]|128
			be.PutUint32(reply[8:12], f.epoch.Load())
		} else if n >= 2 && req[0] == 0 {
			switch req[1] {
			case 0:
				reply = make([]byte, 12)
				ip := f.public.As4()
				copy(reply[8:12], ip[:])
			case 1:
				if n != 12 {
					continue
				}
				reply = make([]byte, 16)
				local := be.Uint16(req[4:6])
				port := be.Uint16(req[6:8])
				if port == 0 {
					port = 40002
				}
				seconds := be.Uint32(req[8:12])
				if life := f.lifetime.Load(); life != 0 && seconds != 0 {
					seconds = life
				}
				if err = f.apply(netip.AddrPortFrom(from.Addr(), local), port, seconds); err != nil {
					reply[3] = 2
				}
				be.PutUint16(reply[8:10], local)
				be.PutUint16(reply[10:12], port)
				be.PutUint32(reply[12:16], seconds)
			default:
				continue
			}
			reply[1] = req[1] | 128
			be.PutUint32(reply[4:8], f.epoch.Load())
		} else {
			continue
		}
		_, _ = f.udp.WriteToUDPAddrPort(reply, from)
	}
}
func (f *fakeGateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	const service = "urn:schemas-upnp-org:service:WANIPConnection:1"
	if r.URL.Path == "/root.xml" {
		_, _ = io.WriteString(w, `<root><device><deviceList><device><serviceList><service><serviceType>`+service+`</serviceType><controlURL>/control</controlURL></service></serviceList></device></deviceList></device></root>`)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxXML+1))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	fields, _, err := xmlFields(body, maxXML, 32)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	action := strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("SOAPAction"), `"`+service+`#`), `"`)
	value := ""
	switch action {
	case "GetExternalIPAddress":
		value = "<NewExternalIPAddress>" + f.public.String() + "</NewExternalIPAddress>"
	case "GetStatusInfo":
		value = fmt.Sprintf("<NewUptime>%d</NewUptime>", f.epoch.Load())
	case "AddPortMapping", "DeletePortMapping":
		port, _ := strconv.ParseUint(fields["NewExternalPort"], 10, 16)
		seconds, _ := strconv.ParseUint(fields["NewLeaseDuration"], 10, 32)
		local := netip.MustParseAddrPort("10.0.0.2:4001")
		if action == "AddPortMapping" {
			n, _ := strconv.ParseUint(fields["NewInternalPort"], 10, 16)
			ip, e := netip.ParseAddr(fields["NewInternalClient"])
			if e != nil {
				http.Error(w, "bad client", 500)
				return
			}
			local = netip.AddrPortFrom(ip, uint16(n))
		}
		if err = f.apply(local, uint16(port), uint32(seconds)); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	default:
		http.Error(w, "unknown action", 500)
		return
	}
	_, _ = fmt.Fprintf(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:%sResponse xmlns:u="%s">%s</u:%sResponse></s:Body></s:Envelope>`, action, service, value, action)
}
