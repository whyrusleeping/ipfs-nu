package daemon

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/node/repo"
)

// startPublic starts the public HTTPS content listener when [public] is configured
// (backlog 0043; docs/operations.md P1–P6). It is a separate listener with its own
// handler (no /api/, P1) serving only roots marked with `nu public add` (P2). A member
// of a private or isolated cluster refuses it: this build has no reader authentication,
// and a members-only node must never expose content by listener misconfiguration (P3,
// N28). TLS comes from cert/key files the daemon serves itself, or from a reverse proxy
// on this machine (behind_proxy = true, loopback listen address only).
func (d *Daemon) startPublic(ctx context.Context, cfg *repo.Config) error {
	pc := cfg.Public
	if pc == nil || pc.Listen == "" {
		return nil
	}
	bad := func(format string, args ...any) *apiv0.Error {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, format, args...)
	}
	if d.Node.PrivateNetwork() {
		return bad("public.listen = %q: this node is a member of a private or isolated cluster, whose content is members-only", pc.Listen).
			WithAction("remove public.listen; public serving needs reader authentication, which this build does not have (P3)")
	}
	domain, err := gateway.ParseDomain(pc.Domain)
	if err != nil {
		return bad("public.domain: %v", err).
			WithAction("set the public name, e.g. `" + nu.Name + " config set public.domain seed.example` (with a wildcard DNS record and certificate for *.seed.example for site origins)")
	}
	var tlsFiles *gateway.CertFiles
	if pc.BehindProxy {
		if err := requireLoopback(pc.Listen); err != nil {
			return bad("public.listen = %q with public.behind_proxy: the plain-HTTP hop to the proxy must stay on this machine (%v)", pc.Listen, err).
				WithAction("listen on 127.0.0.1:<port> and point the TLS-terminating proxy for " + domain + " and *." + domain + " at it")
		}
	} else {
		if pc.Cert == "" || pc.Key == "" {
			return bad("public.listen = %q needs public.cert and public.key (PEM files), or public.behind_proxy = true", pc.Listen).
				WithAction("`" + nu.Name + " config set public.cert /path/fullchain.pem` and `... public.key /path/privkey.pem`")
		}
		tlsFiles, err = gateway.LoadCertFiles(pc.Cert, pc.Key)
		if err != nil {
			return bad("public.cert/public.key: %v", err).WithAction("the files must be a PEM certificate chain and its private key, readable by the daemon")
		}
	}
	opts, err := d.Node.EnablePublic(ctx, domain)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", pc.Listen)
	if err != nil {
		return apiv0.Errorf(apiv0.CodeUnavailable, "cannot listen for public serving on %s: %v", pc.Listen, err).
			WithAction("if another program uses the port, change public.listen; ports below 1024 need a capability or a proxy")
	}
	d.pubLn = ln
	d.pubSrv = &http.Server{
		Handler:           gateway.Limit(gateway.NewPublic(d.Node.Content, opts), pc.MaxRequests, pc.PerIP),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	if tlsFiles != nil {
		d.pubSrv.TLSConfig = tlsFiles.TLSConfig()
		go func() {
			err := d.pubSrv.ServeTLS(ln, "", "")
			if err == http.ErrServerClosed {
				err = nil
			}
			d.errs <- err
		}()
	} else {
		go d.serve(d.pubSrv, ln)
	}
	d.log.Info("public listener started", "addr", ln.Addr().String(), "origin", opts.Origin(), "tls", tlsFiles != nil, "behind_proxy", pc.BehindProxy)
	return nil
}

// PublicAddr returns the public listener's bound address ("" when none runs).
func (d *Daemon) PublicAddr() string {
	if d.pubLn == nil {
		return ""
	}
	return d.pubLn.Addr().String()
}
