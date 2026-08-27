// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"strings"
	"time"
)

// bgSelect is the highlight behind the cursor row.
var bgSelect = rgb{0x3a, 0x1d, 0x5c}

const (
	chromeHeader   = 3 // title bar, status strip, rule
	chromeFooter   = 2 // rule, key hints
	progressHeight = 7
	promptHeight   = 2
)

func (a *app) bodyHeight() int {
	_, h := a.scr.Size()
	bh := h - chromeHeader - chromeFooter
	if a.running {
		bh -= progressHeight
	}
	if prompt.active {
		bh -= promptHeight
	}
	if bh < 3 {
		bh = 3
	}
	return bh
}

func (a *app) render() {
	w, h := a.scr.Size()
	bh := a.bodyHeight()

	var lines []string
	lines = append(lines, a.renderHeader(w)...)
	if a.running {
		lines = append(lines, a.renderProgress(w)...)
	}
	if prompt.active {
		lines = append(lines, a.renderPrompt(w)...)
	}
	lines = append(lines, padLines(a.renderBody(w, bh), bh)...)
	lines = append(lines, a.renderFooter(w)...)
	a.scr.Render(lines, w, h)
}

// --- chrome ----------------------------------------------------------------

var viewNames = map[view]string{
	viewHome:    "dashboard",
	viewRoutes:  "route table",
	viewTargets: "targets",
	viewSnaps:   "snapshots",
	viewDiff:    "comparison",
	viewDetail:  "prefix detail",
	viewHelp:    "help",
}

func (a *app) renderHeader(w int) []string {
	title := sgrBold + colWhite.fg() + " ⚡ ROUTE PROBE " + sgrReset + bgHeader.bg()
	mid := colPink.fg() + viewNames[a.view] + bgHeader.bg()
	right := colGray.fg() + hostname() + "  " + time.Now().Format("15:04:05") + " " + bgHeader.bg()

	left := bgHeader.bg() + title + colDark.fg() + "│ " + bgHeader.bg() + mid
	gap := w - dispWidth(left) - dispWidth(right)
	if gap < 1 {
		gap = 1
	}
	bar := left + strings.Repeat(" ", gap) + right + sgrReset

	return []string{bar, a.renderStatusStrip(w), gradientRule(w)}
}

// renderStatusStrip is the always-visible summary of what state we're in.
func (a *app) renderStatusStrip(w int) string {
	var parts []string

	if a.err != "" {
		parts = append(parts, colRed.paint("⚠ "+a.err))
	} else {
		parts = append(parts, chip("routes", fmt.Sprintf("%d", len(a.routes)), colCyan))
	}

	if a.targets == nil {
		parts = append(parts, chip("targets", "none", colDark))
	} else {
		parts = append(parts, chip("targets", fmt.Sprintf("%d in %d pfx (%s)",
			a.targets.TotalTargets(), a.targets.Covered(), ago(a.targets.DiscoveredAt)), colGreen))
	}

	parts = append(parts, chip("before", snapChip(a.snaps, a.beforeNm), colYellow))
	parts = append(parts, chip("after", snapChip(a.snaps, a.afterNm), colYellow))

	if a.flash != "" && time.Now().Before(a.flashUntil) {
		parts = append(parts, colMagenta.paint("✦ "+a.flash))
	}

	return " " + truncate(strings.Join(parts, colDark.paint(" • ")), w-2)
}

func snapChip(snaps []SnapshotInfo, name string) string {
	for _, s := range snaps {
		if s.Name == name {
			return fmt.Sprintf("%s (%s)", name, ago(s.TakenAt))
		}
	}
	return name + " (—)"
}

func chip(label, value string, c rgb) string {
	return colGray.paint(label+":") + c.paint(value)
}

