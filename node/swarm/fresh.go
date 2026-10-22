package swarm

import (
	"context"
	"errors"
	"log/slog"

	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// The fresh client of an import cutover check (docs/migration.md): a new key and an empty
// store, so retrieval proves the network serves the content, not the local store.
func init() { node.FreshClient = startFresh }

type fresh struct {
	*Swarm
	st *store.Store
}

func (f *fresh) Store() *store.Store { return f.st }

func (f *fresh) Close() error { return errors.Join(f.Swarm.Close(), f.st.Close()) }

func startFresh(ctx context.Context, dir string) (node.FreshFetcher, error) {
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		return nil, err
	}
	key, err := wire.GenerateNodeKey()
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	cfg := Config{Key: key, Store: st, Logger: slog.New(slog.DiscardHandler),
		Listen: []string{"/ip4/0.0.0.0/udp/0/quic-v1", "/ip6/::/udp/0/quic-v1"}}
	sw, err := Start(ctx, cfg)
	if err != nil { // no IPv6 here
		cfg.Listen = cfg.Listen[:1]
		sw, err = Start(ctx, cfg)
	}
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return &fresh{Swarm: sw, st: st}, nil
}
