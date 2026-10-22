package collection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// Defaults (docs/collections.md).
const (
	DefaultReleaseDelay = time.Minute
	DefaultHeadInterval = 10 * time.Second
	DefaultMaxLog       = 1_000_000
	DefaultWorkers      = 4
)

// Holding states and sources.
const (
	StateFetching = "fetching"
	StatePartial  = "partial"
	StateComplete = "complete"
	StateReleased = "released"
	StatePaused   = "paused" // matched, but the rule's budget is exhausted (not a holding)

	SrcSelect = "select"
	SrcRescue = "rescue"
	SrcManual = "manual"
	SrcWriter = "writer"
	// SrcFloor holds an item this member owns by floor placement (replicas > 0);
	// SrcSurplus a complete floor holding it no longer owns, kept until release
	// evidence (docs/operations.md S; release.auto is off).
	SrcFloor   = "floor"
	SrcSurplus = "surplus"
)

// Config configures a Manager.
type Config struct {
	DBPath  string
	Store   *store.Store
	Cluster *cluster.Cluster
	Self    wire.PeerID
	// FetchCatalog fetches a catalog (header, containers, metadata) and FetchItem an
	// item's whole tree; nil when offline (only held content is used).
	FetchCatalog func(ctx context.Context, root cid.CID) error
	FetchItem    func(ctx context.Context, root cid.CID) error
	// Service returns the running cluster service (holdings pulls, peer freshness), or nil.
	Service func() *cluster.Service
	// Index runs the holdings index (it also runs whenever a rescue rule needs it).
	Index        bool
	Interval     time.Duration // reconcile period and heartbeat unit (cluster interval)
	ReleaseDelay time.Duration // gc.release_delay
	HeadInterval time.Duration // head.min_interval
	MaxLog       int           // holdings-log bound per collection
	Workers      int           // concurrent item fetches
	// GC sweeps released content (a store GC); nil disables sweeping.
	GC func(ctx context.Context)
	// Changed is called (without locks) when this node's floor completion reports change.
	Changed func()
	// HTTP fetches item URL files (nil: pushes with URL files are refused); IngestDir
	// keeps partial URL downloads across restarts; IngestWorkers bounds concurrent pushes
	// being stored and IngestBackoff is the first retry delay (doubling to 10 min).
	HTTP          *httpsrc.Client
	IngestDir     string
	IngestWorkers int
	IngestBackoff time.Duration

	Now    func() time.Time
	Logger *slog.Logger
}

// Manager runs the collections of one cluster on this node: catalog sync and union merge,
// facet index, selection, holdings and holdings log, the holdings index, rescue and
// coalesced writer head writes.
type Manager struct {
	cfg   Config
	db    *sql.DB
	epoch string
	log   *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	kick   chan struct{}
	fkick  chan struct{}

	mu        sync.Mutex // serializes reconcile passes and head writes
	fmu       sync.Mutex
	inflight  map[string]bool
	gcPending bool
	lastFlush map[string]time.Time
	// pmu serializes push-row updates; pushCh is closed (and replaced) on push changes;
	// ikick wakes the ingest loop.
	pmu    sync.Mutex
	pushCh chan struct{}
	ikick  chan struct{}
	// ownKeys overrides heldKeys in tests.
	ownKeys func() map[string]bool
	// repMu guards reports, this node's floor completion reports by collection.
	repMu   sync.Mutex
	reports map[string]cluster.Report
}

// Open opens (creating) the collection database. Start runs the background work.
func Open(cfg Config) (*Manager, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = cluster.DefaultInterval
	}
	if cfg.ReleaseDelay <= 0 {
		cfg.ReleaseDelay = DefaultReleaseDelay
	}
	if cfg.HeadInterval <= 0 {
		cfg.HeadInterval = DefaultHeadInterval
	}
	if cfg.MaxLog <= 0 {
		cfg.MaxLog = DefaultMaxLog
	}
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.IngestWorkers <= 0 {
		cfg.IngestWorkers = DefaultIngestWorkers
	}
	if cfg.IngestBackoff <= 0 {
		cfg.IngestBackoff = DefaultIngestBackoff
	}
	if cfg.IngestDir == "" {
		cfg.IngestDir = filepath.Join(filepath.Dir(cfg.DBPath), "ingest")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	db, epoch, err := openDB(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	m := &Manager{cfg: cfg, db: db, epoch: epoch, log: cfg.Logger.With("component", "collections"),
		kick: make(chan struct{}, 1), fkick: make(chan struct{}, 1), inflight: map[string]bool{}, lastFlush: map[string]time.Time{},
		pushCh: make(chan struct{}), ikick: make(chan struct{}, 1), reports: map[string]cluster.Report{}}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m, nil
}

// Start runs reconciliation (on cluster changes and every interval) and item fetches.
func (m *Manager) Start() {
	m.cfg.Cluster.OnChange(m.Kick)
	m.wg.Add(3)
	go m.loop()
	go m.fetchLoop()
	go m.ingestLoop()
	m.Kick()
}

// Close stops the background work and closes the database.
func (m *Manager) Close() error {
	m.cancel()
	m.wg.Wait()
	return m.db.Close()
}

// Kick schedules a reconcile pass.
func (m *Manager) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *Manager) kickFetch() {
	select {
	case m.fkick <- struct{}{}:
	default:
	}
}

