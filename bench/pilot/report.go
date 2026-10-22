package pilot

import (
	"fmt"
	"strings"
)

// Report is one runner invocation's results (results.json); Markdown renders the summary
// tables of docs/pilots.md "Result templates".
type Report struct {
	Note     string   `json:"note"`
	Commit   string   `json:"commit"`
	Go       string   `json:"go"`
	OS       string   `json:"os"`
	CPUModel string   `json:"cpu_model,omitempty"`
	CPUs     int      `json:"cpus"`
	MaxProcs int      `json:"gomaxprocs"`
	TmpDir   string   `json:"tmpdir"`
	Seed     int64    `json:"seed"`
	Scale    float64  `json:"scale"`
	Versions int      `json:"versions"`
	Timings  Timings  `json:"cluster_timings"`
	Shapes   []*Shape `json:"shapes"`
	// MaxRSS is the test process's peak resident set (every node of every shape).
	MaxRSS int64 `json:"max_rss"`
}

// Timings are the compressed cluster protocol windows the run used (production values
// are in docs/cluster.md); readiness and repair durations scale with them.
type Timings struct {
	Heartbeat, EvidenceFresh, DownAfter, UpAfter, Settle string
}

// Shape is one fixture's results.
type Shape struct {
	Name    string     `json:"name"`
	Archive []Version  `json:"archive"`
	Restore []Restore  `json:"restore"`
	Budget  *Budget    `json:"budget,omitempty"`
	Share   *Share     `json:"share,omitempty"`
	Cluster *Cluster   `json:"cluster,omitempty"`
	Errors  []string   `json:"errors,omitempty"`
	Prune   *PruneStep `json:"prune,omitempty"`
}

// Version is one archived version on the one-node archive.
type Version struct {
	V          int   `json:"v"`
	Files      int   `json:"files"`
	Logical    int64 `json:"logical"`
	WholeFile  int64 `json:"whole_file"`
	Novel      int64 `json:"novel"`
	NewBytes   int64 `json:"new_bytes"`   // the version record's new_bytes
	Stored     int64 `json:"stored"`      // Δ blob + outboard bytes
	DB         int64 `json:"db"`          // Δ database files (structure blocks, indexes)
	Physical   int64 `json:"physical"`    // Δ store physical bytes (A5)
	StoreTotal int64 `json:"store_total"` // physical bytes after the version
	AddSec     float64
	CPUSec     float64
}

// Restore is an exact-version extraction.
type Restore struct {
	Where    string  `json:"where"`
	V        int     `json:"v"`
	Bytes    int64   `json:"bytes"`
	Sec      float64 `json:"sec"`
	Verified bool    `json:"verified"`
	Served   int64   `json:"served"` // bytes peers served for it
}

// Budget is the over-budget probe: an update that does not fit store.limit.
type Budget struct {
	Limit, Usage, Need int64
	Refused            bool
	Code, Message      string
	Action             string
	HeldIntact         bool // every earlier version still complete and readable
	AfterRaise         bool // the update succeeded once the limit was raised
	Interventions      []string
}

// PruneStep is retention reclaim: prune to keep_last 1, then GC.
type PruneStep struct {
	Pruned    int   `json:"pruned"`
	FreeBytes int64 `json:"free_bytes"` // the dry run's estimate
	Reclaimed int64 `json:"reclaimed"`  // physical bytes actually freed
}

// Share is the first-magic journey (site shape).
type Share struct {
	AddSec, GetSec, BrowseSec float64
	Bytes, Served             int64
	Updates                   []ShareUpdate
	Steps                     []string
}

// ShareUpdate is a publisher's later version fetched by the same recipient.
type ShareUpdate struct {
	V             int
	Novel, Served int64
	GetSec        float64
}

// Cluster is the three-member part.
type Cluster struct {
	Members, Replicas int
	Versions          []ClusterVersion
	Release           *Release
	Storage           []int64 // per member physical bytes after release
	Restore           []Restore
	Repair            *Repair
	Interventions     []string
}

