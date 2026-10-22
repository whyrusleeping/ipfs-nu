// Command fixturegen writes a synthetic pilot-benchmark fixture (bench/fixtures) to a
// directory, for runs against a real daemon and CLI (docs/pilots.md):
//
//	fixturegen -list
//	fixturegen -shape rewrite -version 0 -out ./src          # empty or missing ./src
//	fixturegen -shape rewrite -from 0 -version 1 -out ./src  # ./src holds version 0
//
// It prints the version's change summary (bench/fixtures.Change) as one JSON line:
// record it beside the measured add/transfer/storage figures. Synthetic fixtures are not
// pilot data.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/whyrusleeping/nu/bench/fixtures"
)

func main() {
	shape := flag.String("shape", "", "fixture shape (see -list)")
	seed := flag.Int64("seed", 1, "random seed")
	scale := flag.Float64("scale", 1, "scale factor (see -list for what it multiplies)")
	version := flag.Int("version", 0, "version to write")
	from := flag.Int("from", -1, "version the output directory already holds (-1: empty)")
	out := flag.String("out", "", "output directory")
	list := flag.Bool("list", false, "list the shapes")
	flag.Parse()
	if *list {
		for _, s := range fixtures.Shapes {
			fmt.Printf("%-8s %-21s %s\n%8s %-21s change: %s\n%8s %-21s scale: %s\n", s.Name, s.Audience, s.Models, "", "", s.Change, "", "", s.Scales)
		}
		return
	}
	s, ok := fixtures.Lookup(*shape)
	if !ok || *out == "" || *version < 0 || *from >= *version {
		fmt.Fprintf(os.Stderr, "usage: fixturegen -shape {%v} -version N [-from M<N] -out DIR [-seed S] [-scale F]\n", fixtures.Names())
		os.Exit(2)
	}
	p := fixtures.Params{Seed: *seed, Scale: *scale}
	vs := s.Versions(p, *version+1)
	var held []fixtures.File
	if *from >= 0 {
		held = vs[*from]
	} else if ents, err := os.ReadDir(*out); err == nil && len(ents) > 0 {
		fmt.Fprintf(os.Stderr, "%s is not empty: pass -from with the version it holds\n", *out)
		os.Exit(2)
	}
	if err := fixtures.Materialize(*out, held, vs[*version]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var prev []fixtures.File
	if *version > 0 {
		prev = vs[*version-1]
	}
	c := fixtures.Diff(prev, vs[*version])
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		Shape   string  `json:"shape"`
		Seed    int64   `json:"seed"`
		Scale   float64 `json:"scale"`
		Version int     `json:"version"`
		fixtures.Change
	}{s.Name, *seed, *scale, *version, c})
}