func (m *Manager) loop() {
	defer m.wg.Done()
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.kick:
		case <-t.C:
		}
		if err := m.Reconcile(m.ctx); err != nil && m.ctx.Err() == nil {
			m.log.Warn("collection reconcile failed", "err", err)
		}
	}
}

// Collections lists the collection datasets of the cluster view.
func Collections(v *cluster.View) []string {
	var out []string
	for name, d := range v.Datasets {
		if !d.Deleted && d.Value != nil && d.Value.Collection != nil {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Reconcile runs one pass: writer head flushes, catalog sync, selection, the holdings
// index and rescue, for every collection.
func (m *Manager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.cfg.Cluster.View()
	var errs []error
	for _, name := range Collections(v) {
		d, _ := v.Dataset(name)
		if err := m.flushHeadsLocked(ctx, name, d, false); err != nil {
			errs = append(errs, fmt.Errorf("%s: head write: %w", name, err))
		}
		v = m.cfg.Cluster.View()
		changed, err := m.syncCatalog(ctx, name, d, v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: catalog: %w", name, err))
		}
		evalErr := m.evaluate(name, d, changed)
		if evalErr != nil {
			errs = append(errs, fmt.Errorf("%s: selection: %w", name, evalErr))
		}
		m.updateReport(name, evalErr == nil)
		if m.indexOn(name) {
			m.pullIndex(ctx, name, v)
			if err := m.rescue(name); err != nil {
				errs = append(errs, fmt.Errorf("%s: rescue: %w", name, err))
			}
		}
	}
	m.dropReports(v)
	m.kickFetch()
	return errors.Join(errs...)
}

func (m *Manager) now() time.Time { return m.cfg.Now() }

// facetSpecs converts a dataset's facet declarations.
func facetSpecs(d *cluster.Dataset) map[string]FacetSpec {
	out := map[string]FacetSpec{}
	if d != nil && d.Collection != nil {
		for k, f := range d.Collection.Facets {
			out[k] = FacetSpec{Ptr: f.Ptr, Type: f.Type}
		}
	}
	return out
}

type writerState struct {
	DID  string  `json:"did"`
	Head cid.CID `json:"-"`
	Root cid.CID `json:"-"`
	H    string  `json:"head"`
	R    string  `json:"root"`
}

type collRow struct {
	root, header cid.CID
	size         uint64
	writers      []writerState
	drops        []string
	seq          int64
	evalMS       int64
	rulesGen     int64
	evalGen      int64
	facets       string
	synced       int64
}

func (m *Manager) loadColl(name string) (*collRow, error) {
	var r collRow
	var root, header []byte
	var ws, drops string
	err := m.db.QueryRow(`SELECT root, header, size, writers, drops, item_seq, eval_ms, rules_gen, eval_gen, facets, synced FROM coll WHERE ds = ?`, name).
		Scan(&root, &header, &r.size, &ws, &drops, &r.seq, &r.evalMS, &r.rulesGen, &r.evalGen, &r.facets, &r.synced)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := m.db.Exec(`INSERT INTO coll (ds) VALUES (?)`, name); err != nil {
			return nil, err
		}
		return &collRow{evalGen: -1, facets: "{}"}, nil
	}
	if err != nil {
		return nil, err
	}
	r.root, _ = cid.Decode(root)
	r.header, _ = cid.Decode(header)
	if err := json.Unmarshal([]byte(ws), &r.writers); err != nil {
		return nil, err
	}
	for i := range r.writers {
		r.writers[i].Head, _ = cid.Parse(r.writers[i].H)
		r.writers[i].Root, _ = cid.Parse(r.writers[i].R)
	}
	_ = json.Unmarshal([]byte(drops), &r.drops)
	return &r, nil
}

func cidBytes(c cid.CID) []byte {
	if !c.Defined() {
		return nil
	}
	return c.Bytes()
}

func catalogPin(c cid.CID, name string) store.Pin {
	return store.Pin{CID: c, Mode: store.Recursive, Profile: tree.CatalogProfile, Source: store.SourceCluster, Name: name}
}

// headerRoot reads a held tree header's root directory.
func (m *Manager) headerRoot(h cid.CID) (cid.CID, error) {
	b, err := m.cfg.Store.Get(h)
	if err != nil {
		return cid.CID{}, err
	}
	hd, err := tree.DecodeHeader(b)
	if err != nil {
		return cid.CID{}, err
	}
	return hd.Root, nil
}

// heldCatalog reports whether everything the catalog walk of pin reaches is held.
func (m *Manager) heldCatalog(ctx context.Context, p store.Pin) bool {
	ws, err := m.cfg.Store.Walk(ctx, p, nil)
	return err == nil && ws.Complete()
}

// writerDIDs returns the writer heads to merge: every ranked writer for union, the LWW
// winner otherwise.
func writerDIDs(v *cluster.View, name string, d *cluster.Dataset) []string {
	if IsFollow(d) {
		return followWriter(v, name)
	}
	if d.Collection.MergeMode() == cluster.MergeUnion {
		return v.Writers(name)
	}
	if h, ok := v.Head(name); ok && slices.Contains(v.Writers(name), h.Signer) {
		return []string{h.Signer}
	}
	return nil
}

// syncCatalog fetches changed writer catalogs and moves the merged catalog to them,
// returning the item paths whose merged entry changed.
func (m *Manager) syncCatalog(ctx context.Context, name string, d *cluster.Dataset, v *cluster.View) ([]string, error) {
	row, err := m.loadColl(name)
	if err != nil {
		return nil, err
	}
	specs := facetSpecs(d)
	var changed []string
	syncSpecs := func() error {
		specJSON, _ := json.Marshal(specs)
		if string(specJSON) == row.facets {
			return nil
		}
		if changed, err = m.reextract(name, specs); err != nil {
			return err
		}
		_, err := m.db.Exec(`UPDATE coll SET facets = ? WHERE ds = ?`, string(specJSON), name)
		return err
	}
	if !IsFollow(d) {
		if err := syncSpecs(); err != nil {
			return nil, err
		}
	}
	prev := map[string]writerState{}
	var prevWriters []Writer
	for _, w := range row.writers {
		prev[w.DID] = w
		prevWriters = append(prevWriters, Writer{DID: w.DID, Root: w.Root})
	}
	var next []writerState
	var errs []error
	for _, did := range writerDIDs(v, name, d) {
		h := v.Heads[name][did].Value
		target, err := cid.Parse(h.Target)
		if err != nil || h.Kind != "tree" || target.Codec() != cid.DRISL {
			errs = append(errs, fmt.Errorf("writer %s: head %s is not a tree", did, h.Target))
			continue
		}
		if p, ok := prev[did]; ok && p.Head == target {
			next = append(next, p)
			continue
		}
		pin := catalogPin(target, "catalog/"+name+"/"+did)
		if err := m.cfg.Store.Pin(pin); err != nil {
			return nil, err
		}
		if !m.heldCatalog(ctx, pin) {
			if m.cfg.FetchCatalog == nil {
				err = errors.New("catalog not held and no network")
			} else {
				fctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				err = m.cfg.FetchCatalog(fctx, target)
				cancel()
			}
			if err == nil && !m.heldCatalog(ctx, pin) {
				err = errors.New("catalog incomplete after fetch")
			}
		}
		var root cid.CID
		if err == nil {
			root, err = m.headerRoot(target)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("writer %s: %w", did, err))
			if p, ok := prev[did]; ok {
				next = append(next, p) // keep merging its last synced catalog
			}
			continue
		}
		next = append(next, writerState{DID: did, Head: target, Root: root})
	}
	if IsFollow(d) {
		// The publisher's header declares the facets (D22 merged header); a follow has no
		// declarations of its own.
		if len(next) > 0 {
			specs = m.headerFacets(next[0].Head)
		} else {
			specs = m.specs(name, d)
		}
		if err := syncSpecs(); err != nil {
			return nil, err
		}
	}
	drops := v.DropPaths(name)
	var nextWriters []Writer
	for _, w := range next {
		nextWriters = append(nextWriters, Writer{DID: w.DID, Root: w.Root})
	}
	if slices.Equal(prevWriters, nextWriters) && slices.Equal(row.drops, drops) && row.header.Defined() {
		m.repinWriters(name, row.writers, next)
		return changed, errors.Join(errs...)
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	seq := row.seq
	u := &Union{St: m.cfg.Store, Writers: prevWriters, Drop: row.drops, Root: row.root, Size: row.size}
	u.OnItem = func(p string, old *tree.Entry, nw *Leaf) error {
		if nw != nil && old != nil && *old == nw.Entry {
			_, err := tx.Exec(`UPDATE items SET writer = ? WHERE ds = ? AND path = ?`, nw.Writer, name, p)
			return err
		}
		seq++
		changed = append(changed, p)
		if _, err := tx.Exec(`DELETE FROM ftime WHERE ds = ? AND path = ?`, name, p); err != nil {
			return err
		}
		if nw == nil {
			_, err := tx.Exec(`UPDATE items SET deleted = 1, seq = ?, facets = NULL WHERE ds = ? AND path = ?`, seq, name, p)
			return err
		}
		fj := m.extractInto(tx, name, p, nw.Entry.Meta, specs)
		_, err := tx.Exec(`INSERT INTO items (ds, path, entry, kind, size, meta, writer, facets, seq, deleted) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
			ON CONFLICT (ds, path) DO UPDATE SET entry = excluded.entry, kind = excluded.kind, size = excluded.size, meta = excluded.meta,
			writer = excluded.writer, facets = excluded.facets, seq = excluded.seq, deleted = 0`,
			name, p, nw.Entry.CID.Bytes(), nw.Entry.Kind, nw.Entry.Size, cidBytes(nw.Entry.Meta), nw.Writer, fj, seq)
		return err
	}
	u.OnDecide = func(p string, win *Leaf, losers []Loser) error {
		rows, err := tx.Query(`SELECT loser, entry FROM conflicts WHERE ds = ? AND path = ?`, name, p)
		if err != nil {
			return err
		}
		old := map[string]cid.CID{}
		for rows.Next() {
			var l string
			var e []byte
			if err := rows.Scan(&l, &e); err != nil {
				rows.Close()
				return err
			}
			old[l], _ = cid.Decode(e)
		}
		rows.Close()
		if _, err := tx.Exec(`DELETE FROM conflicts WHERE ds = ? AND path = ?`, name, p); err != nil {
			return err
		}
		keep := map[string]cid.CID{}
		for _, l := range losers {
			var w string
			if win != nil {
				w = win.Writer
			}
			if _, err := tx.Exec(`INSERT INTO conflicts (ds, path, loser, winner, entry, reason) VALUES (?, ?, ?, ?, ?, ?)`,
				name, p, l.Writer, w, l.Entry.CID.Bytes(), l.Reason); err != nil {
				return err
			}
			keep[l.Writer] = l.Entry.CID
			// The losing entry stays pinned until the conflict is resolved.
			if err := m.cfg.Store.Pin(conflictPin(name, p, l.Writer, l.Entry.CID)); err != nil {
				return err
			}
		}
		for l, c := range old {
			if keep[l] != c && c.Defined() {
				_, _ = m.cfg.Store.Unpin(conflictPin(name, p, l, c))
			}
		}
		return nil
	}
	if err := u.Update(nextWriters, drops); err != nil {
		return nil, err
	}
	hdr := tree.Header{V: 1, Root: u.Root, Bytes: u.Size, Collection: map[string]any{"facets": facetsGeneric(specs)}}
	hc, hb, err := drisl.Sum(hdr)
	if err != nil {
		return nil, err
	}
	if err := m.cfg.Store.Put(hc, hb); err != nil {
		return nil, err
	}
	if err := m.cfg.Store.Pin(catalogPin(hc, "catalog/"+name)); err != nil {
		return nil, err
	}
	for i := range next {
		next[i].H, next[i].R = next[i].Head.String(), next[i].Root.String()
	}
	wj, _ := json.Marshal(next)
	dj, _ := json.Marshal(drops)
	if _, err := tx.Exec(`UPDATE coll SET root = ?, size = ?, header = ?, writers = ?, drops = ?, item_seq = ?, synced = ? WHERE ds = ?`,
		cidBytes(u.Root), u.Size, hc.Bytes(), string(wj), string(dj), seq, m.now().UnixMilli(), name); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if row.header.Defined() && row.header != hc {
		_, _ = m.cfg.Store.Unpin(catalogPin(row.header, "catalog/"+name))
	}
	m.repinWriters(name, row.writers, next)
	return changed, errors.Join(errs...)
}

// repinWriters releases the catalog pins of writer heads no longer merged.
func (m *Manager) repinWriters(name string, prev, next []writerState) {
	keep := map[string]cid.CID{}
	for _, w := range next {
		keep[w.DID] = w.Head
	}
	for _, w := range prev {
		if keep[w.DID] != w.Head && w.Head.Defined() {
			_, _ = m.cfg.Store.Unpin(catalogPin(w.Head, "catalog/"+name+"/"+w.DID))
		}
	}
}

func conflictPin(name, path, loser string, c cid.CID) store.Pin {
	return store.Pin{CID: c, Mode: store.Recursive, Profile: "nu-tree", Source: store.SourceCollectionConflict, Name: name + "/" + path + "#" + loser}
}

func facetsGeneric(specs map[string]FacetSpec) map[string]any {
	out := map[string]any{}
	for k, s := range specs {
		out[k] = map[string]any{"ptr": s.Ptr, "type": s.Type}
	}
	return out
}

// execer is a *sql.DB or *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// extractInto reads an item's metadata record, extracts its facets and indexes its time
// facets; it returns the facets' JSON (nil without metadata).
func (m *Manager) extractInto(x execer, name, path string, meta cid.CID, specs map[string]FacetSpec) any {
	if !meta.Defined() || len(specs) == 0 {
		return nil
	}
	b, err := m.cfg.Store.Get(meta)
	if err != nil {
		return nil
	}
	val, err := drisl.DecOptions{MaxSize: tree.MaxMetaSize}.Decode(b)
	if err != nil {
		return nil
	}
	f := Extract(specs, val)
	for k, v := range f {
		if v.Type == TypeTime {
			_, _ = x.Exec(`INSERT OR REPLACE INTO ftime (ds, facet, path, ms) VALUES (?, ?, ?, ?)`, name, k, path, v.Time)
		}
	}
	j, _ := json.Marshal(f)
	return string(j)
}

// reextract recomputes every item's facets after the declarations changed.
func (m *Manager) reextract(name string, specs map[string]FacetSpec) ([]string, error) {
	rows, err := m.db.Query(`SELECT path, meta FROM items WHERE ds = ? AND deleted = 0`, name)
	if err != nil {
		return nil, err
	}
	type it struct {
		path string
		meta cid.CID
	}
	var all []it
	for rows.Next() {
		var p string
		var mb []byte
		if err := rows.Scan(&p, &mb); err != nil {
			rows.Close()
			return nil, err
		}
		mc, _ := cid.Decode(mb)
		all = append(all, it{p, mc})
	}
	rows.Close()
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM ftime WHERE ds = ?`, name); err != nil {
		return nil, err
	}
	var paths []string
	for _, x := range all {
		if _, err := tx.Exec(`UPDATE items SET facets = ? WHERE ds = ? AND path = ?`, m.extractInto(tx, name, x.path, x.meta, specs), name, x.path); err != nil {
			return nil, err
		}
		paths = append(paths, x.path)
	}
	return paths, tx.Commit()
}

// itemRow is one items-table row.
type itemRow struct {
	path    string
	entry   cid.CID
	kind    string
	size    uint64
	meta    cid.CID
	writer  string
	facets  map[string]Value
	rule    int
	seq     int64
	deleted bool
}

const itemCols = `path, entry, kind, size, meta, writer, facets, rule, seq, deleted`

func scanItem(sc interface{ Scan(...any) error }) (*itemRow, error) {
	var it itemRow
	var eb, mb []byte
	var kind, writer, fj sql.NullString
	if err := sc.Scan(&it.path, &eb, &kind, &it.size, &mb, &writer, &fj, &it.rule, &it.seq, &it.deleted); err != nil {
		return nil, err
	}
	it.entry, _ = cid.Decode(eb)
	it.meta, _ = cid.Decode(mb)
	it.kind, it.writer = kind.String, writer.String
	if fj.Valid {
		_ = json.Unmarshal([]byte(fj.String), &it.facets)
	}
	return &it, nil
}

func (m *Manager) item(name, path string) (*itemRow, error) {
	it, err := scanItem(m.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE ds = ? AND path = ?`, name, path))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return it, err
}