func (a *app) renderFooter(w int) []string {
	var keys []string
	add := func(k, desc string) {
		keys = append(keys, colYellow.paint("["+k+"]")+colGray.paint(desc))
	}
	switch a.view {
	case viewDiff:
		add("↑↓", "move")
		add("↵", "detail")
		add("f", "filter:"+a.filter.String())
		add("w", "write report")
		add("esc", "back")
	case viewSnaps:
		add("↑↓", "move")
		add("1", "set before")
		add("2", "set after")
		add("↵", "compare")
		add("esc", "back")
	case viewDetail:
		add("esc", "back to diff")
	case viewHelp:
		add("esc", "back")
	default:
		add("d", "discover")
		add("b", "before")
		add("a", "after")
		add("c", "compare")
		add("r", "routes")
		add("t", "targets")
		add("s", "snaps")
	}
	if a.running {
		add("x", "stop")
	}
	add("?", "help")
	add("q", "quit")
	return []string{
		hrule(w, colDark),
		" " + truncate(strings.Join(keys, colDark.paint(" ")), w-2),
	}
}

func (a *app) renderProgress(w int) []string {
	p := a.prog
	frac := p.Frac()
	barW := w - 34
	if barW < 10 {
		barW = 10
	}
	elapsed := time.Since(a.runStart).Round(100 * time.Millisecond)

	head := fmt.Sprintf(" %s %s %s",
		colMagenta.paint(spinner(a.tick)),
		sgrBold+colWhite.fg()+a.runKind+sgrReset,
		colGray.paint("· "+elapsed.String()))

	meter := fmt.Sprintf(" %s %s %s",
		bar(frac, barW),
		colCyan.paintf("%5.1f%%", frac*100),
		colGray.paintf("%d/%d probes", p.Sent.Load(), p.Total.Load()))

	stats := fmt.Sprintf("   %s   %s   %s",
		chip("replies", fmt.Sprintf("%d", p.Replies.Load()), colGreen),
		chip("live hosts", fmt.Sprintf("%d", p.Alive.Load()), colPink),
		chip("rate", fmt.Sprintf("%.0f pps", rate(p.Sent.Load(), time.Since(a.runStart))), colBlue))

	return []string{
		"",
		head,
		"   " + colGray.paint(p.Label()),
		meter,
		stats,
		"   " + colDark.paint("press [x] to stop"),
		hrule(w, colDark),
	}
}

func rate(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / d.Seconds()
}

func (a *app) renderPrompt(w int) []string {
	line := fmt.Sprintf(" %s %s%s",
		colYellow.paint(prompt.label+":"),
		colWhite.paint(string(prompt.buf)),
		colMagenta.paint("▏"))
	return []string{line, hrule(w, colDark)}
}

// --- bodies ----------------------------------------------------------------

func (a *app) renderBody(w, h int) []string {
	switch a.view {
	case viewRoutes:
		return a.renderRoutes(w, h)
	case viewTargets:
		return a.renderTargets(w, h)
	case viewSnaps:
		return a.renderSnaps(w, h)
	case viewDiff:
		return a.renderDiff(w, h)
	case viewDetail:
		return a.renderDetail(w, h)
	case viewHelp:
		return a.renderHelp(w, h)
	}
	return a.renderHome(w, h)
}

// logoFont is a 5-row block font covering the letters in "ROUTE PROBE".
var logoFont = map[rune][5]string{
	'R': {"███ ", "█  █", "███ ", "█ █ ", "█  █"},
	'O': {" ██ ", "█  █", "█  █", "█  █", " ██ "},
	'U': {"█  █", "█  █", "█  █", "█  █", " ██ "},
	'T': {"████", " ██ ", " ██ ", " ██ ", " ██ "},
	'E': {"████", "█   ", "███ ", "█   ", "████"},
	'P': {"███ ", "█  █", "███ ", "█   ", "█   "},
	'B': {"███ ", "█  █", "███ ", "█  █", "███ "},
	' ': {"  ", "  ", "  ", "  ", "  "},
}

// renderLogo draws the wordmark with a horizontal color sweep.
func renderLogo(text string, w int) []string {
	rows := make([]strings.Builder, 5)
	width := 0
	for _, r := range text {
		g, ok := logoFont[r]
		if !ok {
			continue
		}
		width += len(g[0]) + 1
	}
	if width+2 > w {
		return nil
	}
	col := 0
	for _, r := range text {
		g, ok := logoFont[r]
		if !ok {
			continue
		}
		for i := range 5 {
			for j, ch := range g[i] {
				c := gradient(float64(col+j) / float64(width))
				if ch == ' ' {
					rows[i].WriteString(" ")
					continue
				}
				rows[i].WriteString(c.fg())
				rows[i].WriteRune(ch)
			}
			rows[i].WriteString(" ")
		}
		col += len(g[0]) + 1
	}
	out := make([]string, 5)
	for i := range rows {
		out[i] = "  " + rows[i].String() + sgrReset
	}
	return out
}

