// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeDiffReport saves a plain-text version of the comparison next to the
// snapshots, suitable for pasting into a change record or a ticket.
func writeDiffReport(d *Diff) (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("report-%s-to-%s-%s.txt",
		safeName(d.Before.Name), safeName(d.After.Name),
		d.After.TakenAt.Format("20060102-150405"))
	path := filepath.Join(dir, name)

	var b strings.Builder
	printDiff(&b, d, false, true)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// printDiff writes a comparison as text. When color is false all escape
// sequences are stripped, so the same rendering serves both a terminal and a
// file. When verbose is true, unchanged prefixes are listed too.
func printDiff(w io.Writer, d *Diff, color, verbose bool) {
	out := func(format string, args ...any) {
		s := fmt.Sprintf(format, args...)
		if !color {
			s = stripANSI(s)
		}
		fmt.Fprintln(w, s)
	}

	out("%s", gradientText("ROUTE PROBE — before/after comparison"))
	out("")
	out("  before   %s   taken %s on %s", colWhite.paint(d.Before.Name),
		d.Before.TakenAt.Format(time.RFC3339), d.Before.Host)
	out("  after    %s   taken %s on %s", colWhite.paint(d.After.Name),
		d.After.TakenAt.Format(time.RFC3339), d.After.Host)
	out("")

	headline := colGreen
	if d.Regressions() > 0 {
		headline = colRed
	}
	out("  %s", headline.paint(d.Headline()))
	out("")

	var chips []string
	for v := range numVerdicts {
		if n := d.Counts[v]; n > 0 {
			vd := Verdict(v)
			chips = append(chips, vd.Color().paintf("%s %s %d", vd.Glyph(), vd.Label(), n))
		}
	}
	out("  %s", strings.Join(chips, "  |  "))
	out("")

	printed := 0
	for _, p := range d.Prefixes {
		if !verbose && (p.Verdict == VerdictOK || p.Verdict == VerdictStillDown) {
			continue
		}
		printed++
		out("  %s %s", p.Verdict.Color().paintf("%s %s", p.Verdict.Glyph(), padRight(p.Verdict.Label(), 10)),
			colCyan.paint(p.Prefix.String()))
		for _, n := range p.Notes {
			out("      %s", colGray.paint("- "+n))
		}
		out("      %s reachable %s → %s   latency %s → %s   loss %.0f%% → %.0f%%",
			colGray.paint("·"),
			countPlain(p.AliveBefore, p.TotalBefore),
			countPlain(p.AliveAfter, p.TotalAfter),
			shortDur(p.RTTBefore), shortDur(p.RTTAfter),
			p.LossBefore, p.LossAfter)
		if p.PathChanged() {
			out("      %s via %s → %s", colGray.paint("·"),
				joinOrNone(p.NextHopsBefore), joinOrNone(p.NextHopsAfter))
		} else {
			out("      %s via %s", colGray.paint("·"), joinOrNone(p.NextHopsAfter))
		}
		out("")
	}
	if printed == 0 {
		out("  %s", colGreen.paint("No prefix-level changes."))
		out("")
	}

	if len(d.RouteChanges) > 0 {
		out("  %s", sgrBold+colMagenta.fg()+"ROUTE TABLE CHANGES"+sgrReset)
		out("  %s", colGray.paint("(includes prefixes with nothing pingable in them)"))
		out("")
		for _, c := range d.RouteChanges {
			tag := ""
			if !c.Measured {
				tag = colDark.paint("  [not measured]")
			}
			out("    %s %s%s", padRight(colCyan.paint(c.Prefix.String()), 24),
				colYellow.paint(c.Kind()), tag)
			out("        before: %s", joinOrNone(c.Before))
			out("        after:  %s", joinOrNone(c.After))
		}
		out("")
	}
}

func countPlain(alive, total int) string {
	if total == 0 {
		return "—"
	}
	return fmt.Sprintf("%d/%d", alive, total)
}

// printRoutes writes the route table as text.
func printRoutes(w io.Writer, routes []Route, color bool) {
	out := func(format string, args ...any) {
		s := fmt.Sprintf(format, args...)
		if !color {
			s = stripANSI(s)
		}
		fmt.Fprintln(w, s)
	}
	out("%s", gradientText("ROUTE PROBE — tailnet route table"))
	out("")
	out("  %s%s%s%s", padRight(colGray.paint("PREFIX"), 24), padRight(colGray.paint("NEXT HOP"), 18),
		padRight(colGray.paint("PEER"), 40), colGray.paint("STATE"))
	for _, r := range routes {
		state := colGreen.paint("online")
		if !r.Online {
			state = colRed.paint("offline")
		}
		out("  %s%s%s%s",
			cell(colCyan.paint(r.Prefix.String()), 24),
			cell(colBlue.paint(r.NextHop.String()), 18),
			cell(colWhite.paint(r.ShortName()), 40),
			state)
	}
	out("")
	out("  %s", colGray.paintf("%d routes", len(routes)))
}

// printTargets writes the discovered target set as text.
func printTargets(w io.Writer, t *Targets, color bool) {
	out := func(format string, args ...any) {
		s := fmt.Sprintf(format, args...)
		if !color {
			s = stripANSI(s)
		}
		fmt.Fprintln(w, s)
	}
	out("%s", gradientText("ROUTE PROBE — discovered targets"))
	out("")
	out("  %s", colGray.paintf("discovered %s on %s · %d addresses across %d of %d prefixes",
		t.DiscoveredAt.Format(time.RFC3339), t.Host, t.TotalTargets(), t.Covered(), len(t.Prefixes)))
	out("")
	for _, p := range t.Prefixes {
		var addrs []string
		for _, a := range p.Alive {
			addrs = append(addrs, a.String())
		}
		list := colDark.paint("(no responders)")
		if len(addrs) > 0 {
			list = colGreen.paint(strings.Join(addrs, " "))
		}
		out("  %s%s%s",
			cell(colCyan.paint(p.Prefix.String()), 22),
			cell(colGray.paintf("%d swept", p.Probed), 12),
			list)
	}
}

// printSnapshots lists saved snapshots.
func printSnapshots(w io.Writer, snaps []SnapshotInfo, color bool) {
	out := func(format string, args ...any) {
		s := fmt.Sprintf(format, args...)
		if !color {
			s = stripANSI(s)
		}
		fmt.Fprintln(w, s)
	}
	out("%s", gradientText("ROUTE PROBE — snapshots"))
	out("")
	if len(snaps) == 0 {
		out("  %s", colGray.paint("none yet"))
		return
	}
	for _, s := range snaps {
		out("  %s%s%s%s",
			cell(colWhite.paint(s.Name), 22),
			cell(colGray.paint(s.TakenAt.Format("2006-01-02 15:04:05")), 22),
			cell(colGreen.paintf("%d/%d up", s.Up, s.Prefixes), 16),
			colDark.paint(s.Note))
	}
}