// holding is one holdings-table row.
type holding struct {
	entry cid.CID
	state string
	src   string
	rule  int
	bytes uint64
	tries int
}

func (m *Manager) holding(name, path string) (*holding, error) {
	var h holding
	var eb []byte
	err := m.db.QueryRow(`SELECT entry, state, src, rule, bytes, tries FROM holdings WHERE ds = ? AND path = ?`, name, path).
		Scan(&eb, &h.state, &h.src, &h.rule, &h.bytes, &h.tries)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h.entry, _ = cid.Decode(eb)
	return &h, nil
}

// heldKeys are this node's authority keys (to recognise items it wrote).
func (m *Manager) heldKeys() map[string]bool {
	if m.ownKeys != nil {
		return m.ownKeys()
	}
	out := map[string]bool{}
	for _, k := range m.cfg.Cluster.HeldKeys() {
		out[k] = true
	}
	return out
}

// evaluate applies selection to the changed items, to items at moving window edges,
// to paused items, and to everything after a rule change.
func (m *Manager) evaluate(name string, d *cluster.Dataset, changed []string) error {
	row, err := m.loadColl(name)
	if err != nil {
		return err
	}
	rules, err := m.Rules(name)
	if err != nil {
		return err
	}
	if err := Compile(rules, m.specs(name, d)); err != nil {
		m.log.Warn("collection rules do not fit the facets; ignoring rules", "collection", name, "err", err)
		rules = nil
	}
	limit := followLimit(d)
	now := m.now()
	set := map[string]bool{}
	for _, p := range changed {
		set[p] = true
	}
	// A placement epoch or policy change re-decides every item's floor owners.
	fl := m.floor(d)
	floorKey, lastFloor := "", ""
	if fl != nil {
		floorKey = fl.Epoch + "/" + d.PolicyHash()
	}
	_ = m.db.QueryRow(`SELECT floor FROM coll WHERE ds = ?`, name).Scan(&lastFloor)
	add := func(q string, args ...any) error {
		rows, err := m.db.Query(q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return err
			}
			set[p] = true
		}
		return rows.Err()
	}
	full := row.evalGen != row.rulesGen || floorKey != lastFloor
	if full {
		if err := add(`SELECT path FROM items WHERE ds = ? AND deleted = 0`, name); err != nil {
			return err
		}
	} else {
		for facet, ws := range Windows(rules) {
			for _, w := range ws {
				lo, hi := row.evalMS-w.Milliseconds(), now.UnixMilli()-w.Milliseconds()
				if err := add(`SELECT path FROM ftime WHERE ds = ? AND facet = ? AND ms >= ? AND ms < ?`, name, facet, lo, hi); err != nil {
					return err
				}
			}
		}
		if err := add(`SELECT path FROM holdings WHERE ds = ? AND state = ? LIMIT 1000`, name, StatePaused); err != nil {
			return err
		}
	}
	if err := add(`SELECT path FROM holdings WHERE ds = ? AND path NOT IN (SELECT path FROM items WHERE ds = ? AND deleted = 0)`, name, name); err != nil {
		return err
	}
	paths := slices.Sorted(maps.Keys(set))
	keys := m.heldKeys()
	if IsFollow(d) {
		keys = nil // the follower's admin wrote the head from the proof; it is no writer
	}
	released := false
	for _, p := range paths {
		r, err := m.evalItem(name, p, rules, now, keys, fl, limit)
		if err != nil {
			_, _ = m.db.Exec(`UPDATE coll SET floor = '' WHERE ds = ?`, name) // re-decide everything next pass
			return err
		}
		released = released || r
	}
	if released {
		m.scheduleGC()
	}
	_, err = m.db.Exec(`UPDATE coll SET eval_ms = ?, eval_gen = ?, floor = ? WHERE ds = ?`, now.UnixMilli(), row.rulesGen, floorKey, name)
	return err
}