func (a *app) renderHome(w, h int) []string {
	var out []string
	if h >= 18 {
		if logo := renderLogo("ROUTE PROBE", w); logo != nil {
			out = append(out, logo...)
			out = append(out, "  "+colGray.paint("before / after routing diffs for your tailnet"), "")
		}
	}

	step := func(n int, key, name, status string, c rgb) string {
		k := "    "
		if key != "" {
			k = colYellow.paint(" [" + key + "]")
		}
		return fmt.Sprintf("  %s %s %s %s",
			colPurple.paintf("%d.", n),
			k,
			padRight(sgrBold+colWhite.fg()+name+sgrReset, 22),
			c.paint(status))
	}

	// Step 1: discovery.
	s1, c1 := "not run yet — sweeps every routed prefix for hosts that answer ICMP", colDark
	if a.targets != nil {
		s1 = fmt.Sprintf("✔ %d live targets across %d/%d prefixes, %s",
			a.targets.TotalTargets(), a.targets.Covered(), len(a.targets.Prefixes), ago(a.targets.DiscoveredAt))
		c1 = colGreen
	}
	out = append(out, step(1, "d", "discover targets", s1, c1))

	// Steps 2 and 4: the two captures.
	out = append(out, step(2, "b", "capture BEFORE", a.snapStatus(a.beforeNm), a.snapColor(a.beforeNm)))
	out = append(out, step(3, "", "make your change", "← switch the route, flip the subnet router, whatever it is", colPink))
	out = append(out, step(4, "a", "capture AFTER", a.snapStatus(a.afterNm), a.snapColor(a.afterNm)))

	// Step 5: compare.
	s5, c5 := "needs both snapshots", colDark
	if a.diff != nil {
		s5, c5 = a.diff.Headline(), colCyan
		if a.diff.Regressions() > 0 {
			c5 = colRed
		}
	} else if a.hasSnap(a.beforeNm) && a.hasSnap(a.afterNm) {
		s5, c5 = "ready — press [c]", colYellow
	}
	out = append(out, step(5, "c", "compare", s5, c5))

	out = append(out, "", "  "+colDark.paint("── activity ")+hrule(max(0, w-16), colDark))
	remaining := h - len(out)
	if remaining > 0 {
		logs := a.logLines
		if len(logs) > remaining {
			logs = logs[len(logs)-remaining:]
		}
		for _, l := range logs {
			out = append(out, "  "+colGray.paint(l))
		}
	}
	return out
}

func (a *app) hasSnap(name string) bool {
	for _, s := range a.snaps {
		if s.Name == name {
			return true
		}
	}
	return false
}

func (a *app) snapStatus(name string) string {
	for _, s := range a.snaps {
		if s.Name == name {
			return fmt.Sprintf("✔ %d/%d prefixes reachable, %s", s.Up, s.Prefixes, ago(s.TakenAt))
		}
	}
	return "not captured"
}

func (a *app) snapColor(name string) rgb {
	if a.hasSnap(name) {
		return colGreen
	}
	return colDark
}

func (a *app) renderRoutes(w, h int) []string {
	if len(a.routes) == 0 {
		return []string{"", "  " + colGray.paint("no routes — is tailscaled running? press [R] to retry")}
	}
	head := "  " + colGray.paint(padRight("PREFIX", 22)+padRight("NEXT HOP", 18)+padRight("PEER", 32)+"STATE")
	rows := make([]string, 0, len(a.routes))
	for _, r := range a.routes {
		state := colGreen.paint("online")
		if !r.Online {
			state = colRed.paint("offline")
		}
		pfxColor := colCyan
		if r.Prefix.Bits() == 0 {
			pfxColor = colMagenta // default route: an exit node
		}
		rows = append(rows, fmt.Sprintf("  %s%s%s%s",
			cell(pfxColor.paint(r.Prefix.String()), 22),
			cell(colBlue.paint(r.NextHop.String()), 18),
			cell(colWhite.paint(r.ShortName()), 32),
			state))
	}
	return a.listBody(head, rows, w, h)
}

