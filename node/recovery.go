package node

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/car"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/node/token"
	"github.com/whyrusleeping/nu/store"
)

// Content export bundles and authority backups (docs/operations.md "Backup, export and
// recovery"). Both are offline: the caller holds the repo lock, so the pin set, the store
// and the cluster state are one consistent snapshot.

// Content bundle layout.
const (
	ExportFormat        = "nu-content-export"
	ExportBundleVersion = 1
	ExportManifestFile  = "manifest.json"
	ExportCAR           = "content.car"
	ExportBlobs         = "blobs"
)

// ExportPin is one pin of the exported pin table with what its walk reached.
type ExportPin struct {
	CID     string `json:"cid"`
	Mode    string `json:"mode"`
	Profile string `json:"profile,omitempty"`
	Source  string `json:"source"`
	Name    string `json:"name,omitempty"`
	Objects int    `json:"objects"`
	Missing int    `json:"missing"`
	Bytes   uint64 `json:"bytes"`
}

// ExportBlob is a raw blob above the CAR block limit, stored as blobs/<cid>.
type ExportBlob struct {
	CID  string `json:"cid"`
	Size uint64 `json:"size"`
}

// ExportDataset lists a dataset's versions at export time (an index for people and
// tools; restoring history needs the authority backup's signed state).
type ExportDataset struct {
	Scope    string          `json:"scope"`
	Name     string          `json:"name"`
	Versions []ExportVersion `json:"versions,omitempty"`
}

// ExportVersion is one version of an exported dataset.
type ExportVersion struct {
	TID          string `json:"tid"`
	Root         string `json:"root"`
	Label        string `json:"label,omitempty"`
	Availability string `json:"availability"`
}

// ExportManifestDoc is manifest.json of a content bundle.
type ExportManifestDoc struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	Created   time.Time `json:"created"`
	NuVersion string    `json:"nu_version"`
	PeerID    string    `json:"peer_id"`
	// Complete is true when every pin's walk reached only held objects.
	Complete bool        `json:"complete"`
	Pins     []ExportPin `json:"pins"`
	CAR      struct {
		File   string `json:"file"`
		Blocks int    `json:"blocks"`
		Bytes  int64  `json:"bytes"`
	} `json:"car"`
	Blobs    []ExportBlob    `json:"blobs,omitempty"`
	Datasets []ExportDataset `json:"datasets,omitempty"`
}

// ImportResult reports a content import.
type ImportResult struct {
	Pins       int         `json:"pins"`
	Blocks     int         `json:"blocks"`
	Blobs      int         `json:"blobs"`
	Bytes      int64       `json:"bytes"`
	Complete   bool        `json:"complete"`
	Incomplete []ExportPin `json:"incomplete,omitempty"`
}