type want struct {
	src  string
	rule int
}

// evalItem decides one item's holding and applies it; it reports a release. fl is the
// collection's floor placement (nil: replicas 0).
func (m *Manager) evalItem(name, path string, rules []Rule, now time.Time, keys map[string]bool, fl *cluster.Placement, limit int64) (bool, error) {
	it, err := m.item(name, path)
	if err != nil {
		return false, err
	}
	cur, err := m.holding(name, path)
	if err != nil {
		return false, err
	}
	// A complete floor holding stays as surplus when its item leaves this member's
	// ownership or the catalog (S: never released without evidence; release.auto off).
	keep := cur != nil && (cur.src == SrcFloor || cur.src == SrcSurplus) && cur.state == StateComplete
	if it == nil || it.deleted {
		if keep {
			return false, m.toSurplus(name, path, cur)
		}
		return m.release(name, path, cur)
	}
	ruleIdx := Select(rules, path, it.facets, now)
	if ruleIdx != it.rule {
		if _, err := m.db.Exec(`UPDATE items SET rule = ? WHERE ds = ? AND path = ?`, ruleIdx, name, path); err != nil {
			return false, err
		}
	}
	var manual bool
	_ = m.db.QueryRow(`SELECT 1 FROM manual WHERE ds = ? AND path = ?`, name, path).Scan(&manual)
	var w *want
	switch {
	case fl != nil && fl.Owns(m.cfg.Self, ItemUnit(it.entry)):
		w = &want{SrcFloor, -1}
	case keep && cur.entry == it.entry:
		w = &want{SrcSurplus, -1}
	case manual:
		w = &want{SrcManual, -1}
	case keys[it.writer]:
		w = &want{SrcWriter, -1}
	case ruleIdx >= 0 && rules[ruleIdx].Rescue != nil:
		if cur != nil && cur.src == SrcRescue && cur.rule == ruleIdx && cur.entry == it.entry {
			w = &want{SrcRescue, ruleIdx} // keep; the rescue pass decides releases
		}
	case ruleIdx >= 0 && rules[ruleIdx].Holds():
		w = &want{SrcSelect, ruleIdx}
	}
	if w == nil {
		return m.release(name, path, cur)
	}
	return false, m.hold(name, it, cur, *w, rules, limit)
}