func (a *app) renderTargets(w, h int) []string {
	if a.targets == nil {
		return []string{"", "  " + colGray.paint("no discovery yet — press [d]")}
	}
	head := "  " + colGray.paint(padRight("PREFIX", 22)+padRight("SWEPT", 8)+padRight("LIVE", 6)+padRight("TARGETS", 44)+"VIA")
	rows := make([]string, 0, len(a.targets.Prefixes))
	for _, p := range a.targets.Prefixes {
		var addrs []string
		for _, x := range p.Alive {
			addrs = append(addrs, x.String())
		}
		live := colDark.paint("0")
		if len(p.Alive) > 0 {
			live = colGreen.paintf("%d", len(p.Alive))
		}
		list := colDark.paint("(no responders)")
		if len(addrs) > 0 {
			list = colWhite.paint(strings.Join(addrs, " "))
		}
		rows = append(rows, fmt.Sprintf("  %s%s%s%s%s",
			cell(colCyan.paint(p.Prefix.String()), 22),
			cell(colGray.paintf("%d", p.Probed), 8),
			cell(live, 6),
			cell(list, 44),
			colPurple.paint(truncate(joinOrNone(p.NextHops), 30))))
	}
	return a.listBody(head, rows, w, h)
}

func (a *app) renderSnaps(w, h int) []string {
	if len(a.snaps) == 0 {
		return []string{"", "  " + colGray.paint("no snapshots yet — press [b] to capture a 'before'")}
	}
	head := "  " + colGray.paint(padRight("", 4)+padRight("NAME", 22)+padRight("TAKEN", 28)+padRight("REACHABLE", 12)+"NOTE")
	rows := make([]string, 0, len(a.snaps))
	for _, s := range a.snaps {
		mark := "  "
		switch s.Name {
		case a.beforeNm:
			mark = colYellow.paint("①")
		case a.afterNm:
			mark = colYellow.paint("②")
		}
		if s.Name == a.beforeNm && s.Name == a.afterNm {
			mark = colRed.paint("!!")
		}
		frac := 0.0
		if s.Prefixes > 0 {
			frac = float64(s.Up) / float64(s.Prefixes)
		}
		c := colGreen
		if frac < 0.9 {
			c = colYellow
		}
		if frac < 0.5 {
			c = colRed
		}
		rows = append(rows, fmt.Sprintf("  %s%s%s%s%s",
			padRight(mark, 4),
			cell(colWhite.paint(s.Name), 22),
			cell(colGray.paint(s.TakenAt.Format("Jan 2 15:04:05")+" ("+ago(s.TakenAt)+")"), 28),
			cell(c.paintf("%d/%d", s.Up, s.Prefixes), 12),
			colDark.paint(s.Note)))
	}
	return a.listBody(head, rows, w, h)
}

func (a *app) renderDiff(w, h int) []string {
	if a.diff == nil {
		return []string{"", "  " + colGray.paint("no comparison yet — capture [b]efore and [a]fter, then press [c]")}
	}
	d := a.diff
	var out []string

	verdictColor := colGreen
	if d.Regressions() > 0 {
		verdictColor = colRed
	} else if d.Improvements() > 0 {
		verdictColor = colCyan
	}
	out = append(out, "  "+sgrBold+verdictColor.fg()+d.Headline()+sgrReset)
	out = append(out, "  "+colGray.paintf("%s (%s) → %s (%s)   ·   %d prefixes measured   ·   %d route table change(s)",
		d.Before.Name, d.Before.TakenAt.Format("15:04:05"),
		d.After.Name, d.After.TakenAt.Format("15:04:05"),
		len(d.Prefixes), len(d.RouteChanges)))

	// Verdict tally.
	var chips []string
	for v := range numVerdicts {
		if n := d.Counts[v]; n > 0 {
			vd := Verdict(v)
			chips = append(chips, vd.Color().paintf("%s %s %d", vd.Glyph(), vd.Label(), n))
		}
	}
	out = append(out, "  "+truncate(strings.Join(chips, colDark.paint(" │ ")), w-4), "")

	rows := a.visibleDiffRows()
	if len(rows) == 0 {
		out = append(out, "  "+colGreen.paint("nothing matches the current filter — press [f] to widen it"))
		return out
	}

	head := "  " + colGray.paint(padRight("", 12)+padRight("PREFIX", 21)+padRight("REACHABLE", 15)+padRight("LATENCY", 22)+"PATH")
	body := make([]string, 0, len(rows))
	for _, p := range rows {
		body = append(body, "  "+diffRow(p, w-2))
	}
	return append(out, a.listBody(head, body, w, h-len(out))...)
}

