package cluster

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/core/drisl"
)

// Version history (docs/cluster.md "Version history"): merging the signers' version
// records, the retention live set, and version lookup.

// VersionEntry is one merged version: the canonical identity record with the winning
// annotations, and any conflicting records.
type VersionEntry struct {
	TID  string
	Time time.Time // the TID's HLC time (ordering metadata, not a trusted timestamp)
	// Version is the canonical record; Label and Note are the winning annotations.
	Version
	// Signer wrote the canonical record; AnnotatedBy the winning annotation.
	Signer, AnnotatedBy string
	Conflicts           []VersionConflict
}

// VersionConflict is a record for the same TID whose identity differs from the
// canonical one (never merged into the version).
type VersionConflict struct {
	Signer, Root, Author string
}

// VersionHistory merges the authorized signers' version records of a dataset in TID order;
// compacted versions (a winning tombstone) are left out.
func (v *View) VersionHistory(name string) []VersionEntry {
	var out []VersionEntry
	for tid, by := range v.Versions[name] {
		if e, ok := mergeVersion(tid, by); ok {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b VersionEntry) int { return strings.Compare(a.TID, b.TID) })
	return out
}

func mergeVersion(tid string, by map[string]Signed[Version]) (VersionEntry, bool) {
	signers := make([]string, 0, len(by))
	for s := range by {
		signers = append(signers, s)
	}
	slices.Sort(signers)
	// Canonical: the earliest (hlc, signer) record its author signed itself, else the
	// earliest record. A later self-authored redefinition is therefore a conflict.
	canon, own := "", false
	for _, s := range signers {
		r := by[s]
		if r.Deleted || r.Value == nil {
			continue
		}
		self := r.Value.Author == s
		switch {
		case canon == "", self && !own, self == own && r.HLC < by[canon].HLC:
			canon, own = s, self
		}
	}
	if canon == "" {
		return VersionEntry{}, false // only tombstones
	}
	c := by[canon]
	e := VersionEntry{TID: tid, Version: *c.Value, Signer: canon, AnnotatedBy: canon}
	if h, err := ParseHLC(tid); err == nil {
		e.Time = h.Time()
	} else if t, err := parseTID(tid); err == nil {
		e.Time = time.UnixMicro(int64(t >> 10))
	}
	win, winDel := c, false
	winSigner := canon
	for _, s := range signers {
		r := by[s]
		if !r.Deleted && r.Value.Identity() != c.Value.Identity() {
			e.Conflicts = append(e.Conflicts, VersionConflict{Signer: s, Root: r.Value.Root, Author: r.Value.Author})
			continue
		}
		if r.HLC > win.HLC || (r.HLC == win.HLC && s > winSigner) {
			win, winSigner, winDel = r, s, r.Deleted
		}
	}
	if winDel {
		return VersionEntry{}, false
	}
	e.Label, e.Note, e.HLC, e.AnnotatedBy = win.Value.Label, win.Value.Note, win.Value.HLC, winSigner
	return e, true
}

// Retention is a dataset's live set: the versions retention keeps as acquisition and
// retention obligations, with the reasons each is kept.
type Retention struct {
	Live map[string][]string // tid → reasons (keep_last, hourly, …, label, head)
	// ID is the live-set identity (the `live` binding of reports): the CID of the DRISL
	// {$type: "io.ipfs.nu.cluster.liveset", name, versions: [[tid, root]…]}.
	ID string
	// Bytes is the summed newBytes of the live set; Excess the part over max_bytes that
	// protected (labeled, head) versions alone exceed it by.
	Bytes, Excess int64
}

// Periods of the retention buckets, in schedule order.
var retainPeriods = []struct {
	name string
	d    time.Duration
}{{"hourly", time.Hour}, {"daily", 24 * time.Hour}, {"weekly", 7 * 24 * time.Hour}, {"monthly", 30 * 24 * time.Hour}, {"yearly", 365 * 24 * time.Hour}}