func pinSource(src string) string {
	switch src {
	case SrcManual:
		return store.SourceCollectionManual
	case SrcFloor:
		return store.SourceCluster
	case SrcSurplus:
		return store.SourceClusterSurplus
	}
	return store.SourceCollectionSelect
}

func itemPin(name, path, src string, c cid.CID) store.Pin {
	return store.Pin{CID: c, Mode: store.Recursive, Profile: "nu-tree", Source: pinSource(src), Name: name + "/" + path}
}

func held(h *holding) bool {
	return h != nil && h.state != StatePaused
}

// hold makes the item held under w, within the rule's budget and, for a followed
// collection, the follow's max_size over every holding (limit; 0: none).
func (m *Manager) hold(name string, it *itemRow, cur *holding, w want, rules []Rule, limit int64) error {
	if held(cur) && cur.entry == it.entry && cur.src == w.src && cur.rule == w.rule {
		return nil
	}
	pause := func() error {
		if held(cur) {
			if _, err := m.release(name, it.path, cur); err != nil {
				return err
			}
		}
		_, err := m.db.Exec(`INSERT OR REPLACE INTO holdings (ds, path, entry, state, src, rule, bytes) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			name, it.path, it.entry.Bytes(), StatePaused, w.src, w.rule, it.size)
		return err
	}
	if w.rule >= 0 && (!held(cur) || cur.rule != w.rule) {
		if budget := rules[w.rule].Budget(); budget > 0 {
			var used int64
			if err := m.db.QueryRow(`SELECT COALESCE(SUM(bytes), 0) FROM holdings WHERE ds = ? AND rule = ? AND state != ? AND path != ?`,
				name, w.rule, StatePaused, it.path).Scan(&used); err != nil {
				return err
			}
			if used+int64(it.size) > budget {
				return pause()
			}
		}
	}
	if limit > 0 && (!held(cur) || cur.entry != it.entry) {
		var used int64
		if err := m.db.QueryRow(`SELECT COALESCE(SUM(bytes), 0) FROM holdings WHERE ds = ? AND state != ? AND path != ?`,
			name, StatePaused, it.path).Scan(&used); err != nil {
			return err
		}
		if used+int64(it.size) > limit {
			return pause()
		}
	}
	if err := m.cfg.Store.Pin(itemPin(name, it.path, w.src, it.entry)); err != nil {
		return err
	}
	state := StateFetching
	if held(cur) {
		if cur.entry == it.entry {
			state = cur.state
		}
		if cur.entry != it.entry || pinSource(cur.src) != pinSource(w.src) {
			_, _ = m.cfg.Store.Unpin(itemPin(name, it.path, cur.src, cur.entry))
		}
	}
	if _, err := m.db.Exec(`INSERT OR REPLACE INTO holdings (ds, path, entry, state, src, rule, bytes, tries) VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		name, it.path, it.entry.Bytes(), state, w.src, w.rule, it.size); err != nil {
		return err
	}
	if !held(cur) || cur.entry != it.entry || cur.src != w.src || cur.state != state {
		if err := m.appendLog(name, it.path, it.entry, state, w.src, it.size); err != nil {
			return err
		}
	}
	if state == StateFetching {
		m.kickFetch()
	}
	return nil
}

// release drops a holding (logging `released` for real holdings).
func (m *Manager) release(name, path string, cur *holding) (bool, error) {
	if cur == nil {
		return false, nil
	}
	if _, err := m.db.Exec(`DELETE FROM holdings WHERE ds = ? AND path = ?`, name, path); err != nil {
		return false, err
	}
	if !held(cur) {
		return false, nil
	}
	_, _ = m.cfg.Store.Unpin(itemPin(name, path, cur.src, cur.entry))
	return true, m.appendLog(name, path, cur.entry, StateReleased, cur.src, cur.bytes)
}

// appendLog adds a holdings-log transition and trims the log to its bound.
func (m *Manager) appendLog(name, path string, entry cid.CID, state, src string, bytes uint64) error {
	var seq int64
	if err := m.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM hlog WHERE ds = ?`, name).Scan(&seq); err != nil {
		return err
	}
	seq++
	if _, err := m.db.Exec(`INSERT INTO hlog (ds, seq, path, entry, state, src, bytes) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, seq, path, cidBytes(entry), state, src, bytes); err != nil {
		return err
	}
	_, err := m.db.Exec(`DELETE FROM hlog WHERE ds = ? AND seq <= ?`, name, seq-int64(m.cfg.MaxLog))
	return err
}