// ExportContent writes every pin of the store, and everything their walk profiles reach
// that is held, to the bundle directory dir (absent or empty): content.car with each
// object at most 8 MiB once, larger BLAKE3 blobs as blobs/<cid>, and manifest.json
// written last. --nocopy sources are copied, so the bundle is an independent copy.
func (n *Local) ExportContent(ctx context.Context, dir string) (*ExportManifestDoc, error) {
	if err := emptyDir(dir); err != nil {
		return nil, err
	}
	pins, err := n.Store.Pins()
	if err != nil {
		return nil, err
	}
	var roots []cid.CID
	for _, p := range pins {
		if !slices.Contains(roots, p.CID) {
			roots = append(roots, p.CID)
		}
	}
	slices.SortFunc(roots, func(a, b cid.CID) int { return strings.Compare(a.String(), b.String()) })
	m := &ExportManifestDoc{Format: ExportFormat, Version: ExportBundleVersion, Created: time.Now().UTC(), NuVersion: nu.Version,
		PeerID: n.PeerID().String(), Complete: true}
	m.CAR.File = ExportCAR
	f, err := os.OpenFile(filepath.Join(dir, ExportCAR), os.O_CREATE|os.O_EXCL|os.O_WRONLY, repo.FileMode)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cw, err := car.NewWriter(f, car.Header{Roots: roots})
	if err != nil {
		return nil, err
	}
	seen := map[cid.Multihash]bool{}
	for _, p := range pins {
		st, err := n.Store.Walk(ctx, p, func(c cid.CID, held bool) error {
			if !held || seen[c.Multihash()] {
				return nil
			}
			seen[c.Multihash()] = true
			return n.exportObject(dir, c, cw, m)
		})
		if err != nil {
			return nil, fmt.Errorf("export %s (%s %s): %w", p.CID, p.Source, p.Name, err)
		}
		m.Pins = append(m.Pins, ExportPin{CID: p.CID.String(), Mode: p.Mode, Profile: p.Profile, Source: p.Source, Name: p.Name,
			Objects: st.Objects, Missing: st.Missing, Bytes: st.Bytes})
		if st.Missing > 0 {
			m.Complete = false
		}
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	m.Datasets = n.exportDatasets(ctx)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFileSync(filepath.Join(dir, ExportManifestFile), b); err != nil {
		return nil, err
	}
	return m, nil
}

func (n *Local) exportObject(dir string, c cid.CID, cw *car.Writer, m *ExportManifestDoc) error {
	if c.Codec() != cid.Raw {
		data, err := n.Store.Get(c)
		if err != nil {
			return err
		}
		m.CAR.Blocks++
		m.CAR.Bytes += int64(len(data))
		return cw.Put(car.Block{CID: c, Data: data})
	}
	b, err := n.Store.OpenBlob(c.Multihash())
	if err != nil {
		return err
	}
	defer b.Close()
	r := io.NewSectionReader(b, 0, int64(b.Size))
	if b.Size <= cid.MaxWholeObject {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		m.CAR.Blocks++
		m.CAR.Bytes += int64(len(data))
		return cw.Put(car.Block{CID: c, Data: data})
	}
	if err := os.MkdirAll(filepath.Join(dir, ExportBlobs), repo.DirMode); err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(dir, ExportBlobs, c.String()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, repo.FileMode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	m.Blobs = append(m.Blobs, ExportBlob{CID: c.String(), Size: b.Size})
	return nil
}

// exportDatasets lists the datasets and versions of the clusters that exist, without
// creating the local cluster.
func (n *Local) exportDatasets(ctx context.Context) []ExportDataset {
	var out []ExportDataset
	for _, s := range []struct{ scope, kind string }{{ScopeLocal, "local"}, {ScopeCluster, "net"}} {
		if !cluster.Exists(n.clusterDir(s.kind)) {
			continue
		}
		ds, err := n.Datasets(ctx, s.scope)
		if err != nil {
			continue
		}
		for _, d := range ds {
			e := ExportDataset{Scope: s.scope, Name: d.Name}
			if vs, err := n.DatasetVersions(ctx, s.scope, d.Name); err == nil {
				for _, v := range vs.Versions {
					e.Versions = append(e.Versions, ExportVersion{TID: v.TID, Root: v.Root, Label: v.Label, Availability: v.Availability})
				}
			}
			out = append(out, e)
		}
	}
	return out
}

// ImportContent verifies a content bundle into the store and restores its pin table: every
// CAR block and large blob is checked against its CID, every pin is walked, and a pin
// that was complete at export but is not now fails the import before anything is pinned.
// Pins that were incomplete at export are restored and reported.
func (n *Local) ImportContent(ctx context.Context, dir string) (*ImportResult, error) {
	b, err := os.ReadFile(filepath.Join(dir, ExportManifestFile))
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "%s is not a complete content export: %v", dir, err).
			WithAction("an export without manifest.json was interrupted; export again")
	}
	var m ExportManifestDoc
	if err := json.Unmarshal(b, &m); err != nil || m.Format != ExportFormat {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s: not a %s manifest", filepath.Join(dir, ExportManifestFile), ExportFormat)
	}
	if m.Version > ExportBundleVersion {
		return nil, apiv0.Errorf(apiv0.CodeNotSupported, "content export version %d is newer than this build (%d)", m.Version, ExportBundleVersion).
			WithAction("import it with the " + nu.Name + " release that wrote it (" + m.NuVersion + ") or newer")
	}
	res := &ImportResult{Complete: true}
	f, err := os.Open(filepath.Join(dir, ExportCAR))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cr, err := car.NewReader(f, car.Options{})
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "%s: %v", ExportCAR, err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		blk, dup, err := cr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "%s: block %d: %v", ExportCAR, res.Blocks, err).
				WithAction("the bundle is damaged; every block must match its CID")
		}
		if dup {
			continue
		}
		if err := n.Store.Put(blk.CID, blk.Data); err != nil {
			return nil, apiErr(err)
		}
		res.Blocks++
		res.Bytes += int64(len(blk.Data))
	}
	if res.Blocks < m.CAR.Blocks {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "%s holds %d of %d blocks", ExportCAR, res.Blocks, m.CAR.Blocks).
			WithAction("the bundle is truncated; copy or export it again")
	}
	for _, eb := range m.Blobs {
		c, err := cid.Parse(eb.CID)
		if err != nil || c.Codec() != cid.Raw {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "manifest blob %q is not a raw CID", eb.CID)
		}
		bf, err := os.Open(filepath.Join(dir, ExportBlobs, c.String()))
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "blob %s: %v", c, err).WithAction("the bundle is incomplete; copy or export it again")
		}
		_, err = n.Store.PutBlob(c, bf, int64(eb.Size))
		bf.Close()
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "blob %s does not verify: %v", c, err).WithAction("the bundle is damaged; export it again")
		}
		res.Blobs++
		res.Bytes += int64(eb.Size)
	}
	var restore []store.Pin
	for _, ep := range m.Pins {
		c, err := cid.Parse(ep.CID)
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "manifest pin %q: %v", ep.CID, err)
		}
		p := store.Pin{CID: c, Mode: ep.Mode, Profile: ep.Profile, Source: ep.Source, Name: ep.Name}
		st, err := n.Store.Walk(ctx, p, nil)
		if err != nil {
			return nil, apiErr(err)
		}
		if st.Missing > 0 {
			if ep.Missing == 0 {
				return nil, apiv0.Errorf(apiv0.CodeFailed, "pin %s (%s %s) was complete at export but %d of %d objects are missing from the bundle", c, ep.Source, ep.Name, st.Missing, st.Objects).
					WithAction("the bundle is damaged or truncated; nothing was pinned")
			}
			res.Complete = false
			ep.Objects, ep.Missing, ep.Bytes = st.Objects, st.Missing, st.Bytes
			res.Incomplete = append(res.Incomplete, ep)
		}
		restore = append(restore, p)
	}
	for _, p := range restore {
		if err := n.Store.Pin(p); err != nil {
			return nil, apiErr(err)
		}
		res.Pins++
	}
	return res, nil
}