// diffRow is one line of the comparison table.
func diffRow(p PrefixDiff, w int) string {
	v := p.Verdict
	verdict := v.Color().paintf("%s %s", v.Glyph(), padRight(v.Label(), 9))

	reach := fmt.Sprintf("%s→%s",
		countStr(p.AliveBefore, p.TotalBefore, p.UpBefore),
		countStr(p.AliveAfter, p.TotalAfter, p.UpAfter))

	lat := colDark.paint("—")
	if p.RTTBefore > 0 || p.RTTAfter > 0 {
		delta := ""
		if p.RTTBefore > 0 && p.RTTAfter > 0 && absF(p.RTTDeltaPct) >= 1 {
			c := colGray
			if p.RTTDeltaPct > 0 {
				c = colOrange
			} else {
				c = colCyan
			}
			delta = c.paintf(" %+.0f%%", p.RTTDeltaPct)
		}
		lat = colWhite.paint(shortDur(p.RTTBefore)) + colDark.paint("→") +
			colWhite.paint(shortDur(p.RTTAfter)) + delta
	}

	path := colDark.paint("unchanged")
	if p.PathChanged() {
		path = colYellow.paint(shortHops(p.NextHopsBefore)) + colDark.paint(" ⇢ ") +
			colPink.paint(shortHops(p.NextHopsAfter))
	}

	return truncate(fmt.Sprintf("%s%s%s%s%s",
		cell(verdict, 12),
		cell(colCyan.paint(p.Prefix.String()), 21),
		cell(reach, 15),
		cell(lat, 22),
		path), w)
}

func countStr(alive, total int, up bool) string {
	if total == 0 {
		return colDark.paint("—")
	}
	c := colRed
	if up {
		c = colGreen
		if alive < total {
			c = colYellow
		}
	}
	return c.paintf("%d/%d", alive, total)
}

// shortHops renders a next-hop set compactly: the peer names without their
// tailnet addresses, since the addresses don't fit in a table column.
func shortHops(hops []string) string {
	if len(hops) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(hops))
	for _, h := range hops {
		if i := strings.Index(h, " ("); i > 0 {
			h = h[:i]
		}
		names = append(names, h)
	}
	s := strings.Join(names, ",")
	if len(s) > 34 {
		s = fmt.Sprintf("%s +%d more", names[0], len(names)-1)
	}
	return s
}