// LogMark returns this node's holdings-log position for a collection.
func (m *Manager) LogMark(name string) (int64, int64) {
	var lo, hi int64
	_ = m.db.QueryRow(`SELECT COALESCE(MIN(seq), 0), COALESCE(MAX(seq), 0) FROM hlog WHERE ds = ?`, name).Scan(&lo, &hi)
	return lo, hi
}

func (m *Manager) scheduleGC() {
	if m.cfg.GC == nil {
		return
	}
	m.fmu.Lock()
	defer m.fmu.Unlock()
	if m.gcPending {
		return
	}
	m.gcPending = true
	time.AfterFunc(m.cfg.ReleaseDelay, func() {
		m.fmu.Lock()
		m.gcPending = false
		m.fmu.Unlock()
		if m.ctx.Err() == nil {
			m.cfg.GC(m.ctx)
		}
	})
}

// fetchLoop fetches items in state fetching, a few at a time.
func (m *Manager) fetchLoop() {
	defer m.wg.Done()
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	sem := make(chan struct{}, m.cfg.Workers)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.fkick:
		case <-t.C:
		}
		rows, err := m.db.Query(`SELECT ds, path, entry, tries FROM holdings WHERE state = ? ORDER BY tries, path LIMIT 256`, StateFetching)
		if err != nil {
			continue
		}
		type job struct {
			ds, path string
			entry    cid.CID
		}
		var jobs []job
		for rows.Next() {
			var j job
			var eb []byte
			var tries int
			if rows.Scan(&j.ds, &j.path, &eb, &tries) == nil {
				j.entry, _ = cid.Decode(eb)
				jobs = append(jobs, j)
			}
		}
		rows.Close()
		for _, j := range jobs {
			key := j.ds + "\x00" + j.path
			m.fmu.Lock()
			busy := m.inflight[key]
			if !busy {
				m.inflight[key] = true
			}
			m.fmu.Unlock()
			if busy {
				continue
			}
			select {
			case sem <- struct{}{}:
			case <-m.ctx.Done():
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				defer func() {
					m.fmu.Lock()
					delete(m.inflight, key)
					m.fmu.Unlock()
				}()
				m.fetchItem(j.ds, j.path, j.entry)
			}()
		}
	}
}

