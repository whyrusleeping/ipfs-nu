package fixtures

import (
	"fmt"
	"time"
)

// Shapes are the workload matrix's synthetic fixtures (docs/pilots.md). Sizes are the
// defaults at Scale 1.
var Shapes = []Shape{
	{
		Name: "site", Audience: "individual/publisher",
		Models:   "first share and publisher updates: a small static site with images and one media file",
		Change:   "a new post, a rewritten index page and one new image per version",
		Measures: "first-magic add/get/browse time and steps; publisher update cost",
		Scales:   "post count and media size", init: siteInit, step: siteStep,
	},
	{
		Name: "sparse", Audience: "institutional",
		Models:   "sparse independently changing small objects (documents, records, thumbnails)",
		Change:   "1% of files rewritten, 0.5% added, 0.2% removed per version",
		Measures: "per-file overhead (structure, DB, transfer requests) and directory-diff cost",
		Scales:   "file count (2,000 files of 256 B–256 KiB, median 4 KiB)", init: sparseInit, step: sparseStep,
	},
	{
		Name: "rewrite", Audience: "institutional",
		Models:   "monolithic in-place rewrites (a SQLite/HDF5/Zarr-v2-like database or image file)",
		Change:   "16 aligned 4 KiB page writes and a 64 KiB tail growth per version",
		Measures: "whole-file amplification: stored and transferred bytes per novel byte",
		Scales:   "file size (32 MiB)", init: rewriteInit, step: rewriteStep,
	},
	{
		Name: "shift", Audience: "institutional",
		Models:   "monolithic files with inserted records (an uncompressed tar or text export)",
		Change:   "3 records of 2–16 KiB inserted at random offsets, shifting the rest",
		Measures: "amplification under shifting edits (what fixed-offset vs content-defined chunking would recover)",
		Scales:   "file size (16 MiB)", init: shiftInit, step: shiftStep,
	},
	{
		Name: "append", Audience: "institutional",
		Models:   "appends and log-structured data (logs, WAL segments, JSONL feeds) with rotation",
		Change:   "each of 4 logs grows 256 KiB; logs over 4 MiB rotate into archive/ (a rename); a small index is rewritten",
		Measures: "append amplification of whole growing files; rename dedup",
		Scales:   "append size and rotation threshold", init: appendInit, step: appendStep,
	},
	{
		Name: "sharded", Audience: "institutional",
		Models:   "sharded indexes (hash-partitioned key/value or search index shards)",
		Change:   "400 64-byte record updates and 64 appended records spread over 256 shards; a manifest is rewritten",
		Measures: "fan-out of small logical changes into many whole-shard changes",
		Scales:   "shard size (256 × 64 KiB)", init: shardedInit, step: shardedStep,
	},
	{
		Name: "dump", Audience: "institutional",
		Models:   "Wikipedia-dump-like periodic snapshots: dated directories of compressed parts, a latest/ copy, rolling retention",
		Change:   "a new dated dump of 10 parts (a quarter byte-identical to the previous dump), latest/ replaced, dumps older than 3 removed",
		Measures: "storage growth per snapshot, cross-directory dedup, retention reclaim",
		Scales:   "part size (8 × 1.5 MiB + 2 × 256 KiB)", init: dumpInit, step: dumpStep,
	},
	{
		Name: "items", Audience: "institutional",
		Models:   "Internet-Archive-collection-like many-item trees: items/<id>/ with metadata, originals and derivatives",
		Change:   "10% new items, metadata edits on 3%, a derivative added to 1%, 0.5% of items removed (darked)",
		Measures: "growth and per-item overhead of a many-item tree; small metadata edits next to large unchanged originals",
		Scales:   "item count (100 items, originals median 48 KiB)", init: itemsInit, step: itemsStep,
	},
}

func siteInit(g *gen) {
	g.put("index.html", 6<<10)
	g.put("style.css", 3<<10)
	for range g.n(30) {
		siteStepPost(g)
	}
	for i := range 12 {
		g.put(fmt.Sprintf("assets/img%03d.jpg", i), g.lognormal(120<<10, 20<<10, 600<<10, 0.8))
	}
	g.put("media/talk.webm", g.size(4<<20))
}

func siteStepPost(g *gen) {
	g.put(fmt.Sprintf("posts/%04d.html", g.counter("post")), g.lognormal(8<<10, 2<<10, 40<<10, 0.6))
}

func siteStep(g *gen, v int) {
	siteStepPost(g)
	g.put("index.html", 6<<10+int64(v)*200)
	g.put(fmt.Sprintf("assets/img%03d.jpg", 11+g.counter("img")), g.lognormal(120<<10, 20<<10, 600<<10, 0.8))
}

func sparsePath(g *gen) string {
	i := g.counter("file")
	return fmt.Sprintf("d%02d/obj%06d.bin", i%64, i)
}

func sparseInit(g *gen) {
	for range g.n(2000) {
		g.put(sparsePath(g), g.lognormal(4<<10, 256, 256<<10, 1.2))
	}
}

func sparseStep(g *gen, _ int) {
	all := g.paths("")
	for _, p := range g.pick(all, max(1, len(all)/100)) {
		g.put(p, g.lognormal(4<<10, 256, 256<<10, 1.2))
	}
	for _, p := range g.pick(all, max(1, len(all)/500)) {
		g.del(p)
	}
	for range max(1, len(all)/200) {
		g.put(sparsePath(g), g.lognormal(4<<10, 256, 256<<10, 1.2))
	}
}

func rewriteInit(g *gen) {
	g.put("db/main.db", g.size(32<<20)/4096*4096)
	g.put("db/README.txt", 2<<10)
}

