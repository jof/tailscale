// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Verdict is the headline judgement for one prefix, ordered worst-first so
// that sorting by Verdict puts the things you need to look at on top.
type Verdict int

const (
	VerdictBroken     Verdict = iota // reachable before, unreachable now
	VerdictWithdrawn                 // route disappeared from the tailnet
	VerdictLossUp                    // still up, but losing meaningfully more
	VerdictPathChange                // different next hop(s) than before
	VerdictSlower                    // materially higher latency
	VerdictGone                      // prefix was measured before, absent now
	VerdictNew                       // prefix appeared since the before capture
	VerdictFixed                     // unreachable before, reachable now
	VerdictFaster                    // materially lower latency
	VerdictLossDown                  // still up, losing meaningfully less
	VerdictStillDown                 // unreachable in both captures
	VerdictOK                        // no meaningful change
	numVerdicts
)

var verdictMeta = map[Verdict]struct {
	label string
	glyph string
	color rgb
}{
	VerdictBroken:     {"BROKEN", "✖", colRed},
	VerdictWithdrawn:  {"WITHDRAWN", "⊘", colRed},
	VerdictLossUp:     {"LOSSY", "▼", colOrange},
	VerdictPathChange: {"REROUTED", "⇄", colYellow},
	VerdictSlower:     {"SLOWER", "↑", colOrange},
	VerdictGone:       {"GONE", "–", colGray},
	VerdictNew:        {"NEW", "✚", colPurple},
	VerdictFixed:      {"FIXED", "✔", colGreen},
	VerdictFaster:     {"FASTER", "↓", colCyan},
	VerdictLossDown:   {"CLEANER", "▲", colCyan},
	VerdictStillDown:  {"DOWN", "·", colDark},
	VerdictOK:         {"OK", "=", colGreen},
}

func (v Verdict) Label() string { return verdictMeta[v].label }
func (v Verdict) Glyph() string { return verdictMeta[v].glyph }
func (v Verdict) Color() rgb    { return verdictMeta[v].color }

// Regression reports whether this verdict is something that got worse. These
// are what the summary line counts and what a CI-style exit code keys off.
func (v Verdict) Regression() bool {
	switch v {
	case VerdictBroken, VerdictWithdrawn, VerdictLossUp, VerdictSlower, VerdictGone:
		return true
	}
	return false
}

// Improvement reports whether this verdict is something that got better.
func (v Verdict) Improvement() bool {
	switch v {
	case VerdictFixed, VerdictFaster, VerdictLossDown:
		return true
	}
	return false
}

// Thresholds decide what counts as a real change rather than noise. Latency
// must move by both a relative and an absolute amount to be reported, so that
// a 0.4ms LAN destination going to 0.6ms doesn't show up as "+50% slower".
type Thresholds struct {
	RTTPct   float64       // minimum relative change, e.g. 25 for 25%
	RTTAbs   time.Duration // minimum absolute change
	LossPts  float64       // minimum change in loss percentage points
	MinAlive int           // targets that must answer for a prefix to count as up
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		RTTPct:   25,
		RTTAbs:   3 * time.Millisecond,
		LossPts:  20,
		MinAlive: 1,
	}
}

// PrefixDiff is the before/after comparison for one prefix.
type PrefixDiff struct {
	Prefix  netip.Prefix
	Verdict Verdict
	Notes   []string

	UpBefore, UpAfter       bool
	AliveBefore, AliveAfter int
	TotalBefore, TotalAfter int

	RTTBefore, RTTAfter time.Duration
	RTTDeltaPct         float64

	LossBefore, LossAfter float64

	NextHopsBefore, NextHopsAfter []string

	UnreachAfter int
}

// PathChanged reports whether the set of next hops differs.
func (d PrefixDiff) PathChanged() bool {
	return !equalStrings(d.NextHopsBefore, d.NextHopsAfter)
}

// Diff is a full before/after comparison.
type Diff struct {
	Before, After *Snapshot
	Thresholds    Thresholds
	Prefixes      []PrefixDiff
	Counts        [numVerdicts]int
	RouteChanges  []RouteChange
}

