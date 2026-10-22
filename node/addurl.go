package node

import (
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/node/op"
	"github.com/whyrusleeping/nu/store"
)

// URL import limits for `nu add https://…` (D17): BLAKE3 imports are bounded only by
// the store's blob limit, SHA-256 ones by the 8 MiB whole-object limit.
const (
	AddURLTimeout  = 24 * time.Hour
	AddURLAttempts = 4
)

// addURL starts a URL import: the bytes are fetched through httpsrc's guards, hashed
// into the store as they arrive (resuming on a strong validator after an interruption)
// and added as a file node, with the provenance in the result.
func (n *Local) addURL(req apiv0.AddRequest) (*apiv0.Operation, error) {
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not an https:// URL", req.URL)
	}
	if req.Path != "" || req.Recursive || req.NoCopy {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "a URL import takes no path, -r or --nocopy")
	}
	h := cid.BLAKE3
	switch req.Hash {
	case "", "blake3":
	case "sha256":
		h = cid.SHA256
	default:
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "unknown hash %q", req.Hash).WithAction("use blake3 (default) or sha256")
	}
	if c, _ := n.netCluster(); c != nil && c.Mode() == cluster.ModeIsolated {
		return nil, apiv0.Errorf(apiv0.CodeForbidden, "this node is in an isolated cluster, which has no egress").
			WithAction("download the file elsewhere and add it from a local path")
	}
	hc := n.httpClient()
	if err := hc.Check(req.URL, nil); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "cannot import %s: %v", req.URL, err).
			WithAction("URL imports need a public https:// URL without credentials")
	}
	return n.start("add", req.URL, func(o *op.Operation) (any, error) {
		o.Stage("fetching")
		last := time.Now()
		max := int64(store.MaxBlobSize)
		if h == cid.SHA256 {
			max = cid.MaxWholeObject
		}
		imp, err := hc.Import(o.Context(), req.URL, n.Store, httpsrc.ImportOptions{
			Options: httpsrc.Options{MaxBytes: max, Timeout: AddURLTimeout, IdleTimeout: time.Minute},
			Hash:    h, Dir: n.Repo.Path("staging"), Attempts: AddURLAttempts,
			Progress: func(done int64) {
				if time.Since(last) > 100*time.Millisecond {
					o.Progress(done, 0, "bytes")
					last = time.Now()
				}
			}})
		if err != nil {
			if h == cid.SHA256 && errors.Is(err, httpsrc.ErrTooLarge) {
				return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is larger than the 8 MiB limit of --hash sha256 blobs", req.URL).
					WithAction("import it with the default BLAKE3 hash, which has no size limit")
			}
			return nil, apiErr(err)
		}
		o.Progress(imp.Size, imp.Size, "bytes")
		src := &apiv0.URLSource{URL: req.URL, FinalURL: imp.FinalURL, ETag: imp.ETag, LastModified: imp.LastModified,
			ContentType: imp.ContentType, Size: imp.Size, Resumed: imp.Resumed}
		if src.FinalURL == req.URL {
			src.FinalURL = ""
		}
		name := urlName(imp.FinalURL)
		root := imp.CID
		if !req.Raw {
			node := tree.FileNode{Src: imp.CID, Nu: tree.FileMeta{Size: uint64(imp.Size), Name: name}}
			c, data, err := drisl.Sum(node)
			if err != nil {
				return nil, err
			}
			if err := n.Store.Put(c, data); err != nil {
				return nil, apiErr(err)
			}
			root = c
		}
		if req.Pin {
			o.Stage("pinning")
			if err := n.Store.Pin(pinFor(root, "")); err != nil {
				return nil, err
			}
			if err := n.pinned(root, req.NoAnnounce); err != nil {
				return nil, err
			}
		}
		res := &apiv0.AddResult{CID: root.String(), Name: name, Size: imp.Size, Files: 1, Link: n.Link(root, "").String(), Source: src}
		return res, n.addMeta(res, imp.CID, tree.KindFile, uint64(imp.Size), req.Meta, src)
	}), nil
}

// urlName is the file name a URL import records: the last path segment, else "download".
func urlName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "download"
	}
	name := path.Base(u.Path)
	if tree.ValidName(name) != nil || name == "/" || name == "." {
		return "download"
	}
	return name
}

// addMeta stores an add's metadata record (with a URL import's provenance under
// "source" unless the record has one) and fills the result's Meta and Entry.
func (n *Local) addMeta(res *apiv0.AddResult, target cid.CID, kind string, size uint64, raw json.RawMessage, src *apiv0.URLSource) error {
	if len(raw) == 0 {
		return nil
	}
	v, err := drisl.FromJSON(raw, drisl.JSONOptions{MaxSize: tree.MaxMetaSize})
	if err != nil {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "meta: %v", err)
	}
	rec, ok := v.(map[string]any)
	if !ok {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "meta must be a JSON object")
	}
	if _, has := rec["source"]; src != nil && !has {
		s := map[string]any{"url": src.URL, "size": src.Size}
		for k, v := range map[string]string{"final_url": src.FinalURL, "etag": src.ETag, "last_modified": src.LastModified} {
			if v != "" {
				s[k] = v
			}
		}
		rec["source"] = s
	}
	c, b, err := drisl.Sum(rec)
	if err != nil {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "meta: %v", err)
	}
	if len(b) > tree.MaxMetaSize {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "meta exceeds %d bytes", tree.MaxMetaSize)
	}
	if err := n.Store.Put(c, b); err != nil {
		return apiErr(err)
	}
	if err := n.Store.Pin(store.Pin{CID: c, Mode: store.Direct, Source: store.SourceExplicit}); err != nil {
		return err
	}
	res.Meta = c.String()
	res.Entry = &apiv0.TreeEntry{Kind: kind, CID: target.String(), Size: size, Meta: c.String()}
	return nil
}