func (m *Manager) fetchItem(name, path string, entry cid.CID) {
	pin := store.Pin{CID: entry, Mode: store.Recursive, Profile: "nu-tree"}
	complete := func() bool {
		ws, err := m.cfg.Store.Walk(m.ctx, pin, nil)
		return err == nil && ws.Complete()
	}
	ok := complete()
	if !ok && m.cfg.FetchItem != nil {
		ctx, cancel := context.WithTimeout(m.ctx, time.Hour)
		err := m.cfg.FetchItem(ctx, entry)
		cancel()
		if err != nil && m.ctx.Err() == nil {
			m.log.Debug("item fetch failed", "collection", name, "path", path, "err", err)
		}
		ok = err == nil && complete()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := m.holding(name, path)
	if err != nil || cur == nil || cur.entry != entry || cur.state != StateFetching {
		return
	}
	if !ok {
		_, _ = m.db.Exec(`UPDATE holdings SET tries = tries + 1 WHERE ds = ? AND path = ?`, name, path)
		return
	}
	if _, err := m.db.Exec(`UPDATE holdings SET state = ?, tries = 0 WHERE ds = ? AND path = ?`, StateComplete, name, path); err != nil {
		return
	}
	_ = m.appendLog(name, path, entry, StateComplete, cur.src, cur.bytes)
	if cur.src == SrcFloor {
		m.updateReport(name, true)
	}
}

// splitPath validates an item path.
func splitPath(p string) ([]string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil, errors.New("empty item path")
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if err := tree.ValidName(s); err != nil {
			return nil, err
		}
	}
	return segs, nil
}