// Regressions is the number of prefixes that got worse.
func (d *Diff) Regressions() int {
	n := 0
	for v := range numVerdicts {
		if Verdict(v).Regression() {
			n += d.Counts[v]
		}
	}
	return n
}

// Improvements is the number of prefixes that got better.
func (d *Diff) Improvements() int {
	n := 0
	for v := range numVerdicts {
		if Verdict(v).Improvement() {
			n += d.Counts[v]
		}
	}
	return n
}

// Headline is the one-sentence summary shown at the top of the diff view.
func (d *Diff) Headline() string {
	r, i := d.Regressions(), d.Improvements()
	switch {
	case r == 0 && i == 0:
		return "No meaningful change — routing and reachability look identical."
	case r == 0:
		return fmt.Sprintf("%d improvement(s), no regressions. Ship it.", i)
	case i == 0:
		return fmt.Sprintf("%d regression(s). Something got worse.", r)
	}
	return fmt.Sprintf("%d regression(s) and %d improvement(s) — mixed result.", r, i)
}

// RouteChange records a route table difference for a prefix, including
// prefixes that had no live target to measure. A route can change without any
// measurable effect, and that's still worth knowing about.
type RouteChange struct {
	Prefix   netip.Prefix
	Before   []string
	After    []string
	Measured bool // true if this prefix also appears in Diff.Prefixes
}

// Kind describes the change in words.
func (c RouteChange) Kind() string {
	switch {
	case len(c.Before) == 0:
		return "added"
	case len(c.After) == 0:
		return "withdrawn"
	}
	return "next hop changed"
}

// Compare builds the full diff between two snapshots.
func Compare(before, after *Snapshot, th Thresholds) *Diff {
	if th.MinAlive <= 0 {
		th.MinAlive = 1
	}
	d := &Diff{Before: before, After: after, Thresholds: th}

	bp, ap := before.byPrefix(), after.byPrefix()

	// Union of prefixes, in stable address order.
	var all []netip.Prefix
	seen := map[netip.Prefix]bool{}
	for _, s := range []*Snapshot{before, after} {
		for _, p := range s.Prefixes {
			if !seen[p.Prefix] {
				seen[p.Prefix] = true
				all = append(all, p.Prefix)
			}
		}
	}
	sortPrefixes(all)

	for _, pfx := range all {
		b, hasB := bp[pfx]
		a, hasA := ap[pfx]
		d.Prefixes = append(d.Prefixes, comparePrefix(pfx, b, hasB, a, hasA, th))
	}

	for _, pd := range d.Prefixes {
		d.Counts[pd.Verdict]++
	}
	// Worst first, then by address so repeated runs are stable.
	sort.SliceStable(d.Prefixes, func(i, j int) bool {
		if d.Prefixes[i].Verdict != d.Prefixes[j].Verdict {
			return d.Prefixes[i].Verdict < d.Prefixes[j].Verdict
		}
		return prefixLess(d.Prefixes[i].Prefix, d.Prefixes[j].Prefix)
	})

	d.RouteChanges = compareRoutes(before.Routes, after.Routes, seen)
	return d
}

