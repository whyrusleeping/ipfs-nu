package follow

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// Jetstream is the optional update-hint source of docs/follow.md "Update notification"
// (2): a subscription to a Jetstream instance restricted to the followed DIDs and the
// io.ipfs.nu.name collection. Events are unsigned hints; OnEvent must re-resolve the name
// from its PDS with a proof. It is not a feed ingest: nothing from the stream is stored.
type Jetstream struct {
	// URL is the subscribe endpoint (ws:// or wss://), e.g.
	// wss://jetstream2.us-east.bsky.network/subscribe.
	URL string
	// Dial connects to host:port (tests route it; nil uses net.Dialer with TLS for wss).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// OnEvent receives each commit event for a followed name (did, name, rev, deleted).
	OnEvent func(did, name, rev string, deleted bool)
	// Backoff bounds reconnect waits (default 1 s doubling to 1 min).
	Backoff time.Duration

	mu     sync.Mutex
	dids   []string
	cancel context.CancelFunc
	// Connects counts connections made (tests).
	Connects int
}

// Collection is the only collection subscribed.
const Collection = "io.ipfs.nu.name"

// MaxEventBytes bounds one event frame.
const MaxEventBytes = 64 << 10

// Subscribe sets the followed DIDs; a changed set reconnects with the new filter. An
// empty set disconnects. Run must be running for events to flow.
func (j *Jetstream) Subscribe(dids []string) {
	sorted := append([]string(nil), dids...)
	sort.Strings(sorted)
	j.mu.Lock()
	defer j.mu.Unlock()
	if strings.Join(sorted, ",") == strings.Join(j.dids, ",") {
		return
	}
	j.dids = sorted
	if j.cancel != nil {
		j.cancel() // Run reconnects with the new set
	}
}

// Run keeps a subscription open for the current DID set until ctx ends.
func (j *Jetstream) Run(ctx context.Context) {
	wait := j.Backoff
	if wait == 0 {
		wait = time.Second
	}
	delay := wait
	for ctx.Err() == nil {
		j.mu.Lock()
		dids := j.dids
		cctx, cancel := context.WithCancel(ctx)
		j.cancel = cancel
		j.mu.Unlock()
		if len(dids) == 0 {
			<-cctx.Done()
			continue
		}
		err := j.connect(cctx, dids)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil || errors.Is(err, context.Canceled) {
			delay = wait // the set changed: reconnect at once
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Minute)
	}
}

type event struct {
	DID    string `json:"did"`
	Kind   string `json:"kind"`
	Commit *struct {
		Rev        string `json:"rev"`
		Operation  string `json:"operation"`
		Collection string `json:"collection"`
		RKey       string `json:"rkey"`
	} `json:"commit"`
}

func (j *Jetstream) connect(ctx context.Context, dids []string) error {
	u, err := url.Parse(j.URL)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("wantedCollections", Collection)
	q.Del("wantedDids")
	for _, d := range dids {
		q.Add("wantedDids", d)
	}
	u.RawQuery = q.Encode()
	cfg, err := websocket.NewConfig(u.String(), "http://localhost/")
	if err != nil {
		return err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	dial := j.Dial
	if dial == nil {
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if u.Scheme == "wss" {
				return (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 30 * time.Second}}).DialContext(ctx, network, addr)
			}
			return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, addr)
		}
	}
	conn, err := dial(ctx, "tcp", host)
	if err != nil {
		return err
	}
	ws, err := websocket.NewClient(cfg, conn)
	if err != nil {
		conn.Close()
		return err
	}
	ws.MaxPayloadBytes = MaxEventBytes
	j.mu.Lock()
	j.Connects++
	j.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { ws.Close() })
	defer stop()
	defer ws.Close()
	for {
		var raw []byte
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		var ev event
		if json.Unmarshal(raw, &ev) != nil || ev.Kind != "commit" || ev.Commit == nil || ev.Commit.Collection != Collection || ev.DID == "" || ev.Commit.RKey == "" {
			continue
		}
		if j.OnEvent != nil {
			j.OnEvent(ev.DID, ev.Commit.RKey, ev.Commit.Rev, ev.Commit.Operation == "delete")
		}
	}
}