// Retain computes the live set of a dataset's merged versions (in TID order) under h,
// with headTID the current head's version ("" if none).
func Retain(name string, versions []VersionEntry, h *History, headTID string) Retention {
	r := Retention{Live: map[string][]string{}}
	keep := func(tid, why string) {
		if !slices.Contains(r.Live[tid], why) {
			r.Live[tid] = append(r.Live[tid], why)
		}
	}
	n := len(versions)
	if h != nil && n > 0 {
		for i := 0; i < n && (h.KeepLast < 0 || int64(i) < h.KeepLast); i++ {
			keep(versions[n-1-i].TID, "keep_last")
		}
		newest := versions[n-1].Time
		counts := []int64{h.Hourly, h.Daily, h.Weekly, h.Monthly, h.Yearly}
		for pi, p := range retainPeriods {
			count := counts[pi]
			if count == 0 {
				continue
			}
			seen := map[int64]bool{}
			for i := n - 1; i >= 0; i-- { // newest first: the first of a bucket is its newest
				b := int64(newest.Sub(versions[i].Time) / p.d)
				if b < 0 {
					b = 0
				}
				if seen[b] || (count > 0 && b >= count) {
					continue
				}
				seen[b] = true
				keep(versions[i].TID, p.name)
			}
		}
	}
	for _, v := range versions {
		if v.Label != "" {
			keep(v.TID, "label")
		}
	}
	if headTID != "" {
		keep(headTID, "head")
	}
	protected := func(tid string) bool {
		return slices.Contains(r.Live[tid], "label") || slices.Contains(r.Live[tid], "head")
	}
	for _, v := range versions {
		if _, ok := r.Live[v.TID]; ok {
			r.Bytes += v.NewBytes
		}
	}
	if h != nil && h.MaxBytes > 0 {
		for _, v := range versions { // oldest first
			if r.Bytes <= h.MaxBytes {
				break
			}
			if _, ok := r.Live[v.TID]; ok && !protected(v.TID) {
				delete(r.Live, v.TID)
				r.Bytes -= v.NewBytes
			}
		}
		r.Excess = max(r.Bytes-h.MaxBytes, 0)
	}
	type pair = []any
	list := []any{}
	for _, v := range versions {
		if _, ok := r.Live[v.TID]; ok {
			list = append(list, pair{v.TID, v.Root})
		}
	}
	if c, _, err := drisl.Sum(map[string]any{"$type": "io.ipfs.nu.cluster.liveset", "name": name, "versions": list}); err == nil {
		r.ID = c.String()
	}
	return r
}

// HeadVersion returns the TID of the newest version whose root is target ("" if none).
func HeadVersion(versions []VersionEntry, target string) string {
	for i := len(versions) - 1; i >= 0; i-- {
		if versions[i].Root == target {
			return versions[i].TID
		}
	}
	return ""
}

// Version selection errors.
var (
	ErrNoVersion        = errors.New("no such version")
	ErrAmbiguousVersion = errors.New("ambiguous version")
)

// ParseVersionTime parses a time selector: RFC 3339 (seconds optional), or a date
// YYYY-MM-DD meaning the end of that day in UTC.
func ParseVersionTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Add(24*time.Hour - time.Microsecond), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a time (RFC 3339 or YYYY-MM-DD)", s)
}

// SelectVersion selects a version by TID, time (the newest version at or before it) or
// label, without falling back to another version: a time before every version, an
// unknown TID or label is ErrNoVersion; a label on two versions is ErrAmbiguousVersion.
func SelectVersion(versions []VersionEntry, sel string) (VersionEntry, error) {
	if _, err := parseTID(sel); err == nil && len(sel) == 13 {
		for _, v := range versions {
			if v.TID == sel {
				return v, nil
			}
		}
		return VersionEntry{}, fmt.Errorf("%w: no version %s", ErrNoVersion, sel)
	}
	if t, err := ParseVersionTime(sel); err == nil {
		for i := len(versions) - 1; i >= 0; i-- {
			if !versions[i].Time.After(t) {
				return versions[i], nil
			}
		}
		return VersionEntry{}, fmt.Errorf("%w: no version at or before %s", ErrNoVersion, t.UTC().Format(time.RFC3339))
	}
	var found []VersionEntry
	for _, v := range versions {
		if v.Label == sel {
			found = append(found, v)
		}
	}
	switch len(found) {
	case 0:
		return VersionEntry{}, fmt.Errorf("%w: no version labeled %q", ErrNoVersion, sel)
	case 1:
		return found[0], nil
	}
	tids := make([]string, len(found))
	for i, v := range found {
		tids[i] = v.TID
	}
	return VersionEntry{}, fmt.Errorf("%w: label %q is on versions %s", ErrAmbiguousVersion, sel, strings.Join(tids, ", "))
}