func comparePrefix(pfx netip.Prefix, b PrefixSnapshot, hasB bool, a PrefixSnapshot, hasA bool, th Thresholds) PrefixDiff {
	d := PrefixDiff{Prefix: pfx}

	if hasB {
		d.AliveBefore = b.AliveCount()
		d.TotalBefore = len(b.Targets)
		d.UpBefore = d.AliveBefore >= th.MinAlive
		d.RTTBefore = b.RTT()
		d.LossBefore = b.LossPct()
		d.NextHopsBefore = b.NextHops
	}
	if hasA {
		d.AliveAfter = a.AliveCount()
		d.TotalAfter = len(a.Targets)
		d.UpAfter = d.AliveAfter >= th.MinAlive
		d.RTTAfter = a.RTT()
		d.LossAfter = a.LossPct()
		d.NextHopsAfter = a.NextHops
		d.UnreachAfter = a.Unreachables()
	}

	if d.RTTBefore > 0 && d.RTTAfter > 0 {
		d.RTTDeltaPct = (float64(d.RTTAfter) - float64(d.RTTBefore)) / float64(d.RTTBefore) * 100
	}

	// Collect every signal, then pick the most severe as the headline.
	var signals []Verdict
	note := func(v Verdict, format string, args ...any) {
		signals = append(signals, v)
		d.Notes = append(d.Notes, fmt.Sprintf(format, args...))
	}

	switch {
	case !hasA:
		note(VerdictGone, "prefix was measured before but is missing from the after capture")
	case !hasB:
		note(VerdictNew, "new prefix, not present in the before capture")
	}

	if hasA && len(d.NextHopsAfter) == 0 && len(d.NextHopsBefore) > 0 {
		note(VerdictWithdrawn, "route withdrawn: no peer advertises this prefix any more")
	} else if hasA && hasB && d.PathChanged() {
		note(VerdictPathChange, "next hop: %s → %s",
			joinOrNone(d.NextHopsBefore), joinOrNone(d.NextHopsAfter))
	}

	if hasA && hasB {
		switch {
		case d.UpBefore && !d.UpAfter:
			reason := "no replies"
			if d.UnreachAfter > 0 {
				reason = fmt.Sprintf("%d ICMP unreachable(s)", d.UnreachAfter)
			}
			note(VerdictBroken, "was reachable (%d/%d targets), now unreachable — %s",
				d.AliveBefore, d.TotalBefore, reason)
		case !d.UpBefore && d.UpAfter:
			note(VerdictFixed, "was unreachable, now answering on %d/%d targets",
				d.AliveAfter, d.TotalAfter)
		case !d.UpBefore && !d.UpAfter:
			note(VerdictStillDown, "unreachable in both captures")
		}

		if d.UpBefore && d.UpAfter {
			if lossDelta := d.LossAfter - d.LossBefore; lossDelta >= th.LossPts {
				note(VerdictLossUp, "packet loss %.0f%% → %.0f%%", d.LossBefore, d.LossAfter)
			} else if -lossDelta >= th.LossPts {
				note(VerdictLossDown, "packet loss %.0f%% → %.0f%%", d.LossBefore, d.LossAfter)
			}

			delta := d.RTTAfter - d.RTTBefore
			if abs(delta) >= th.RTTAbs && absF(d.RTTDeltaPct) >= th.RTTPct {
				v := VerdictFaster
				if delta > 0 {
					v = VerdictSlower
				}
				note(v, "latency %s → %s (%+.0f%%)",
					shortDur(d.RTTBefore), shortDur(d.RTTAfter), d.RTTDeltaPct)
			}
		}
	}

	d.Verdict = VerdictOK
	if len(signals) > 0 {
		d.Verdict = slicesMin(signals)
	}
	return d
}

// compareRoutes diffs the raw route tables, so route changes are visible even
// for prefixes with nothing pingable in them.
func compareRoutes(before, after []Route, measured map[netip.Prefix]bool) []RouteChange {
	_, bm := groupByPrefix(before)
	_, am := groupByPrefix(after)

	var all []netip.Prefix
	seen := map[netip.Prefix]bool{}
	for _, m := range []map[netip.Prefix][]Route{bm, am} {
		for p := range m {
			if !seen[p] {
				seen[p] = true
				all = append(all, p)
			}
		}
	}
	sortPrefixes(all)

	var out []RouteChange
	for _, p := range all {
		b := nextHopSet(bm[p])
		a := nextHopSet(am[p])
		if equalStrings(a, b) {
			continue
		}
		out = append(out, RouteChange{Prefix: p, Before: b, After: a, Measured: measured[p]})
	}
	return out
}

// --- small helpers ---------------------------------------------------------

func sortPrefixes(ps []netip.Prefix) {
	sort.Slice(ps, func(i, j int) bool { return prefixLess(ps[i], ps[j]) })
}

func prefixLess(a, b netip.Prefix) bool {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c < 0
	}
	return a.Bits() > b.Bits()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func joinOrNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ", ")
}

func slicesMin(v []Verdict) Verdict {
	m := v[0]
	for _, x := range v[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// shortDur formats a duration the way a network engineer reads latency.
func shortDur(d time.Duration) string {
	switch {
	case d == 0:
		return "—"
	case d < time.Millisecond:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%.0fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
