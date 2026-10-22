package collection

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/store"
)

// Item pushes (docs/collections.md "Ingest"): a producer pushes an item (path, metadata
// record, files); the push is durably queued in the pushes table, its files are stored
// (uploads, local paths, URL imports), the item entry is built and queued into this
// node's writer head, and the push is written once a head write contains it. The pushes
// table is also the push log: each row's seq moves forward on every state change.

// Ingest defaults.
const (
	DefaultIngestWorkers = 4
	DefaultIngestBackoff = 5 * time.Second
	MaxIngestBackoff     = 10 * time.Minute
	// MaxPushFiles bounds the files of one item.
	MaxPushFiles = 4096
	// IngestIdleTimeout is the network idle timeout of URL fetches.
	IngestIdleTimeout = time.Minute
)

// DefaultIngestLimits apply where a dataset's ingest.limits leave a field zero.
var DefaultIngestLimits = cluster.IngestLimits{MaxBytes: 1 << 40, Attempts: 6, Timeout: "2h"}

// pushFile is a validated request file with its digest form.
type pushFile struct {
	Key      string `json:"key"`
	Upload   string `json:"upload,omitempty"`
	Path     string `json:"path,omitempty"`
	NoCopy   bool   `json:"nocopy,omitempty"`
	URL      string `json:"url,omitempty"`
	Checksum string `json:"checksum,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// pushReq is the stored form of an accepted push.
type pushReq struct {
	Meta  string     `json:"meta"`
	Files []pushFile `json:"files"`
}

// stored is a file of a push that is in the store.
type stored struct {
	CID  string `json:"cid"`
	Size uint64 `json:"size"`
}

type pushRow struct {
	ds, path, id string
	digest       []byte
	req          pushReq
	state        string
	replace      bool
	files        map[string]stored
	sources      map[string]apiv0.URLSource
	entry, meta  cid.CID
	head         cid.CID
	size         uint64
	err          *apiv0.Error
	tries        int
	nextMS       int64
	created      int64
	updated      int64
	seq          int64
}

const pushCols = `ds, path, id, digest, req, state, replace, files, sources, entry, size, meta, head, error, tries, next_ms, created_ms, updated_ms, seq`

type rowScanner interface{ Scan(...any) error }

func scanPush(sc rowScanner) (*pushRow, error) {
	var r pushRow
	var req, files, sources string
	var entry, meta, head []byte
	var errJSON sql.NullString
	if err := sc.Scan(&r.ds, &r.path, &r.id, &r.digest, &req, &r.state, &r.replace, &files, &sources, &entry, &r.size, &meta, &head,
		&errJSON, &r.tries, &r.nextMS, &r.created, &r.updated, &r.seq); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(req), &r.req)
	_ = json.Unmarshal([]byte(files), &r.files)
	_ = json.Unmarshal([]byte(sources), &r.sources)
	if r.files == nil {
		r.files = map[string]stored{}
	}
	if r.sources == nil {
		r.sources = map[string]apiv0.URLSource{}
	}
	r.entry, _ = cid.Decode(entry)
	r.meta, _ = cid.Decode(meta)
	r.head, _ = cid.Decode(head)
	if errJSON.Valid && errJSON.String != "" {
		r.err = &apiv0.Error{}
		_ = json.Unmarshal([]byte(errJSON.String), r.err)
	}
	return &r, nil
}

func (m *Manager) loadPush(q interface {
	QueryRow(string, ...any) *sql.Row
}, name, path string) (*pushRow, error) {
	r, err := scanPush(q.QueryRow(`SELECT `+pushCols+` FROM pushes WHERE ds = ? AND path = ?`, name, path))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func errString(e *apiv0.Error) any {
	if e == nil {
		return nil
	}
	return jsonString(e)
}

// savePush writes r with the next push-log position.
func (m *Manager) savePush(tx *sql.Tx, r *pushRow) error {
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM pushes WHERE ds = ?`, r.ds).Scan(&r.seq); err != nil {
		return err
	}
	r.updated = m.now().UnixMilli()
	if r.digest == nil {
		r.digest = []byte{}
	}
	_, err := tx.Exec(`INSERT OR REPLACE INTO pushes (`+pushCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ds, r.path, r.id, r.digest, jsonString(r.req), r.state, r.replace, jsonString(r.files), jsonString(r.sources),
		cidBytes(r.entry), r.size, cidBytes(r.meta), cidBytes(r.head), errString(r.err), r.tries, r.nextMS, r.created, r.updated, r.seq)
	return err
}