func (a *app) renderDetail(w, h int) []string {
	p, ok := a.selectedDiff()
	if !ok {
		return []string{"", "  " + colGray.paint("nothing selected")}
	}
	v := p.Verdict
	out := []string{
		"",
		"  " + sgrBold + colCyan.fg() + p.Prefix.String() + sgrReset + "   " +
			v.Color().paintf("%s %s", v.Glyph(), v.Label()),
		"",
	}
	for _, n := range p.Notes {
		out = append(out, "    "+colWhite.paint("• "+n))
	}
	if len(p.Notes) == 0 {
		out = append(out, "    "+colGray.paint("• no meaningful change"))
	}

	out = append(out, "", "  "+colGray.paint("PATH"))
	out = append(out, "    before  "+colYellow.paint(joinOrNone(p.NextHopsBefore)))
	out = append(out, "    after   "+colPink.paint(joinOrNone(p.NextHopsAfter)))

	out = append(out, "", "  "+colGray.paint("MEASUREMENTS"))
	out = append(out, fmt.Sprintf("    reachable  %s   →   %s",
		countStr(p.AliveBefore, p.TotalBefore, p.UpBefore),
		countStr(p.AliveAfter, p.TotalAfter, p.UpAfter)))
	out = append(out, fmt.Sprintf("    latency    %s   →   %s   %s",
		colWhite.paint(shortDur(p.RTTBefore)),
		colWhite.paint(shortDur(p.RTTAfter)),
		deltaText(p.RTTDeltaPct)))
	out = append(out, fmt.Sprintf("    loss       %s   →   %s",
		lossText(p.LossBefore), lossText(p.LossAfter)))
	if p.UnreachAfter > 0 {
		out = append(out, "    "+colOrange.paintf("icmp       %d unreachable/TTL-exceeded replies after the change", p.UnreachAfter))
	}

	// Per-target breakdown from the after snapshot, which is what you'd want
	// to hand to whoever owns the subnet.
	if a.diff != nil {
		bp := a.diff.Before.byPrefix()
		ap := a.diff.After.byPrefix()
		out = append(out, "", "  "+colGray.paint("PER-TARGET   "+padRight("ADDRESS", 20)+padRight("BEFORE", 22)+"AFTER"))
		b, a2 := bp[p.Prefix], ap[p.Prefix]
		addrs := map[string]bool{}
		var order []string
		for _, s := range append(append([]Stat{}, b.Targets...), a2.Targets...) {
			if !addrs[s.Addr.String()] {
				addrs[s.Addr.String()] = true
				order = append(order, s.Addr.String())
			}
		}
		for _, addr := range order {
			out = append(out, fmt.Sprintf("               %s%s%s",
				cell(colWhite.paint(addr), 20),
				cell(statText(findStat(b.Targets, addr)), 22),
				statText(findStat(a2.Targets, addr))))
		}
	}
	_ = w
	if len(out) > h {
		out = out[:h]
	}
	return out
}

func findStat(stats []Stat, addr string) *Stat {
	for i := range stats {
		if stats[i].Addr.String() == addr {
			return &stats[i]
		}
	}
	return nil
}

func statText(s *Stat) string {
	if s == nil {
		return colDark.paint("not measured")
	}
	if !s.Alive() {
		if s.Unreach > 0 {
			return colRed.paintf("unreachable (%d icmp err)", s.Unreach)
		}
		return colRed.paintf("no reply (%d sent)", s.Sent)
	}
	return fmt.Sprintf("%s %s", colGreen.paint(shortDur(s.AvgRTT())),
		colGray.paintf("%d/%d", s.Recv, s.Sent))
}

func deltaText(pct float64) string {
	if pct == 0 {
		return ""
	}
	c := colCyan
	if pct > 0 {
		c = colOrange
	}
	return c.paintf("(%+.0f%%)", pct)
}

func lossText(pct float64) string {
	switch {
	case pct == 0:
		return colGreen.paint("0%")
	case pct < 20:
		return colYellow.paintf("%.0f%%", pct)
	}
	return colRed.paintf("%.0f%%", pct)
}

func (a *app) helpLines() []string {
	sec := func(s string) string { return "  " + sgrBold + colMagenta.fg() + s + sgrReset }
	row := func(k, d string) string {
		return "    " + padRight(colYellow.paint(k), 20) + colGray.paint(d)
	}
	out := []string{
		"",
		sec("THE WORKFLOW"),
		"    " + colGray.paint("discover once, capture before, make your change, capture after, compare."),
		"    " + colGray.paint("both captures probe the same fixed target list, which is what makes"),
		"    " + colGray.paint("the comparison meaningful."),
		"",
		sec("ACTIONS"),
		row("d", "discover — sweep every routed prefix for hosts that answer ICMP"),
		row("b", "capture the 'before' snapshot"),
		row("a", "capture the 'after' snapshot"),
		row("n", "capture under a custom name"),
		row("c", "compare before → after"),
		row("w", "write the comparison to a text report"),
		row("x", "stop the running sweep"),
		row("R", "reload routes and snapshots from disk"),
		"",
		sec("VIEWS"),
		row("r", "route table as tailscaled sees it"),
		row("t", "discovered targets per prefix"),
		row("s", "saved snapshots (1 = set before, 2 = set after)"),
		row("f", "cycle the diff filter: changes / regressions / all"),
		"",
		sec("NAVIGATION"),
		row("↑ ↓ / j k", "move"),
		row("PgUp PgDn / space", "page"),
		row("g / G", "top / bottom"),
		row("↵", "open detail"),
		row("esc", "back"),
		row("q", "back to dashboard, or quit from it"),
		"",
		sec("WHAT THE VERDICTS MEAN"),
	}
	for v := range numVerdicts {
		vd := Verdict(v)
		out = append(out, "    "+padRight(vd.Color().paintf("%s %s", vd.Glyph(), vd.Label()), 22)+
			colGray.paint(verdictHelp[vd]))
	}
	return out
}