// ClusterVersion is one version written by the first member and replicated.
type ClusterVersion struct {
	V                  int
	Novel, WholeFile   int64
	Transfer           int64 // bytes served by all members until every owner held it
	StoredTotal        int64 // Δ physical bytes summed over members
	AddSec             float64
	ReadySec, HeldSec  float64
	ReadyBeats         float64 // ReadySec in heartbeats
	CPUSec             float64
	WriterSurplusBytes int64 `json:",omitempty"`
}

// Release is the writer's surplus release (S2: explicit gc).
type Release struct {
	Blobs, Bytes int64
	Sec          float64
}

// Repair is the loss of one member and re-replication by the survivors.
type Repair struct {
	DetectSec, RestoreSec float64
	Transfer              int64
	Stored                int64
	Ready                 bool
	Interventions         []string
}

// MiB formats bytes as MiB.
func MiB(n int64) string {
	switch {
	case n == 0:
		return "0"
	case n < 0:
		return "−" + MiB(-n)
	case n < 1<<20/100:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	}
	return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
}

func ratio(a, b int64) string {
	if b <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f×", float64(a)/float64(b))
}

func sec(s float64) string { return fmt.Sprintf("%.2f s", s) }

// Markdown renders the summary tables.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", r.Note)
	fmt.Fprintf(&b, "Commit %s, %s, %s, %s (%d CPUs, GOMAXPROCS %d), TMPDIR %s, seed %d, scale %g, %d versions; cluster heartbeat %s, evidence fresh %s, down_after %s, settle %s; process max RSS %s.\n\n",
		r.Commit, r.Go, r.OS, r.CPUModel, r.CPUs, r.MaxProcs, r.TmpDir, r.Seed, r.Scale, r.Versions,
		r.Timings.Heartbeat, r.Timings.EvidenceFresh, r.Timings.DownAfter, r.Timings.Settle, MiB(r.MaxRSS))

	b.WriteString("**One-node archive** (per update = mean over versions 1…n−1; stored = Δ blob and outboard bytes; DB = Δ database files; amplification = Δ physical store bytes ÷ novel bytes):\n\n")
	b.WriteString("| shape | v0 logical / files | v0 add | update: novel | whole-file changed | stored | DB | amplification | add | restore oldest | over-budget update |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range r.Shapes {
		if len(s.Archive) == 0 {
			continue
		}
		v0 := s.Archive[0]
		var novel, whole, stored, db, phys int64
		var add float64
		n := int64(len(s.Archive) - 1)
		for _, v := range s.Archive[1:] {
			novel += v.Novel
			whole += v.WholeFile
			stored += v.Stored
			db += v.DB
			phys += v.Physical
			add += v.AddSec
		}
		restore := "—"
		for _, rs := range s.Restore {
			if rs.Where == "archive" && rs.V == 0 {
				restore = sec(rs.Sec)
				if !rs.Verified {
					restore += " (NOT verified)"
				}
			}
		}
		budget := "—"
		if bu := s.Budget; bu != nil {
			switch {
			case bu.Refused && bu.HeldIntact && bu.AfterRaise:
				budget = "refused `" + bu.Code + "`, held intact, ok after raise"
			default:
				budget = fmt.Sprintf("refused=%v held=%v after-raise=%v", bu.Refused, bu.HeldIntact, bu.AfterRaise)
			}
		}
		if n == 0 {
			fmt.Fprintf(&b, "| %s | %s / %d | %s | — | — | — | — | — | — | %s | %s |\n", s.Name, MiB(v0.Logical), v0.Files, sec(v0.AddSec), restore, budget)
			continue
		}
		fmt.Fprintf(&b, "| %s | %s / %d | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", s.Name, MiB(v0.Logical), v0.Files, sec(v0.AddSec),
			MiB(novel/n), MiB(whole/n), MiB(stored/n), MiB(db/n), ratio(phys, novel), sec(add/float64(n)), restore, budget)
	}

	b.WriteString("\n**Retention reclaim** (history keep_last 2: prune after the over-budget probe's extra version, then store GC):\n\n| shape | pruned versions | dry-run estimate | reclaimed |\n|---|---|---|---|\n")
	for _, s := range r.Shapes {
		if p := s.Prune; p != nil {
			fmt.Fprintf(&b, "| %s | %d | %s | %s |\n", s.Name, p.Pruned, MiB(p.FreeBytes), MiB(p.Reclaimed))
		}
	}

	var cl bool
	for _, s := range r.Shapes {
		cl = cl || s.Cluster != nil
	}
	if cl {
		b.WriteString("\n**Three-member cluster, replicas 2** (writer is a member; per update = mean over versions 1…n−1; transfer floor ≈ 1.33 × novel, see docs/pilots.md):\n\n")
		b.WriteString("| shape | update: novel | transfer | transfer amplification | stored (all members) | ready | all owners held | writer gc freed | per-member storage | restore oldest (member) | repair: detect / restore / transfer | interventions |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, s := range r.Shapes {
			c := s.Cluster
			if c == nil || len(c.Versions) == 0 {
				continue
			}
			var novel, tr, st int64
			var ready, held float64
			vs := c.Versions[1:]
			if len(vs) == 0 {
				vs = c.Versions
			}
			for _, v := range vs {
				novel += v.Novel
				tr += v.Transfer
				st += v.StoredTotal
				ready += v.ReadySec
				held += v.HeldSec
			}
			n := int64(len(vs))
			freed := "—"
			if c.Release != nil {
				freed = MiB(c.Release.Bytes)
			}
			var per []string
			for _, x := range c.Storage {
				per = append(per, MiB(x))
			}
			restore := "—"
			for _, rs := range c.Restore {
				if rs.V == 0 {
					restore = fmt.Sprintf("%s, %s served", sec(rs.Sec), MiB(rs.Served))
					if !rs.Verified {
						restore += " (NOT verified)"
					}
				}
			}
			repair := "—"
			if rp := c.Repair; rp != nil {
				repair = fmt.Sprintf("%s / %s / %s", sec(rp.DetectSec), sec(rp.RestoreSec), MiB(rp.Transfer))
				if !rp.Ready {
					repair += " (NOT ready)"
				}
			}
			iv := append(append([]string{}, c.Interventions...), func() []string {
				if c.Repair != nil {
					return c.Repair.Interventions
				}
				return nil
			}()...)
			ivs := "none"
			if len(iv) > 0 {
				ivs = fmt.Sprintf("%d: %s", len(iv), strings.Join(iv, "; "))
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", s.Name, MiB(novel/n), MiB(tr/n), ratio(tr, novel), MiB(st/n),
				sec(ready/float64(n)), sec(held/float64(n)), freed, strings.Join(per, " / "), restore, repair, ivs)
		}
	}

	for _, s := range r.Shapes {
		if sh := s.Share; sh != nil {
			b.WriteString("\n**First magic** (`site`: sharer daemon → recipient with no daemon; browser daemon's gateway):\n\n")
			fmt.Fprintf(&b, "| add | get (no daemon) | gateway page after get | bytes | served |\n|---|---|---|---|---|\n| %s | %s | %s | %s | %s |\n\n", sec(sh.AddSec), sec(sh.GetSec), sec(sh.BrowseSec), MiB(sh.Bytes), MiB(sh.Served))
			b.WriteString("| publisher update | novel | served to recipient | get |\n|---|---|---|---|\n")
			for _, u := range sh.Updates {
				fmt.Fprintf(&b, "| v%d | %s | %s | %s |\n", u.V, MiB(u.Novel), MiB(u.Served), sec(u.GetSec))
			}
			fmt.Fprintf(&b, "\nSteps: %s.\n", strings.Join(sh.Steps, " → "))
		}
	}
	for _, s := range r.Shapes {
		for _, e := range s.Errors {
			fmt.Fprintf(&b, "\n- **%s error:** %s", s.Name, e)
		}
	}
	b.WriteString("\n")
	return b.String()
}
