package lookup

import (
	"context"
	"io"
	"time"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// SealPublishedRecord filters before signing. RelayOnly never publishes direct
// addresses or a dnsaddr indirection which could resolve to a direct address.
func SealPublishedRecord(key *wire.NodeKey, seq uint64, addresses []wire.Multiaddr, relayOnly bool) ([]byte, error) {
	if key == nil || len(addresses) > 32 {
		return nil, wire.ErrLimit
	}
	record := wire.PeerRecord{PeerID: key.Public().PeerID(), Seq: seq}
	seen := map[string]bool{}
	for _, a := range addresses {
		a, err := NormalizeAddress(record.PeerID, a)
		if err != nil {
			return nil, err
		}
		if relayOnly {
			m, err := ma.NewMultiaddrBytes(a.Bytes())
			if err != nil {
				return nil, err
			}
			parts := ma.Split(m)
			if len(parts) < 4 || parts[len(parts)-2].Protocol().Code != ma.P_CIRCUIT || parts[len(parts)-3].Protocol().Code != ma.P_P2P {
				continue
			}
		}
		if !seen[a.String()] {
			seen[a.String()] = true
			record.Addresses = append(record.Addresses, a)
		}
	}
	return record.Seal(key, PeerRecordLimits())
}

type PublisherConfig struct {
	Client    *httpsrc.Client
	BaseURL   string
	Hosts     []string
	Key       *wire.NodeKey
	Addresses func() []wire.Multiaddr
	// NextSequence must advance across restarts (normally a persisted counter or
	// libp2p's persisted monotonic timestamp). No wall-clock/replay assumption here.
	NextSequence    func() (uint64, error)
	RelayOnly       bool
	Timeout         time.Duration // default 3s
	RefreshInterval time.Duration // default 30m, must be below server retention
}
type Publisher struct{ cfg PublisherConfig }

func NewPublisher(cfg PublisherConfig) (*Publisher, error) {
	if cfg.Client == nil || cfg.Key == nil || cfg.Addresses == nil || cfg.NextSequence == nil {
		return nil, wire.ErrFormat
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 30 * time.Minute
	}
	if cfg.Timeout <= 0 || cfg.RefreshInterval <= 0 {
		return nil, wire.ErrLimit
	}
	var err error
	cfg.BaseURL, err = endpointBase(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.Hosts != nil {
		cfg.Hosts = append([]string{}, cfg.Hosts...)
	}
	return &Publisher{cfg}, nil
}
func (p *Publisher) Publish(ctx context.Context) error {
	seq, err := p.cfg.NextSequence()
	if err != nil {
		return err
	}
	record, err := SealPublishedRecord(p.cfg.Key, seq, p.cfg.Addresses(), p.cfg.RelayOnly)
	if err != nil {
		return err
	}
	_, err = p.cfg.Client.Put(ctx, p.cfg.BaseURL+PublishPath+p.cfg.Key.Public().PeerID().String(), record, func(int64) (io.Writer, error) { return io.Discard, nil }, httpsrc.Options{MaxBytes: 8192, Timeout: p.cfg.Timeout, IdleTimeout: p.cfg.Timeout, Hosts: p.cfg.Hosts})
	return err
}

// Run publishes initially, on coalesced address-change notifications, and before
// retention expires. Errors return to the owner for retry/backoff; closed changes
// stops the loop. Host.WatchChanges can supply the channel without an import cycle.
func (p *Publisher) Run(ctx context.Context, changes <-chan struct{}) error {
	if err := p.Publish(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(p.cfg.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-changes:
			if !ok {
				return nil
			}
		case <-ticker.C:
		}
		if err := p.Publish(ctx); err != nil {
			return err
		}
	}
}