// renderHelp windows the help text, which is longer than most terminals. The
// cursor doubles as the scroll offset here; there is no row to select.
func (a *app) renderHelp(w, h int) []string {
	_ = w
	lines := a.helpLines()
	if len(lines) <= h {
		return lines
	}
	// Reserve the last row for the scroll indicator rather than covering a
	// line of help with it.
	contentH := h - 1
	off := a.cursor[viewHelp]
	if maxOff := len(lines) - contentH; off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	end := min(len(lines), off+contentH)
	out := append([]string(nil), lines[off:end]...)
	return append(out, "  "+colDark.paintf("↑↓ to scroll · %d–%d of %d", off+1, end, len(lines)))
}

var verdictHelp = map[Verdict]string{
	VerdictBroken:     "answered before the change, silent after it — the thing you're looking for",
	VerdictWithdrawn:  "no peer advertises this prefix any more",
	VerdictLossUp:     "still reachable, but dropping meaningfully more packets",
	VerdictPathChange: "reachable via a different peer than before",
	VerdictSlower:     "latency rose past both the relative and absolute thresholds",
	VerdictGone:       "measured before, missing from the after capture",
	VerdictNew:        "appeared since the before capture",
	VerdictFixed:      "was unreachable, now answers",
	VerdictFaster:     "latency dropped past both thresholds",
	VerdictLossDown:   "dropping meaningfully fewer packets",
	VerdictStillDown:  "unreachable in both captures — probably firewalled, not broken",
	VerdictOK:         "no meaningful change",
}

// listBody renders a scrollable list with the cursor row highlighted.
func (a *app) listBody(head string, rows []string, w, h int) []string {
	if h < 2 {
		return nil
	}
	avail := h - 1 // leave a line for the header
	n := len(rows)
	cur := a.cursor[a.view]
	if cur >= n {
		cur = max(0, n-1)
		a.cursor[a.view] = cur
	}
	scroll := a.scroll[a.view]
	if cur < scroll {
		scroll = cur
	}
	if cur >= scroll+avail {
		scroll = cur - avail + 1
	}
	if scroll > n-avail {
		scroll = n - avail
	}
	if scroll < 0 {
		scroll = 0
	}
	a.scroll[a.view] = scroll

	out := []string{head}
	end := min(n, scroll+avail)
	for i := scroll; i < end; i++ {
		if i == cur {
			out = append(out, selectedRow(rows[i], w))
			continue
		}
		out = append(out, rows[i])
	}
	// A scroll indicator, so it's obvious when the list runs off the screen.
	if n > avail {
		out[0] = out[0] + colDark.paintf("   %d–%d of %d", scroll+1, end, n)
	}
	return out
}

// selectedRow repaints a row flat on the highlight background. The row's own
// color codes are stripped first: a nested reset would clear the background
// partway across and leave a ragged highlight.
func selectedRow(row string, w int) string {
	plain := stripANSI(row)
	if len(plain) > 1 {
		plain = "▸" + plain[1:]
	}
	return bgSelect.bg() + colWhite.fg() + sgrBold + padRight(truncate(plain, w), w) + sgrReset
}

func padLines(lines []string, n int) []string {
	if len(lines) > n {
		return lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}

// ago renders a timestamp as a short relative age.
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
