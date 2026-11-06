package main

import (
	"io"
	"testing"
)

// The release gate: a deterministic pair (including public↔random) with one
// relayed trial, an EIM↔random pair below 98 % or short of the campaign, and a
// missing lab row all fail; APDM↔APDM is reported only.
func TestGate(t *testing.T) {
	rows := map[[3]string]*row{}
	add := func(a, b string, direct, trials int) {
		k := pairKey(a, b)
		rows[[3]string{"pair", k[0], k[1]}] = &row{a: k[0], b: k[1], scenario: "pair", direct: direct, trials: trials}
	}
	for _, k := range labRows() {
		add(k[0], k[1], 50, 50)
	}
	add("APDM-seq/APDF", "APDM-random/APDF", 0, 3)
	if failed := render(io.Discard, rows, true, 50); len(failed) != 0 {
		t.Fatal("passing table failed:", failed)
	}
	add("EIM/EIF", "public", 2, 3)
	add("APDM-random/APDF", "EIM/APDF", 97, 100)
	add("APDM-random/APDF", "EIM/EIF", 10, 10)
	add("APDM-random/APDF", "public", 49, 50)
	k := pairKey("IPv6", "IPv6")
	delete(rows, [3]string{"pair", k[0], k[1]})
	if failed := render(io.Discard, rows, true, 50); len(failed) != 5 {
		t.Fatal("want 5 failures, got", failed)
	}
}