func (m *Manager) info(r *pushRow) *apiv0.CollectionPush {
	out := &apiv0.CollectionPush{Path: r.path, ID: r.id, State: r.state, Replace: r.replace, Size: r.size, Tries: r.tries,
		Error: r.err, Seq: r.seq, Created: msTime(r.created), Updated: msTime(r.updated)}
	if r.entry.Defined() {
		out.Entry = r.entry.String()
	}
	if r.meta.Defined() {
		out.Meta = r.meta.String()
	}
	if r.head.Defined() {
		out.Head = r.head.String()
	}
	if len(r.sources) > 0 {
		out.Sources = r.sources
	}
	return out
}

// pushDone reports whether a push reached its final state (a removal once a head write
// no longer has the item).
func pushDone(r *pushRow) bool {
	return r.state == apiv0.PushWritten || r.state == apiv0.PushFailed || (r.state == apiv0.PushRemoved && r.head.Defined())
}

// notifyPushes wakes waiters after push state changes.
func (m *Manager) notifyPushes() {
	m.fmu.Lock()
	close(m.pushCh)
	m.pushCh = make(chan struct{})
	m.fmu.Unlock()
}

func (m *Manager) pushWake() <-chan struct{} {
	m.fmu.Lock()
	defer m.fmu.Unlock()
	return m.pushCh
}

func (m *Manager) kickIngest() {
	select {
	case m.ikick <- struct{}{}:
	default:
	}
}

// receiving returns the collection's dataset when this node can receive pushes for it
// (it holds a writer key).
func (m *Manager) receiving(name string) (*cluster.Dataset, error) {
	v := m.cfg.Cluster.View()
	d, ok := v.Dataset(name)
	if !ok || d.Collection == nil {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no collection %q in the cluster", name).
			WithAction("list datasets with `nu cluster dataset ls`")
	}
	if m.WriterKey(v, d) == "" {
		return nil, apiv0.Errorf(apiv0.CodeForbidden, "this node holds no writer key for collection %q, so it cannot receive pushes", name).
			WithAction("push to a receiving member (one whose key is in the dataset's writers), or grant this node a writer key")
	}
	return d, nil
}

// EgressHosts returns the URL host allowlist for a collection's item URLs on this node:
// nil permits any public host (public mode without ingest.hosts); ok is false when URL
// files are not allowed at all (isolated mode).
func EgressHosts(mode string, d *cluster.Dataset) (hosts []string, ok bool) {
	var listed []string
	if d.Source.Ingest != nil {
		listed = d.Source.Ingest.Hosts
	}
	switch mode {
	case cluster.ModeIsolated:
		return nil, false
	case cluster.ModePrivate:
		return append([]string{}, listed...), true // an empty list permits none
	}
	if len(listed) > 0 {
		return listed, true
	}
	return nil, true
}

func ingestLimits(d *cluster.Dataset) (cluster.IngestLimits, time.Duration) {
	l := DefaultIngestLimits
	if d.Source.Ingest != nil && d.Source.Ingest.Limits != nil {
		x := d.Source.Ingest.Limits
		if x.MaxBytes > 0 {
			l.MaxBytes = x.MaxBytes
		}
		if x.Attempts > 0 {
			l.Attempts = x.Attempts
		}
		if x.Timeout != "" {
			l.Timeout = x.Timeout
		}
	}
	t, err := ParseDuration(l.Timeout)
	if err != nil || t <= 0 {
		t = 2 * time.Hour
	}
	return l, t
}

func invalid(format string, args ...any) *apiv0.Error {
	return apiv0.Errorf(apiv0.CodeInvalidArgument, format, args...)
}

