// Command nattable turns NAT-lab outcome records (NDJSON natlab.Outcome lines, as
// written via NU_NATLAB_RESULTS) into the published per-pair success table and
// fails on a regression.
//
//	nattable [-lab] [-campaign N] results.ndjson... > nat-pairs.md
//
// Records with scenario "pair" and outcome "direct" or "relayed" are grouped by
// unordered side labels. A pair with at least one EIM, public, router-mapped or
// IPv6 side must always be direct, except EIM↔random-APDM, which must be ≥ 98 %
// over at least -campaign trials. Other pairs (APDM↔APDM) and other scenarios
// are reported, not gated. -lab also fails when a software-lab row is missing.
// Exit status 1 means a gated row failed or was missing.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

type record struct {
	Scenario     string        `json:"scenario"`
	From         string        `json:"from"`
	To           string        `json:"to"`
	Outcome      string        `json:"outcome"`
	TimeToDirect time.Duration `json:"time_to_direct_ns"`
}

type row struct {
	a, b, scenario string
	direct, trials int
	times          []time.Duration
}

// labRows are the software-lab pairs p2p/disco's TestHardNATLab and
// TestNATLab(PublicEIM|IPv6|MappedMiniUPnPd) must produce.
func labRows() [][2]string {
	kinds := []string{"public", "EIM/EIF", "EIM/APDF", "APDM-seq/APDF", "APDM-random/APDF"}
	var out [][2]string
	for i := range kinds {
		for j := i; j < len(kinds); j++ {
			if eimClass(kinds[i]) || eimClass(kinds[j]) {
				out = append(out, pairKey(kinds[i], kinds[j]))
			}
		}
	}
	return append(out, pairKey("IPv6", "IPv6"), pairKey("mapped (UPnP)", "EIM/APDF"))
}

func eimClass(l string) bool {
	return l == "public" || l == "IPv6" || strings.HasPrefix(l, "EIM/") || strings.HasPrefix(l, "mapped")
}

func pairKey(a, b string) [2]string {
	if b < a {
		a, b = b, a
	}
	return [2]string{a, b}
}

// threshold returns the required direct rate, or 0 for a reported-only pair.
func threshold(a, b string) float64 {
	switch {
	case !eimClass(a) && !eimClass(b):
		return 0
	case strings.HasPrefix(a, "APDM-random") && strings.HasPrefix(b, "EIM/"),
		strings.HasPrefix(b, "APDM-random") && strings.HasPrefix(a, "EIM/"):
		return 0.98 // the only allowance: public, mapped and IPv6 sides stay always-direct
	}
	return 1
}

func main() {
	lab := flag.Bool("lab", false, "fail when a software NAT-lab row is missing")
	campaign := flag.Int("campaign", 1, "minimum trials for a ≥ 98 % row")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: nattable [-lab] [-campaign N] results.ndjson...")
		os.Exit(2)
	}
	rows := map[[3]string]*row{}
	for _, path := range flag.Args() {
		if err := read(path, rows); err != nil {
			fmt.Fprintln(os.Stderr, "nattable:", err)
			os.Exit(2)
		}
	}
	failed := render(os.Stdout, rows, *lab, *campaign)
	for _, f := range failed {
		fmt.Fprintln(os.Stderr, "nattable: FAIL", f)
	}
	if len(failed) > 0 {
		os.Exit(1)
	}
}

func read(path string, rows map[[3]string]*row) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var r record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return fmt.Errorf("%s:%d: %v", path, line, err)
		}
		if r.Outcome != "direct" && r.Outcome != "relayed" {
			continue // other natlab evidence (AutoNAT, netcheck) is not a pair outcome
		}
		k := pairKey(r.From, r.To)
		key := [3]string{r.Scenario, k[0], k[1]}
		x := rows[key]
		if x == nil {
			x = &row{a: k[0], b: k[1], scenario: r.Scenario}
			rows[key] = x
		}
		x.trials++
		if r.Outcome == "direct" {
			x.direct++
			if r.TimeToDirect > 0 {
				x.times = append(x.times, r.TimeToDirect)
			}
		}
	}
	return sc.Err()
}

func render(w io.Writer, rows map[[3]string]*row, lab bool, campaign int) (failed []string) {
	var list []*row
	for _, r := range rows {
		list = append(list, r)
	}
	if lab {
		for _, k := range labRows() {
			if rows[[3]string{"pair", k[0], k[1]}] == nil {
				list = append(list, &row{a: k[0], b: k[1], scenario: "pair"})
			}
		}
	}
	slices.SortFunc(list, func(x, y *row) int {
		return strings.Compare(x.scenario+"\x00"+x.a+"\x00"+x.b, y.scenario+"\x00"+y.a+"\x00"+y.b)
	})
	fmt.Fprintln(w, "| Pair | Scenario | Required | Direct / trials | Rate | Time to direct (median / max) | Result |")
	fmt.Fprintln(w, "|---|---|---|---:|---:|---|---|")
	for _, r := range list {
		want := 0.0
		if r.scenario == "pair" {
			want = threshold(r.a, r.b)
		}
		required, result := "reported", "info"
		switch {
		case want == 1:
			required = "always direct"
		case want > 0:
			required = fmt.Sprintf("≥ %.0f %% direct, ≥ %d trials", want*100, campaign)
		}
		rate := "–"
		if r.trials > 0 {
			rate = fmt.Sprintf("%.1f %%", 100*float64(r.direct)/float64(r.trials))
		}
		if want > 0 {
			switch {
			case r.trials == 0:
				result = "**MISSING**"
			case float64(r.direct) < want*float64(r.trials) || (want < 1 && r.trials < campaign):
				result = "**FAIL**"
			default:
				result = "pass"
			}
			if result != "pass" {
				failed = append(failed, fmt.Sprintf("%s ↔ %s: %d/%d direct (%s)", r.a, r.b, r.direct, r.trials, required))
			}
		}
		times := "–"
		if len(r.times) > 0 {
			slices.Sort(r.times)
			times = fmt.Sprintf("%s / %s", r.times[len(r.times)/2].Round(time.Millisecond), r.times[len(r.times)-1].Round(time.Millisecond))
		}
		fmt.Fprintf(w, "| %s ↔ %s | %s | %s | %d / %d | %s | %s | %s |\n", r.a, r.b, r.scenario, required, r.direct, r.trials, rate, times, result)
	}
	return failed
}