// Authority backup layout (a tar archive, mode 0600, plaintext).
const (
	BackupFormat       = "nu-authority-backup"
	BackupVersion      = 1
	BackupManifestFile = "manifest.json"
	maxBackupEntry     = 256 << 20
	maxBackupFiles     = 100000
)

// BackupFile is one file of an authority backup.
type BackupFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// BackupCluster describes a cluster whose keys and signed state a backup holds.
type BackupCluster struct {
	Kind  string       `json:"kind"` // "local" or "net"
	ID    string       `json:"id"`
	Name  string       `json:"name"`
	Mode  string       `json:"mode"`
	Root  string       `json:"root_key"`
	Keys  []string     `json:"held_keys"`
	Heads []BackupHead `json:"heads"`
}

// BackupHead is one state repository head carried as a CAR.
type BackupHead struct {
	DID    string `json:"did"`
	Rev    string `json:"rev"`
	Commit string `json:"commit"`
	File   string `json:"file"`
}

// BackupManifestDoc is manifest.json of an authority backup.
type BackupManifestDoc struct {
	Format    string          `json:"format"`
	Version   int             `json:"version"`
	Created   time.Time       `json:"created"`
	NuVersion string          `json:"nu_version"`
	PeerID    string          `json:"peer_id"`
	Files     []BackupFile    `json:"files"`
	Clusters  []BackupCluster `json:"clusters,omitempty"`
	// Verified is set by a restore: the heads that verified against their did:keys.
	Verified int `json:"verified,omitempty"`
}

var clusterKinds = []string{"local", "net"}

