package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/tree"
)

// Inventory is a structure-only dry run of one UnixFS root (docs/migration.md "Dry run"):
// directories, HAMT shards, symlinks and the top node of each file are retrieved and
// verified under their original CIDs; file chunks and raw leaves are not. Bytes are
// therefore declared sizes (UnixFS filesize, raw-leaf link Tsize), not verified content.
type Inventory struct {
	IPFSRoot string `json:"ipfs_root"`
	Kind     string `json:"kind,omitempty"` // directory, file or symlink
	// Complete is false when a structural block was missing: the counts then cover only
	// what was reached.
	Complete               bool   `json:"complete"`
	StructureBlocks        int    `json:"structure_blocks"`
	Files                  uint64 `json:"files"`
	Directories            uint64 `json:"directories"`
	Symlinks               uint64 `json:"symlinks"`
	Bytes                  uint64 `json:"bytes"`
	EstimatedPhysicalBytes uint64 `json:"estimated_blob_physical_bytes"`
	Missing                []Item `json:"missing,omitempty"`
	Unsupported            []Item `json:"unsupported,omitempty"`
	Transformations        []Item `json:"transformations,omitempty"`
}

// Supported reports whether a full import of the inventoried root can succeed as far as
// the structure shows.
func (v Inventory) Supported() bool { return v.Complete && len(v.Unsupported) == 0 }

// TakeInventory walks root's structure from src. Missing and unsupported items are
// reported and the walk continues past them; only cancellation and the block budget end it
// early (as an error).
func TakeInventory(ctx context.Context, root IPFSCID, src BlockSource, opts ImportOptions) (Inventory, error) {
	if opts.MaxDepth == 0 {
		opts.MaxDepth = 1024
	}
	if opts.MaxBlocks == 0 {
		opts.MaxBlocks = 1 << 20
	}
	i := &importer{ctx: ctx, src: src, opts: opts, seen: map[IPFSCID]bool{}}
	inv := &Inventory{IPFSRoot: root.String(), Complete: true}
	kind, err := i.walk(inv, root, "", 0, 0, true)
	if err != nil {
		return Inventory{}, err
	}
	inv.Kind = kind
	inv.StructureBlocks = i.blocks
	inv.Transformations = i.changes
	if kind == tree.KindSymlink {
		inv.Unsupported = append(inv.Unsupported, Item{CID: root.String(), Reason: "a standalone symlink root has no entry name; import its containing directory"})
	}
	return *inv, nil
}

// fatal reports errors that end the walk instead of being itemized.
func (i *importer) fatal(err error) bool {
	return i.ctx.Err() != nil || errors.Is(err, errBlockLimit)
}

var errBlockLimit = errors.New("bridge: block limit exceeded")

func (i *importer) walk(inv *Inventory, c IPFSCID, path string, depth int, linkSize uint64, root bool) (string, error) {
	if depth > i.opts.MaxDepth {
		inv.Unsupported = append(inv.Unsupported, Item{Path: path, CID: c.String(), Reason: "UnixFS depth limit exceeded"})
		return "", nil
	}
	if c.Codec == codecRaw && !root {
		inv.addFile(linkSize)
		return tree.KindFile, nil
	}
	if i.seen[c] {
		inv.Unsupported = append(inv.Unsupported, Item{Path: path, CID: c.String(), Reason: "cyclic UnixFS DAG"})
		return "", nil
	}
	i.seen[c] = true
	defer delete(i.seen, c)
	b, err := i.block(c)
	var missing *MissingBlockError
	switch {
	case err == nil:
	case i.fatal(err):
		return "", err
	case errors.As(err, &missing):
		inv.Complete = false
		inv.Missing = append(inv.Missing, Item{Path: path, CID: c.String(), Reason: missing.Err.Error()})
		return "", nil
	default:
		inv.Unsupported = append(inv.Unsupported, Item{Path: path, CID: c.String(), Reason: err.Error()})
		return "", nil
	}
	if c.Codec == codecRaw {
		inv.addFile(uint64(len(b)))
		return tree.KindFile, nil
	}
	unsupported := func(err error) (string, error) {
		inv.Unsupported = append(inv.Unsupported, Item{Path: path, CID: c.String(), Reason: err.Error()})
		return "", nil
	}
	pb, err := DecodePBNode(b)
	if err != nil {
		return unsupported(err)
	}
	u, err := DecodeUnixFSData(pb.Data)
	if err != nil {
		return unsupported(err)
	}
	i.noteMeta(path, c, u)
	switch u.Type {
	case UnixFSFile:
		size := u.Filesize
		if !u.HasFilesize {
			size = uint64(len(u.Data))
			for _, s := range u.Blocksizes {
				size += s
			}
		}
		inv.addFile(size)
		return tree.KindFile, nil
	case UnixFSSymlink:
		inv.Symlinks++
		return tree.KindSymlink, nil
	case UnixFSDirectory, UnixFSHAMTShard:
		links := pb.Links
		if u.Type == UnixFSHAMTShard {
			if links, err = i.hamtLinks(c, u, pb.Links, depth); err != nil {
				if i.fatal(err) {
					return "", err
				}
				if errors.As(err, &missing) {
					inv.Complete = false
					inv.Missing = append(inv.Missing, Item{Path: path, CID: missing.CID.String(), Reason: missing.Err.Error()})
					return "", nil
				}
				return unsupported(err)
			}
		}
		inv.Directories++
		names := map[string]bool{}
		for _, l := range links {
			p := l.Name
			if path != "" {
				p = path + "/" + p
			}
			if err := tree.ValidName(l.Name); err != nil {
				inv.Unsupported = append(inv.Unsupported, Item{Path: p, Reason: fmt.Sprintf("unsafe IPFS name: %v", err)})
				continue
			}
			if names[l.Name] {
				inv.Unsupported = append(inv.Unsupported, Item{Path: p, Reason: "duplicate directory entry name"})
				continue
			}
			names[l.Name] = true
			child, err := ParseIPFSBytes(l.CID)
			if err != nil {
				inv.Unsupported = append(inv.Unsupported, Item{Path: p, Reason: err.Error()})
				continue
			}
			if _, err := i.walk(inv, child, p, depth+1, l.Tsize, false); err != nil {
				return "", err
			}
		}
		return tree.KindDir, nil
	default:
		return unsupported(fmt.Errorf("unsupported UnixFS type %d", u.Type))
	}
}

func (v *Inventory) addFile(size uint64) {
	v.Files++
	v.Bytes += size
	v.EstimatedPhysicalBytes += size
	if blob.HasOutboard(size) {
		v.EstimatedPhysicalBytes += blob.OutboardSize(size)
	}
}
