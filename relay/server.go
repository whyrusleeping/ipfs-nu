package relay

import (
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/proto/autonat"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
)

// Config assembles the standalone relay role. Host.Key is the persisted relay
// identity. Default listen is UDP 443 on all IPv4 interfaces; supply concrete
// Server.Addresses when using wildcard listeners. No discovery/default public
// operator list, admission accounts, or content daemon is implicitly enabled.
type Config struct {
	Host    host.Config
	Server  circuit.ServerConfig
	Core    proto.Config
	AutoNAT autonat.Config
	Lookup  *LookupConfig // nil disables public dnsaddr/routing services
}
type Server struct {
	Host    *host.Host
	Circuit *circuit.Server
	Core    *proto.Service
	AutoNAT *autonat.Service
	Lookup  *LookupService
}

func New(cfg Config) (*Server, error) {
	if len(cfg.Host.Listen) == 0 && len(cfg.Host.PacketConns) == 0 {
		cfg.Host.Listen = []string{"0.0.0.0:443"}
	}
	if cfg.AutoNAT.Private {
		return nil, autonat.ErrPrivateMode
	}
	h, err := host.New(cfg.Host)
	if err != nil {
		return nil, err
	}
	s := &Server{Host: h}
	s.Core, err = proto.New(h, cfg.Core)
	if err != nil {
		h.Close()
		return nil, err
	}
	cfg.AutoNAT.Server = true
	s.AutoNAT, err = autonat.New(h, cfg.AutoNAT)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.Circuit, err = circuit.NewServer(h, cfg.Host.Key, cfg.Server)
	if err != nil {
		s.Close()
		return nil, err
	}
	if cfg.Lookup != nil {
		s.Lookup, err = NewLookup(*cfg.Lookup)
		if err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Server) Close() error {
	if s.Lookup != nil {
		s.Lookup.Close()
	}
	if s.Circuit != nil {
		s.Circuit.Close()
	}
	if s.AutoNAT != nil {
		s.AutoNAT.Close()
	}
	if s.Core != nil {
		s.Core.Close()
	}
	return s.Host.Close()
}
