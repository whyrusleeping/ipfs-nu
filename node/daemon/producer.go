package daemon

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/token"
)

// startProducer starts the producer listener when api.producer.listen is set: the item
// push routes for producers on other machines, authorized by push tokens (rule A8 of
// docs/operations.md). This build has no TLS, so the operator must declare the network
// trusted (api.producer.trusted_network = true), and the Host header must be one of
// api.producer.hosts. It is never the API or the content listener.
func (d *Daemon) startProducer(ctx context.Context, opts Options) error {
	r := d.Node.Repo
	addr := opts.ProducerListen
	if addr == "" {
		v, _ := r.ConfigGet("api.producer.listen")
		addr, _ = v.(string)
	}
	if addr == "" {
		return nil
	}
	trusted, _ := r.ConfigGet("api.producer.trusted_network")
	if t, _ := trusted.(bool); !t {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "api.producer.listen = %q needs api.producer.trusted_network = true: this build sends push tokens without TLS", addr).
			WithAction("run producers over a VPN or a network you trust and set `" + nu.Name + " config set api.producer.trusted_network true`, or remove api.producer.listen")
	}
	var hosts []string
	if v, err := r.ConfigGet("api.producer.hosts"); err == nil {
		list, _ := v.([]any)
		for _, x := range list {
			if s, ok := x.(string); ok && s != "" {
				hosts = append(hosts, s)
			}
		}
	}
	if len(hosts) == 0 {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "api.producer.listen needs api.producer.hosts, the names producers use to reach this node").
			WithAction("e.g. `" + nu.Name + ` config set api.producer.hosts '["nu1.lan", "10.0.0.5"]'` + "`")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return listenError("API", addr, err)
	}
	dir := r.Path(token.Dir)
	h := api.NewProducer(d.Node, api.ProducerConfig{Hosts: hosts, Authorize: func(secret string) ([]string, bool) {
		t, ok := token.Lookup(dir, secret)
		if !ok || t.Scope != token.ScopePush {
			return nil, false
		}
		return t.Collections, true
	}}, d.log)
	d.prodLn = ln
	d.prodSrv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go d.serve(d.prodSrv, ln)
	d.log.Info("producer listener started", "addr", ln.Addr().String(), "hosts", hosts)
	return nil
}

// ProducerAddr returns the producer listener's address ("" when none runs).
func (d *Daemon) ProducerAddr() string {
	if d.prodLn == nil {
		return ""
	}
	return d.prodLn.Addr().String()
}