// validatePush checks a request and stores its metadata record, returning the stored
// request form.
func (m *Manager) validatePush(d *cluster.Dataset, req apiv0.CollectionPushRequest) (string, pushReq, error) {
	segs, err := splitPath(req.Path)
	if err != nil {
		return "", pushReq{}, invalid("item path %q: %v", req.Path, err)
	}
	path := joinSegs(segs)
	var out pushReq
	meta, err := m.storeMeta(req.Meta)
	if err != nil {
		return "", pushReq{}, err
	}
	out.Meta = meta.String()
	if len(req.Files) == 0 || len(req.Files) > MaxPushFiles {
		return "", pushReq{}, invalid("an item push needs 1 to %d files", MaxPushFiles)
	}
	keys := slices.Sorted(maps.Keys(req.Files))
	for i, k := range keys {
		ks, err := splitPath(k)
		if err != nil {
			return "", pushReq{}, invalid("file %q: %v", k, err)
		}
		key := joinSegs(ks)
		if key != k {
			return "", pushReq{}, invalid("file %q: write it as %q", k, key)
		}
		if i+1 < len(keys) && strings.HasPrefix(keys[i+1], key+"/") {
			return "", pushReq{}, invalid("file %q is also a directory of %q", key, keys[i+1])
		}
		f := req.Files[k]
		pf := pushFile{Key: key, Size: f.Size}
		n := 0
		for _, set := range []bool{f.Upload != "", f.Path != "", f.URL != ""} {
			if set {
				n++
			}
		}
		if n != 1 {
			return "", pushReq{}, invalid("file %q needs exactly one of upload, path and url", key)
		}
		if f.Size < 0 || (f.Checksum != "" && f.URL == "") || (f.NoCopy && f.Path == "") {
			return "", pushReq{}, invalid("file %q: checksum applies to url files, nocopy to path files, and size must not be negative", key)
		}
		switch {
		case f.Upload != "":
			c, err := cid.Parse(f.Upload)
			if err != nil || c.Codec() != cid.Raw {
				return "", pushReq{}, invalid("file %q: upload %q is not a blob CID", key, f.Upload)
			}
			if _, ok, err := m.cfg.Store.Has(c); err != nil || !ok {
				return "", pushReq{}, apiv0.Errorf(apiv0.CodeNotFound, "file %q: upload %s is not held", key, c).
					WithAction("upload the file first (POST …/uploads), then push within an hour")
			}
			pf.Upload = c.String()
		case f.Path != "":
			if !filepath.IsAbs(f.Path) {
				return "", pushReq{}, invalid("file %q: path %q must be absolute on the receiving node", key, f.Path)
			}
			fi, err := os.Stat(f.Path)
			if err != nil {
				return "", pushReq{}, apiv0.Errorf(apiv0.CodeNotFound, "file %q: %v", key, err)
			}
			if !fi.Mode().IsRegular() {
				return "", pushReq{}, invalid("file %q: %s is not a regular file (push directories file by file; the CLI expands them)", key, f.Path)
			}
			pf.Path, pf.NoCopy = f.Path, f.NoCopy
		default:
			if err := m.checkURL(d, f.URL); err != nil {
				return "", pushReq{}, err
			}
			if f.Checksum != "" {
				ck, err := httpsrc.ParseChecksum(f.Checksum)
				if err != nil {
					return "", pushReq{}, invalid("file %q: %v", key, err)
				}
				pf.Checksum = ck.String()
			}
			pf.URL = f.URL
		}
		out.Files = append(out.Files, pf)
	}
	return path, out, nil
}

// checkURL applies the egress policy to an item URL before anything is fetched.
func (m *Manager) checkURL(d *cluster.Dataset, raw string) error {
	hosts, ok := EgressHosts(m.cfg.Cluster.Mode(), d)
	if !ok || m.cfg.HTTP == nil {
		return apiv0.Errorf(apiv0.CodeForbidden, "this node does not fetch item URLs (isolated cluster or no HTTPS client)").
			WithAction("push uploads or local paths instead")
	}
	if err := m.cfg.HTTP.Check(raw, hosts); err != nil {
		return apiv0.Errorf(apiv0.CodeForbidden, "URL %q is not allowed: %v", raw, err).
			WithAction("item URLs must be HTTPS without credentials; in private mode their host must be in the dataset's ingest.hosts")
	}
	return nil
}

// storeMeta stores a metadata record given as atproto-style JSON (generic DRISL, floats
// allowed) or as the CID string of a held record.
func (m *Manager) storeMeta(raw json.RawMessage) (cid.CID, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return cid.CID{}, invalid("an item push needs a metadata record (meta)")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return cid.CID{}, invalid("meta: %v", err)
		}
		c, err := cid.Parse(s)
		if err != nil || c.Codec() != cid.DRISL || c.Hash() != cid.SHA256 {
			return cid.CID{}, invalid("meta %q is not a SHA-256 DRISL CID", s)
		}
		if _, ok, err := m.cfg.Store.Has(c); err != nil || !ok {
			return cid.CID{}, apiv0.Errorf(apiv0.CodeNotFound, "metadata record %s is not held", c).WithAction("store it with `nu dag put`, or push the record itself")
		}
		return c, nil
	}
	v, err := drisl.FromJSON(raw, drisl.JSONOptions{MaxSize: tree.MaxMetaSize})
	if err != nil {
		return cid.CID{}, invalid("meta: %v", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return cid.CID{}, invalid("meta must be a JSON object or a record CID")
	}
	c, b, err := drisl.Sum(v)
	if err != nil {
		return cid.CID{}, invalid("meta: %v", err)
	}
	if len(b) > tree.MaxMetaSize {
		return cid.CID{}, invalid("meta exceeds %d bytes", tree.MaxMetaSize)
	}
	return c, m.cfg.Store.Put(c, b)
}