func rewriteStep(g *gen, _ int) {
	const page = 4096
	f := g.files["db/main.db"]
	pages := f.Size() / page
	for range 16 {
		g.overwrite("db/main.db", g.rng.Int64N(pages)*page, page)
	}
	g.overwrite("db/main.db", f.Size(), 64<<10) // growth at the tail
}

func shiftInit(g *gen) { g.put("export/records.tar", g.size(16<<20)) }

func shiftStep(g *gen, _ int) {
	for range 3 {
		size := g.files["export/records.tar"].Size()
		g.insert("export/records.tar", g.rng.Int64N(size), 2<<10+g.rng.Int64N(14<<10))
	}
}

var logs = []string{"api", "ingest", "auth", "worker"}

func appendInit(g *gen) {
	for _, l := range logs {
		g.appendTo("logs/"+l+".log", g.rng.Int64N(g.size(3<<20))+1)
	}
	g.put("logs/index.json", 2<<10)
}

func appendStep(g *gen, _ int) {
	for _, l := range logs {
		p := "logs/" + l + ".log"
		g.appendTo(p, g.size(256<<10))
		if g.files[p].Size() > g.size(4<<20) {
			g.rename(p, fmt.Sprintf("logs/archive/%s.%04d.log", l, g.counter("rot:"+l)))
		}
	}
	g.put("logs/index.json", 2<<10)
}

const record = 64

func shardedInit(g *gen) {
	for i := range 256 {
		g.put(fmt.Sprintf("index/%02x.idx", i), g.size(64<<10)/record*record)
	}
	g.put("index/manifest.json", 4<<10)
}

func shardedStep(g *gen, _ int) {
	for range 400 {
		p := fmt.Sprintf("index/%02x.idx", g.rng.IntN(256))
		g.overwrite(p, g.rng.Int64N(g.files[p].Size()/record)*record, record)
	}
	for range 64 {
		p := fmt.Sprintf("index/%02x.idx", g.rng.IntN(256))
		g.overwrite(p, g.files[p].Size(), record)
	}
	g.put("index/manifest.json", 4<<10)
}

var dumpEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func dumpParts() []string {
	var out []string
	for i := 1; i <= 8; i++ {
		out = append(out, fmt.Sprintf("pages-articles-multistream%d.xml.bz2", i))
	}
	return append(out, "stub-articles.xml.gz", "abstract.xml.gz")
}

func dumpPartSize(g *gen, name string) int64 {
	if len(name) > 5 && (name[:5] == "stub-" || name[:5] == "abstr") {
		return g.size(256 << 10)
	}
	return g.size(3<<20) / 2
}

func dumpInit(g *gen) { dumpStep(g, 0) }

func dumpStep(g *gen, v int) {
	date := dumpEpoch.AddDate(0, 0, 14*v).Format("20060102")
	prev := ""
	if v > 0 {
		prev = dumpEpoch.AddDate(0, 0, 14*(v-1)).Format("20060102")
	}
	for _, d := range g.paths("latest/") {
		g.del(d)
	}
	for i, part := range dumpParts() {
		p := "dumps/" + date + "/" + part
		if old, ok := g.files["dumps/"+prev+"/"+part]; ok && i%4 == 0 {
			old.Path = p // unchanged namespace: byte-identical part
			g.files[p] = old
		} else {
			g.put(p, dumpPartSize(g, part))
		}
		f := g.files[p]
		f.Path = "latest/" + part
		g.files[f.Path] = f
	}
	g.put("dumps/"+date+"/md5sums.txt", 1<<10)
	g.put("dumps/"+date+"/dumpstatus.json", 2<<10)
	if v >= 3 {
		gone := dumpEpoch.AddDate(0, 0, 14*(v-3)).Format("20060102")
		for _, p := range g.paths("dumps/" + gone + "/") {
			g.del(p)
		}
	}
}

func itemPut(g *gen) {
	id := fmt.Sprintf("nu-fixture-%06d", g.counter("item"))
	dir := "items/" + id + "/"
	g.put(dir+id+"_meta.xml", 1<<10+g.rng.Int64N(2<<10))
	g.put(dir+id+"_files.xml", 1<<10+g.rng.Int64N(1<<10))
	for i := range 1 + g.rng.IntN(4) {
		g.put(fmt.Sprintf("%s%s_%02d.orig", dir, id, i), g.lognormal(48<<10, 4<<10, 4<<20, 1.0))
	}
	g.put(dir+"__ia_thumb.jpg", 8<<10+g.rng.Int64N(16<<10))
}

func itemIDs(g *gen) []string {
	var out []string
	for _, p := range g.paths("items/") {
		if len(p) > 10 && p[len(p)-9:] == "_meta.xml" {
			out = append(out, p[:len(p)-len("_meta.xml")])
		}
	}
	return out
}

func itemsInit(g *gen) {
	for range g.n(100) {
		itemPut(g)
	}
}

func itemsStep(g *gen, _ int) {
	ids := itemIDs(g) // items/<id>/<id>
	for _, base := range g.pick(ids, max(1, len(ids)*3/100)) {
		g.put(base+"_meta.xml", 1<<10+g.rng.Int64N(2<<10))
		g.put(base+"_files.xml", 1<<10+g.rng.Int64N(1<<10))
	}
	for _, base := range g.pick(ids, max(1, len(ids)/100)) {
		g.put(base+".pdf", g.lognormal(256<<10, 16<<10, 4<<20, 0.8))
	}
	for _, base := range g.pick(ids, max(1, len(ids)/200)) {
		for _, p := range g.paths(itemDir(base)) {
			g.del(p)
		}
	}
	for range max(1, len(ids)/10) {
		itemPut(g)
	}
}

// itemDir is items/<id>/ for items/<id>/<id>.
func itemDir(base string) string {
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '/' {
			return base[:i+1]
		}
	}
	return base
}