// BackupAuthority writes the node's authority and signed state to w as a tar archive:
// node.key, config.toml, push tokens, atproto login sessions, and for each cluster its state.json, the private
// keys this node holds and a CAR of every held state repository. Content is not
// included (ExportContent). The archive holds private keys in plaintext: store it 0600
// on media the operator protects.
func (n *Local) BackupAuthority(w io.Writer) (*BackupManifestDoc, error) {
	m := &BackupManifestDoc{Format: BackupFormat, Version: BackupVersion, Created: time.Now().UTC(), NuVersion: nu.Version, PeerID: n.PeerID().String()}
	files := map[string][]byte{}
	add := func(rel string, data []byte) {
		sum := sha256.Sum256(data)
		files[rel] = data
		m.Files = append(m.Files, BackupFile{Path: rel, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	}
	addFile := func(rel string, required bool) error {
		data, err := os.ReadFile(n.Repo.Path(filepath.FromSlash(rel)))
		if errors.Is(err, os.ErrNotExist) && !required {
			return nil
		}
		if err != nil {
			return err
		}
		add(rel, data)
		return nil
	}
	if err := addFile(repo.KeyFile, true); err != nil {
		return nil, err
	}
	if err := addFile(repo.ConfigFile, false); err != nil {
		return nil, err
	}
	for _, dir := range []string{token.Dir, AuthDir} {
		ents, err := os.ReadDir(n.Repo.Path(dir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, e := range ents {
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") {
				if err := addFile(dir+"/"+e.Name(), true); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, kind := range clusterKinds {
		if !cluster.Exists(n.clusterDir(kind)) {
			continue
		}
		var c *cluster.Cluster
		var err error
		if kind == "net" {
			c, err = n.netCluster()
		} else {
			c, err = n.localCluster()
		}
		if err != nil {
			return nil, err
		}
		base := "cluster/" + kind
		if err := addFile(base+"/"+cluster.StateFile, true); err != nil {
			return nil, err
		}
		ents, err := os.ReadDir(filepath.Join(n.clusterDir(kind), cluster.KeysDir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, e := range ents {
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".key") {
				if err := addFile(base+"/"+cluster.KeysDir+"/"+e.Name(), true); err != nil {
					return nil, err
				}
			}
		}
		g := c.Genesis()
		bc := BackupCluster{Kind: kind, ID: c.ID.String(), Name: g.Name, Mode: g.Mode, Root: g.RootKey, Keys: c.HeldKeys()}
		for _, h := range c.Heads() {
			var buf bytes.Buffer
			if err := atrepo.Export(&buf, n.Store, h.Commit); err != nil {
				return nil, fmt.Errorf("state repository %s: %w", h.DID, err)
			}
			rel := base + "/repos/" + strings.TrimPrefix(h.DID, "did:key:") + ".car"
			add(rel, buf.Bytes())
			bc.Heads = append(bc.Heads, BackupHead{DID: h.DID, Rev: h.Rev, Commit: h.Commit.String(), File: rel})
		}
		m.Clusters = append(m.Clusters, bc)
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(w)
	put := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: m.Created, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := put(BackupManifestFile, mb); err != nil {
		return nil, err
	}
	for _, f := range m.Files {
		if err := put(f.Path, files[f.Path]); err != nil {
			return nil, err
		}
	}
	return m, tw.Close()
}

// backupPathOK reports whether rel may appear in an authority backup.
func backupPathOK(rel string) bool {
	if rel != path.Clean(rel) || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") || strings.ContainsAny(rel, "\\\x00") {
		return false
	}
	parts := strings.Split(rel, "/")
	switch {
	case rel == repo.KeyFile || rel == repo.ConfigFile:
		return true
	case len(parts) == 2 && (parts[0] == token.Dir || parts[0] == AuthDir):
		return strings.HasSuffix(parts[1], ".json")
	case len(parts) >= 3 && parts[0] == "cluster" && (parts[1] == "local" || parts[1] == "net"):
		switch {
		case len(parts) == 3:
			return parts[2] == cluster.StateFile
		case len(parts) == 4 && parts[2] == cluster.KeysDir:
			return strings.HasSuffix(parts[3], ".key")
		case len(parts) == 4 && parts[2] == "repos":
			return strings.HasSuffix(parts[3], ".car")
		}
	}
	return false
}

// RestoreAuthority restores an authority backup into the repo r, which must hold no
// cluster state and either no node key or the same one (restore into a fresh $NU_PATH
// before importing content). Every file is checked against the manifest; then the state
// repositories are loaded into the store and each cluster's heads are verified against
// their did:keys and pinned. The caller holds the repo lock and no node is open on r.
func RestoreAuthority(r *repo.Repo, rd io.Reader) (*BackupManifestDoc, error) {
	tr := tar.NewReader(rd)
	var m *BackupManifestDoc
	files := map[string][]byte{}
	for count := 0; ; count++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "not an authority backup: %v", err)
		}
		if count > maxBackupFiles || h.Size > maxBackupEntry || h.Typeflag != tar.TypeReg {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "backup entry %q is not a regular file within limits", h.Name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxBackupEntry))
		if err != nil {
			return nil, err
		}
		if h.Name == BackupManifestFile && m == nil {
			m = &BackupManifestDoc{}
			if err := json.Unmarshal(data, m); err != nil || m.Format != BackupFormat {
				return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "not a %s manifest", BackupFormat)
			}
			if m.Version > BackupVersion {
				return nil, apiv0.Errorf(apiv0.CodeNotSupported, "authority backup version %d is newer than this build (%d)", m.Version, BackupVersion).
					WithAction("restore it with the " + nu.Name + " release that wrote it (" + m.NuVersion + ") or newer")
			}
			continue
		}
		if !backupPathOK(h.Name) {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "backup entry %q is not an authority file", h.Name)
		}
		files[h.Name] = data
	}
	if m == nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "the backup has no %s", BackupManifestFile)
	}
	if len(files) != len(m.Files) {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "the backup holds %d files, its manifest lists %d", len(files), len(m.Files))
	}
	for _, f := range m.Files {
		data, ok := files[f.Path]
		sum := sha256.Sum256(data)
		if !ok || hex.EncodeToString(sum[:]) != f.SHA256 {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "backup file %s is missing or damaged", f.Path).WithAction("use another copy of the backup")
		}
	}
	if _, ok := files[repo.KeyFile]; !ok {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "the backup has no %s", repo.KeyFile)
	}

	// Preconditions: never merge into or overwrite another identity or cluster.
	for _, kind := range clusterKinds {
		if cluster.Exists(r.Path(filepath.Join("cluster", kind))) {
			return nil, apiv0.Errorf(apiv0.CodeConflict, "%s already holds cluster state (%s)", r.Root(), kind).
				WithAction("restore into a fresh " + repo.EnvPath)
		}
	}
	if cur, err := os.ReadFile(r.Path(repo.KeyFile)); err == nil && !bytes.Equal(cur, files[repo.KeyFile]) {
		return nil, apiv0.Errorf(apiv0.CodeConflict, "%s already has a different node key", r.Root()).
			WithAction("restore into a fresh " + repo.EnvPath + " first, then import content into it")
	}

	var repos []string
	for _, f := range m.Files {
		if strings.Contains(f.Path, "/repos/") {
			repos = append(repos, f.Path)
			continue
		}
		p := r.Path(filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), repo.DirMode); err != nil {
			return nil, err
		}
		if err := writeFileSync(p, files[f.Path]); err != nil {
			return nil, err
		}
	}
	n, err := NewLocal(r)
	if err != nil {
		return nil, err
	}
	defer n.Close()
	for _, rel := range repos {
		cr, err := car.NewReader(bytes.NewReader(files[rel]), car.Options{})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		for {
			blk, _, err := cr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			if err := n.Store.Put(blk.CID, blk.Data); err != nil {
				return nil, err
			}
		}
	}
	for _, bc := range m.Clusters {
		c, err := cluster.Open(cluster.Options{Dir: n.clusterDir(bc.Kind), Store: n.Store})
		if err != nil {
			return nil, err
		}
		if c.ID.String() != bc.ID {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "restored %s cluster is %s, the manifest says %s", bc.Kind, c.ID, bc.ID)
		}
		heads, err := c.VerifyHeads()
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "%v", err).WithAction("the backup's signed state does not verify; use another copy")
		}
		m.Verified += len(heads)
	}
	return m, nil
}

// emptyDir creates dir (0700) or accepts an existing empty directory.
func emptyDir(dir string) error {
	if ents, err := os.ReadDir(dir); err == nil {
		if len(ents) > 0 {
			return apiv0.Errorf(apiv0.CodeConflict, "%s is not empty", dir).WithAction("export into a new or empty directory")
		}
		return nil
	}
	return os.MkdirAll(dir, repo.DirMode)
}

// writeFileSync writes a 0600 file via a synced temp file and rename.
func writeFileSync(p string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(repo.FileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