// digest names what a push asks for: its metadata record and, per file, the upload, the
// local path, or the checksum (else the URL) with the declared size. An identical resend
// has the same digest, even when a URL's query (a presigned token) changed but its
// checksum did not.
func digest(r pushReq) []byte {
	type df struct {
		K, U, P, X string
		N          bool
		S          int64
	}
	var fs []df
	for _, f := range r.Files {
		x := f.Checksum
		if x == "" {
			x = f.URL
		}
		fs = append(fs, df{f.Key, f.Upload, f.Path, x, f.NoCopy, f.Size})
	}
	h := sha256.Sum256([]byte(jsonString(struct {
		M string
		F []df
	}{r.Meta, fs})))
	return h[:]
}

func newPushID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func pushPin(name, path, id string, c cid.CID) store.Pin {
	return store.Pin{CID: c, Mode: store.Direct, Source: store.SourceCollectionPush, Name: name + "/" + path + "#" + id}
}

// pinned lists what a push pins: its metadata record, uploads and stored files.
func (r *pushRow) pinned() []cid.CID {
	var out []cid.CID
	if c, err := cid.Parse(r.req.Meta); err == nil {
		out = append(out, c)
	}
	for _, f := range r.req.Files {
		if c, err := cid.Parse(f.Upload); err == nil {
			out = append(out, c)
		}
	}
	for _, s := range r.files {
		if c, err := cid.Parse(s.CID); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// unpinPush releases a finished (or superseded) push's pins and partial downloads.
func (m *Manager) unpinPush(r *pushRow) {
	for _, c := range r.pinned() {
		_, _ = m.cfg.Store.Unpin(pushPin(r.ds, r.path, r.id, c))
	}
	for _, f := range r.req.Files {
		if f.URL != "" {
			p := m.partialPath(r, f)
			_ = os.Remove(p)
			_ = os.Remove(p + ".json")
		}
	}
}

// Push accepts an item push: it validates the request, stores the metadata record and
// durably queues the push. Pushing what was already pushed (the same digest) is a no-op
// answered with the existing push (a failed one is retried); a different push while one
// is in progress at the path is a conflict unless req.Replace.
func (m *Manager) Push(name string, req apiv0.CollectionPushRequest) (*apiv0.CollectionPush, error) {
	d, err := m.receiving(name)
	if err != nil {
		return nil, err
	}
	path, pr, err := m.validatePush(d, req)
	if err != nil {
		return nil, err
	}
	dg := digest(pr)
	m.pmu.Lock()
	defer m.pmu.Unlock()
	old, err := m.loadPush(m.db, name, path)
	if err != nil {
		return nil, err
	}
	if old != nil && old.state != apiv0.PushRemoved && string(old.digest) == string(dg) {
		if old.state != apiv0.PushFailed {
			out := m.info(old)
			out.Duplicate = true
			return out, nil
		}
	} else if old != nil && !req.Replace && (old.state == apiv0.PushAccepted || old.state == apiv0.PushFetching) {
		return nil, apiv0.Errorf(apiv0.CodeConflict, "a different push for %q is in progress (push %s)", path, old.id).
			WithAction("wait for it with ?wait=written, or push again with replace").WithDetail("push", m.info(old))
	}
	now := m.now().UnixMilli()
	r := &pushRow{ds: name, path: path, id: newPushID(), digest: dg, req: pr, state: apiv0.PushAccepted, replace: req.Replace,
		files: map[string]stored{}, sources: map[string]apiv0.URLSource{}, created: now}
	r.meta, _ = cid.Parse(pr.Meta)
	for _, c := range r.pinned() {
		if err := m.cfg.Store.Pin(pushPin(name, path, r.id, c)); err != nil {
			return nil, err
		}
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := m.savePush(tx, r); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if old != nil {
		m.unpinPush(old) // pins are per push; a written or failed one has none left
	}
	m.notifyPushes()
	m.kickIngest()
	return m.info(r), nil
}

// Remove queues the item's removal from this node's writer head (idempotent); a push in
// progress at the path is cancelled.
func (m *Manager) Remove(name, path string) (*apiv0.CollectionPush, error) {
	if _, err := m.receiving(name); err != nil {
		return nil, err
	}
	segs, err := splitPath(path)
	if err != nil {
		return nil, invalid("item path %q: %v", path, err)
	}
	path = joinSegs(segs)
	m.pmu.Lock()
	defer m.pmu.Unlock()
	old, err := m.loadPush(m.db, name, path)
	if err != nil {
		return nil, err
	}
	if old != nil && old.state == apiv0.PushRemoved {
		return m.info(old), nil
	}
	now := m.now().UnixMilli()
	r := &pushRow{ds: name, path: path, id: newPushID(), digest: []byte{}, state: apiv0.PushRemoved,
		files: map[string]stored{}, sources: map[string]apiv0.URLSource{}, created: now}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO pending (ds, path, entry) VALUES (?, ?, NULL)`, name, path); err != nil {
		return nil, err
	}
	if err := m.savePush(tx, r); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if old != nil {
		m.unpinPush(old)
	}
	m.scheduleFlush(name)
	m.notifyPushes()
	return m.info(r), nil
}

// PushStatus returns this node's push at a path (nil when there is none).
func (m *Manager) PushStatus(name, path string) (*apiv0.CollectionPush, error) {
	r, err := m.loadPush(m.db, name, strings.Trim(path, "/"))
	if err != nil || r == nil {
		return nil, err
	}
	return m.info(r), nil
}

// WaitPush waits until the push with id at path is final (written, failed, or a removal
// written), or timeout passes; it then returns the push's state (a timeout error carries
// the current state in details.push).
func (m *Manager) WaitPush(ctx context.Context, name, path, id string, timeout time.Duration) (*apiv0.CollectionPush, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		wake := m.pushWake()
		r, err := m.loadPush(m.db, name, path)
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, apiv0.Errorf(apiv0.CodeNotFound, "no push for %q", path)
		}
		if r.id != id || pushDone(r) {
			return m.info(r), nil
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.ctx.Done():
			return nil, apiv0.Errorf(apiv0.CodeUnavailable, "shutting down")
		case <-t.C:
			return nil, apiv0.Errorf(apiv0.CodeTimeout, "push for %q is still %s", path, r.state).
				WithAction("wait again, or follow the push log").WithDetail("push", m.info(r))
		}
	}
}

// Pushes returns the push log after a cursor ("0" or empty: from the start): each path
// once, with its latest state, in the order of their last change; next_cursor is always
// set, and a cursor from another database epoch answers resync.
func (m *Manager) Pushes(name, since string, limit int) (*apiv0.Page[apiv0.CollectionPush], error) {
	if _, err := m.dataset(name); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = apiv0.DefaultPageLimit
	}
	limit = min(limit, apiv0.MaxPageLimit)
	var seq int64
	if since != "" && since != "0" {
		c, err := apiv0.ParseCursor(since)
		if err != nil {
			return nil, err
		}
		if c.Epoch != m.epoch {
			return &apiv0.Page[apiv0.CollectionPush]{Items: []apiv0.CollectionPush{}, Resync: true}, nil
		}
		seq = int64(c.Offset)
	}
	rows, err := m.db.Query(`SELECT `+pushCols+` FROM pushes WHERE ds = ? AND seq > ? ORDER BY seq LIMIT ?`, name, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &apiv0.Page[apiv0.CollectionPush]{Items: []apiv0.CollectionPush{}}
	for rows.Next() {
		r, err := scanPush(rows)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *m.info(r))
		seq = r.seq
	}
	page.NextCursor = apiv0.Cursor{Epoch: m.epoch, Offset: int(seq)}.Encode()
	return page, rows.Err()
}

// Upload stores a blob for a later push (BLAKE3; size -1 when unknown).
func (m *Manager) Upload(name string, r io.Reader, size int64) (*apiv0.CollectionUpload, error) {
	if _, err := m.receiving(name); err != nil {
		return nil, err
	}
	c, err := m.cfg.Store.AddBlobHash(r, size, cid.BLAKE3)
	if err != nil {
		return nil, err
	}
	info, _, err := m.cfg.Store.Has(c)
	if err != nil {
		return nil, err
	}
	return &apiv0.CollectionUpload{CID: c.String(), Size: int64(info.Size)}, nil
}

// ingestLoop stores the files of accepted pushes, a few pushes at a time.
func (m *Manager) ingestLoop() {
	defer m.wg.Done()
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	sem := make(chan struct{}, m.cfg.IngestWorkers)
	inflight := map[string]bool{}
	done := make(chan string, m.cfg.IngestWorkers)
	running := 0
	defer func() {
		for ; running > 0; running-- {
			<-done
		}
	}()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.ikick:
		case <-t.C:
		case k := <-done:
			delete(inflight, k)
			running--
			<-sem
		}
		// The literal terms match the pushes_todo partial index (apiv0.PushAccepted and
		// PushFetching).
		rows, err := m.db.Query(`SELECT ds, path, id FROM pushes INDEXED BY pushes_todo WHERE state IN ('accepted', 'fetching') AND entry IS NULL AND next_ms <= ? ORDER BY next_ms, seq LIMIT 64`,
			m.now().UnixMilli())
		if err != nil {
			continue
		}
		var jobs [][3]string
		for rows.Next() {
			var j [3]string
			if rows.Scan(&j[0], &j[1], &j[2]) == nil {
				jobs = append(jobs, j)
			}
		}
		rows.Close()
		for _, j := range jobs {
			key := j[0] + "\x00" + j[1]
			if inflight[key] {
				continue
			}
			select {
			case sem <- struct{}{}:
			default:
				continue // all workers busy; the next wake picks it up
			}
			inflight[key] = true
			running++
			go func() {
				m.ingest(j[0], j[1], j[2])
				done <- key
			}()
		}
	}
}

// update applies fn to the push (if it is still push id) and saves it.
func (m *Manager) update(name, path, id string, fn func(r *pushRow) error) (*pushRow, error) {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	r, err := m.loadPush(m.db, name, path)
	if err != nil || r == nil || r.id != id {
		return nil, err
	}
	if err := fn(r); err != nil {
		return nil, err
	}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := m.savePush(tx, r); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.notifyPushes()
	return r, nil
}

func asAPIError(err error) *apiv0.Error {
	var ae *apiv0.Error
	if errors.As(err, &ae) {
		return ae
	}
	var be *store.BudgetError
	if errors.As(err, &be) {
		return apiv0.Errorf(apiv0.CodeInsufficientStorage, "%v", err)
	}
	if errors.Is(err, httpsrc.ErrPolicy) {
		return apiv0.Errorf(apiv0.CodeForbidden, "%v", err)
	}
	return apiv0.Errorf(apiv0.CodeFailed, "%v", err)
}

// ingest works on one push: store the missing files, then build and queue the entry.
func (m *Manager) ingest(name, path, id string) {
	r, err := m.update(name, path, id, func(r *pushRow) error {
		if r.state != apiv0.PushAccepted && r.state != apiv0.PushFetching {
			return errStale
		}
		if r.state == apiv0.PushAccepted {
			r.state = apiv0.PushFetching
		}
		return nil
	})
	if err != nil || r == nil {
		return
	}
	d, ok := m.cfg.Cluster.View().Dataset(name)
	if !ok || d.Collection == nil {
		m.fail(r, apiv0.Errorf(apiv0.CodeNotFound, "collection %q no longer exists", name))
		return
	}
	limits, timeout := ingestLimits(d)
	for _, f := range r.req.Files {
		if _, ok := r.files[f.Key]; ok {
			continue
		}
		s, src, err := m.storeFile(r, d, f, limits, timeout)
		if m.ctx.Err() != nil {
			return
		}
		if err != nil {
			permanent := httpsrc.Permanent(err) || errors.Is(err, errLocal) || errors.Is(err, cid.ErrTooLarge)
			_, _ = m.update(name, path, id, func(r *pushRow) error {
				r.tries++
				r.err = apiv0.Errorf(asAPIError(err).Code, "file %q: %v", f.Key, err)
				if f.URL != "" {
					r.sources[f.Key] = src
				}
				if permanent || int64(r.tries) >= limits.Attempts {
					m.failLocked(r, r.err)
					return nil
				}
				r.nextMS = m.now().Add(m.backoff(r.tries)).UnixMilli()
				return nil
			})
			m.unpinAfterFail(name, path, id)
			return
		}
		c, _ := cid.Parse(s.CID)
		if err := m.cfg.Store.Pin(pushPin(name, path, id, c)); err != nil {
			return
		}
		r, err = m.update(name, path, id, func(r *pushRow) error {
			r.files[f.Key] = s
			if f.URL != "" {
				r.sources[f.Key] = src
			}
			return nil
		})
		if err != nil || r == nil { // superseded (or removed) meanwhile
			_, _ = m.cfg.Store.Unpin(pushPin(name, path, id, c))
			return
		}
	}
	entry, err := m.buildItem(r)
	if err != nil {
		m.fail(r, asAPIError(err))
		return
	}
	m.queueEntry(r, entry)
}

var (
	errStale = errors.New("push changed")
	errLocal = errors.New("local file")
)

func (m *Manager) backoff(tries int) time.Duration {
	d := m.cfg.IngestBackoff
	for i := 1; i < tries && d < MaxIngestBackoff; i++ {
		d *= 2
	}
	return min(d, MaxIngestBackoff)
}

// failLocked marks r failed (r is saved by the caller).
func (m *Manager) failLocked(r *pushRow, e *apiv0.Error) {
	r.state, r.err, r.nextMS = apiv0.PushFailed, e, 0
}

func (m *Manager) fail(r *pushRow, e *apiv0.Error) {
	if _, err := m.update(r.ds, r.path, r.id, func(r *pushRow) error { m.failLocked(r, e); return nil }); err == nil {
		m.unpinAfterFail(r.ds, r.path, r.id)
	}
}

func (m *Manager) unpinAfterFail(name, path, id string) {
	if r, err := m.loadPush(m.db, name, path); err == nil && r != nil && r.id == id && r.state == apiv0.PushFailed {
		m.unpinPush(r)
	}
}

// storeFile brings one file into the store.
func (m *Manager) storeFile(r *pushRow, d *cluster.Dataset, f pushFile, limits cluster.IngestLimits, timeout time.Duration) (stored, apiv0.URLSource, error) {
	src := apiv0.URLSource{URL: f.URL}
	switch {
	case f.Upload != "":
		c, _ := cid.Parse(f.Upload)
		info, ok, err := m.cfg.Store.Has(c)
		if err != nil {
			return stored{}, src, err
		}
		if !ok {
			return stored{}, src, fmt.Errorf("%w: upload %s is no longer held", errLocal, c)
		}
		return stored{CID: c.String(), Size: info.Size}, src, nil
	case f.Path != "":
		fi, err := os.Stat(f.Path)
		if err != nil || !fi.Mode().IsRegular() {
			return stored{}, src, fmt.Errorf("%w: %s is not a readable regular file", errLocal, f.Path)
		}
		var c cid.CID
		if f.NoCopy {
			c, err = m.cfg.Store.AddNoCopy(f.Path)
		} else {
			var fh *os.File
			if fh, err = os.Open(f.Path); err == nil {
				c, err = m.cfg.Store.AddBlobHash(fh, fi.Size(), cid.BLAKE3)
				fh.Close()
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				err = fmt.Errorf("%w: %v", errLocal, err)
			}
			return stored{}, src, err
		}
		return stored{CID: c.String(), Size: uint64(fi.Size())}, src, nil
	}
	hosts, ok := EgressHosts(m.cfg.Cluster.Mode(), d)
	if !ok || m.cfg.HTTP == nil {
		return stored{}, src, fmt.Errorf("%w: item URLs are not fetched on this node", httpsrc.ErrPolicy)
	}
	o := httpsrc.ImportOptions{Options: httpsrc.Options{MaxBytes: limits.MaxBytes, Timeout: timeout, IdleTimeout: IngestIdleTimeout, Hosts: hosts},
		Size: f.Size, Partial: m.partialPath(r, f)}
	if f.Checksum != "" {
		ck, err := httpsrc.ParseChecksum(f.Checksum)
		if err != nil {
			return stored{}, src, fmt.Errorf("%w: %v", errLocal, err)
		}
		o.Checksum = &ck
		src.Checksum = ck.String()
	}
	if err := os.MkdirAll(filepath.Dir(o.Partial), 0o700); err != nil {
		return stored{}, src, err
	}
	ctx, cancel := context.WithCancel(m.ctx)
	defer cancel()
	res, err := m.cfg.HTTP.Import(ctx, f.URL, m.cfg.Store, o)
	src.FinalURL, src.ETag, src.LastModified, src.ContentType, src.Size, src.Resumed =
		res.FinalURL, res.ETag, res.LastModified, res.ContentType, res.Size, res.Resumed
	if err != nil {
		return stored{}, src, err
	}
	if src.FinalURL == f.URL {
		src.FinalURL = ""
	}
	return stored{CID: res.CID.String(), Size: uint64(res.Size)}, src, nil
}

// partialPath is where a URL file's retained download lives between attempts.
func (m *Manager) partialPath(r *pushRow, f pushFile) string {
	h := sha256.Sum256([]byte(r.ds + "\x00" + r.path + "\x00" + r.id + "\x00" + f.Key))
	return filepath.Join(m.cfg.IngestDir, hex.EncodeToString(h[:16]))
}

// buildItem builds the item directory from the stored files.
func (m *Manager) buildItem(r *pushRow) (tree.Entry, error) {
	ed, err := newEditor(m.cfg.Store, cid.CID{}, 0)
	if err != nil {
		return tree.Entry{}, err
	}
	for _, f := range r.req.Files {
		s := r.files[f.Key]
		c, err := cid.Parse(s.CID)
		if err != nil {
			return tree.Entry{}, err
		}
		if err := ed.put(split(f.Key), tree.Entry{Kind: tree.KindFile, CID: c, Size: s.Size}); err != nil {
			return tree.Entry{}, err
		}
	}
	root, err := ed.flush()
	if err != nil {
		return tree.Entry{}, err
	}
	meta, err := cid.Parse(r.req.Meta)
	if err != nil {
		return tree.Entry{}, err
	}
	return tree.Entry{Kind: tree.KindDir, CID: root, Size: ed.root.size, Meta: meta}, nil
}

// ownEntry returns what this node's writer side has at path: a queued change (a nil
// entry for a queued removal) or else its current writer head's entry.
func (m *Manager) ownEntry(name, path string) (e *tree.Entry, queued bool, head cid.CID, err error) {
	var eb []byte
	err = m.db.QueryRow(`SELECT entry FROM pending WHERE ds = ? AND path = ?`, name, path).Scan(&eb)
	switch {
	case err == nil:
		if eb == nil {
			return nil, true, cid.CID{}, nil
		}
		var x tree.Entry
		if err := drisl.Unmarshal(eb, &x); err != nil {
			return nil, false, cid.CID{}, err
		}
		return &x, true, cid.CID{}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, cid.CID{}, err
	}
	v := m.cfg.Cluster.View()
	d, ok := v.Dataset(name)
	if !ok {
		return nil, false, cid.CID{}, nil
	}
	h, ok := v.Heads[name][m.WriterKey(v, d)]
	if !ok || h.Deleted || h.Value == nil {
		return nil, false, cid.CID{}, nil
	}
	if head, err = cid.Parse(h.Value.Target); err != nil {
		return nil, false, cid.CID{}, err
	}
	root, err := m.headerRoot(head)
	if err != nil {
		return nil, false, head, err
	}
	ed, err := newEditor(m.cfg.Store, root, 0)
	if err != nil {
		return nil, false, head, err
	}
	st, x, err := ed.lookup(split(path))
	if err != nil || st != leaf {
		return nil, false, head, err
	}
	return &x, false, head, nil
}

// queueEntry decides a built entry against this node's writer side and queues it.
func (m *Manager) queueEntry(r *pushRow, entry tree.Entry) {
	cur, queued, head, err := m.ownEntry(r.ds, r.path)
	if err != nil {
		m.log.Warn("item push: cannot read own head", "collection", r.ds, "path", r.path, "err", err)
		return // retried on the next ingest pass
	}
	eb, err := drisl.Marshal(entry)
	if err != nil {
		m.fail(r, asAPIError(err))
		return
	}
	var written bool
	m.pmu.Lock()
	cr, err := m.loadPush(m.db, r.ds, r.path)
	if err != nil || cr == nil || cr.id != r.id {
		m.pmu.Unlock()
		return
	}
	cr.entry, cr.size, cr.err, cr.nextMS = entry.CID, entry.Size, nil, 0
	tx, err := m.db.Begin()
	if err != nil {
		m.pmu.Unlock()
		return
	}
	switch {
	case cur != nil && *cur == entry:
		if !queued {
			cr.state, cr.head, written = apiv0.PushWritten, head, true
		}
	case cur != nil && !cr.replace:
		m.failLocked(cr, apiv0.Errorf(apiv0.CodeConflict, "a different entry (%s) is at %q in this node's writer head", cur.CID, r.path).
			WithAction("push with replace to supersede it, or remove the item first").WithDetail("existing", cur.CID.String()))
		written = true
	default:
		_, err = tx.Exec(`INSERT OR REPLACE INTO pending (ds, path, entry) VALUES (?, ?, ?)`, r.ds, r.path, eb)
	}
	if err == nil {
		err = m.savePush(tx, cr)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	m.pmu.Unlock()
	if err != nil {
		return
	}
	if written {
		m.unpinPush(cr)
	} else {
		m.scheduleFlush(r.ds)
	}
	m.notifyPushes()
}

// headWritten records the queued changes a head write cleared (in the head write's
// transaction): their pushes are written (removals get their head).
func (m *Manager) headWritten(tx *sql.Tx, name string, hc cid.CID, puts []pendingPut) ([]*pushRow, error) {
	var done []*pushRow
	for _, p := range puts {
		r, err := m.loadPush(tx, name, p.path)
		if err != nil {
			return nil, err
		}
		if r == nil {
			continue
		}
		switch {
		case p.entry == nil && r.state == apiv0.PushRemoved && !r.head.Defined():
		case p.entry != nil && r.state == apiv0.PushFetching && r.entry.Defined():
			var e tree.Entry
			if drisl.Unmarshal(p.entry, &e) != nil || e.CID != r.entry {
				continue
			}
			r.state = apiv0.PushWritten
		default:
			continue
		}
		r.head = hc
		if err := m.savePush(tx, r); err != nil {
			return nil, err
		}
		done = append(done, r)
	}
	return done, nil
}
