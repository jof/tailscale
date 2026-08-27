// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type view int

const (
	viewHome view = iota
	viewRoutes
	viewTargets
	viewSnaps
	viewDiff
	viewDetail
	viewHelp
)

// diffFilter narrows the diff list to the rows worth staring at.
type diffFilter int

const (
	filterChanges diffFilter = iota // anything that isn't "no change"
	filterRegress                   // only things that got worse
	filterAll
)

func (f diffFilter) String() string {
	switch f {
	case filterChanges:
		return "changes"
	case filterRegress:
		return "regressions"
	}
	return "all"
}

// runResult carries the outcome of a background operation back to the event
// loop, so nothing but the event loop ever mutates app state.
type runResult struct {
	kind    string
	err     error
	targets *Targets
	snap    *Snapshot
}

// input is the one-line text prompt used for naming snapshots.
type input struct {
	active bool
	label  string
	buf    []rune
	onDone func(string)
}

type app struct {
	scr  *Screen
	ping *Pinger
	ctx  context.Context

	tick int
	view view
	back view // where Esc returns to

	// Data.
	routes   []Route
	targets  *Targets
	snaps    []SnapshotInfo
	diff     *Diff
	beforeNm string
	afterNm  string

	// Per-view cursor and scroll offsets.
	cursor map[view]int
	scroll map[view]int

	// Background operation state.
	running   bool
	runKind   string
	runStart  time.Time
	prog      *Progress
	runCancel context.CancelFunc
	results   chan runResult

	filter     diffFilter
	logLines   []string
	flash      string
	flashUntil time.Time
	err        string

	quit bool
}

func newApp(ctx context.Context, scr *Screen, p *Pinger) *app {
	return &app{
		scr:      scr,
		ping:     p,
		ctx:      ctx,
		view:     viewHome,
		cursor:   map[view]int{},
		scroll:   map[view]int{},
		results:  make(chan runResult, 4),
		beforeNm: "before",
		afterNm:  "after",
		filter:   filterChanges,
	}
}

// Run is the event loop. It returns when the user quits.
func (a *app) Run() {
	a.reload()
	a.logf("welcome — press %s to discover pingable targets", "[d]")

	ticker := time.NewTicker(90 * time.Millisecond)
	defer ticker.Stop()

	a.render()
	for !a.quit {
		select {
		case k, ok := <-a.scr.Keys:
			if !ok {
				return
			}
			a.onKey(k)
		case r := <-a.results:
			a.onResult(r)
		case <-ticker.C:
			a.tick++
		case <-a.ctx.Done():
			return
		}
		a.render()
	}
}

// --- state helpers ---------------------------------------------------------

func (a *app) logf(format string, args ...any) {
	line := fmt.Sprintf("%s %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	a.logLines = append(a.logLines, line)
	if len(a.logLines) > 200 {
		a.logLines = a.logLines[len(a.logLines)-200:]
	}
}

func (a *app) flashf(format string, args ...any) {
	a.flash = fmt.Sprintf(format, args...)
	a.flashUntil = time.Now().Add(4 * time.Second)
}

func (a *app) errorf(format string, args ...any) {
	a.err = fmt.Sprintf(format, args...)
	a.logf("error: %s", a.err)
}

// reload refreshes everything we read from disk or from tailscaled.
func (a *app) reload() {
	if routes, _, err := fetchRoutes(a.ctx); err != nil {
		a.errorf("%v", err)
	} else {
		a.routes = routes
		a.err = ""
	}
	if t, err := loadTargets(); err == nil {
		a.targets = t
	}
	if s, err := listSnapshots(); err == nil {
		a.snaps = s
	}
}

func (a *app) goTo(v view) {
	a.back = a.view
	a.view = v
}

// --- background operations -------------------------------------------------

func (a *app) start(kind string, fn func(ctx context.Context, prog *Progress) runResult) {
	if a.running {
		a.flashf("%s already running — press [x] to stop it", a.runKind)
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	prog := &Progress{}
	prog.SetLabel("starting…")

	a.running = true
	a.runKind = kind
	a.runStart = time.Now()
	a.prog = prog
	a.runCancel = cancel
	a.err = ""
	a.logf("▶ %s started", kind)

	go func() {
		defer cancel()
		res := fn(ctx, prog)
		res.kind = kind
		a.results <- res
	}()
}

func (a *app) stopRun() {
	if !a.running {
		return
	}
	a.runCancel()
	a.logf("✖ %s cancelled", a.runKind)
}

func (a *app) onResult(r runResult) {
	a.running = false
	took := time.Since(a.runStart).Round(time.Millisecond)

	if r.err != nil {
		a.errorf("%s failed after %s: %v", r.kind, took, r.err)
		return
	}
	switch {
	case r.targets != nil:
		a.targets = r.targets
		if err := saveTargets(r.targets); err != nil {
			a.errorf("saving targets: %v", err)
			return
		}
		a.logf("✔ discovery done in %s — %d/%d prefixes have live targets (%d addresses)",
			took, r.targets.Covered(), len(r.targets.Prefixes), r.targets.TotalTargets())
		a.flashf("discovered %d live targets across %d prefixes",
			r.targets.TotalTargets(), r.targets.Covered())
	case r.snap != nil:
		if err := saveSnapshot(r.snap); err != nil {
			a.errorf("saving snapshot: %v", err)
			return
		}
		up := 0
		for _, p := range r.snap.Prefixes {
			if p.Up() {
				up++
			}
		}
		a.logf("✔ captured %q in %s — %d/%d prefixes reachable", r.snap.Name, took, up, len(r.snap.Prefixes))
		a.flashf("snapshot %q saved", r.snap.Name)
		a.snaps, _ = listSnapshots()
	}
	a.routes, _, _ = fetchRoutes(a.ctx)
}

func (a *app) startDiscover() {
	opts := DefaultDiscoverOptions()
	a.start("discovery", func(ctx context.Context, prog *Progress) runResult {
		t, err := Discover(ctx, a.ping, opts, prog)
		return runResult{targets: t, err: err}
	})
}

func (a *app) startCapture(name, note string) {
	if a.targets == nil || a.targets.TotalTargets() == 0 {
		a.flashf("no targets yet — press [d] to run discovery first")
		return
	}
	tg := a.targets
	opts := DefaultCaptureOptions()
	opts.Name = name
	opts.Note = note
	a.start("capture "+name, func(ctx context.Context, prog *Progress) runResult {
		s, err := Capture(ctx, a.ping, tg, opts, prog)
		return runResult{snap: s, err: err}
	})
}

func (a *app) compare() {
	before, err := loadSnapshot(a.beforeNm)
	if err != nil {
		a.flashf("%v", err)
		return
	}
	after, err := loadSnapshot(a.afterNm)
	if err != nil {
		a.flashf("%v", err)
		return
	}
	a.diff = Compare(before, after, DefaultThresholds())
	a.cursor[viewDiff], a.scroll[viewDiff] = 0, 0
	a.goTo(viewDiff)
	a.logf("compared %q → %q: %d regressions, %d improvements",
		before.Name, after.Name, a.diff.Regressions(), a.diff.Improvements())
}

// --- input -----------------------------------------------------------------

var prompt input

func (a *app) askFor(label, initial string, onDone func(string)) {
	prompt = input{active: true, label: label, buf: []rune(initial), onDone: onDone}
}

func (a *app) onKey(k Key) {
	if prompt.active {
		a.promptKey(k)
		return
	}
	if k.Type == KeyCtrlC {
		if a.running {
			a.stopRun()
			return
		}
		a.quit = true
		return
	}

	// Global keys.
	switch {
	case k.IsRune('q'):
		if a.view != viewHome {
			a.view = viewHome
			return
		}
		a.quit = true
		return
	case k.IsRune('?'), k.IsRune('h'):
		if a.view == viewHelp {
			a.view = a.back
		} else {
			a.goTo(viewHelp)
		}
		return
	case k.Type == KeyEsc:
		if a.view != viewHome {
			a.view = a.back
			if a.view == viewDetail {
				a.view = viewDiff
			}
		}
		return
	case k.IsRune('x'):
		a.stopRun()
		return
	case k.IsRune('R'):
		a.reload()
		a.flashf("reloaded route table and snapshots")
		return
	case k.IsRune('d'):
		a.startDiscover()
		return
	case k.IsRune('b'):
		a.beforeNm = "before"
		a.startCapture("before", "captured before the change")
		return
	case k.IsRune('a'):
		a.afterNm = "after"
		a.startCapture("after", "captured after the change")
		return
	case k.IsRune('n'):
		a.askFor("snapshot name", "", func(s string) {
			if s == "" {
				return
			}
			a.startCapture(s, "")
		})
		return
	case k.IsRune('c'):
		a.compare()
		return
	case k.IsRune('r'):
		a.goTo(viewRoutes)
		return
	case k.IsRune('t'):
		a.goTo(viewTargets)
		return
	case k.IsRune('s'):
		a.snaps, _ = listSnapshots()
		a.goTo(viewSnaps)
		return
	case k.IsRune('w'):
		a.exportDiff()
		return
	}

	// View-local keys.
	switch a.view {
	case viewDiff:
		a.diffKey(k)
	case viewSnaps:
		a.snapsKey(k)
	}
	a.moveCursor(k)
}

func (a *app) promptKey(k Key) {
	switch k.Type {
	case KeyEnter:
		prompt.active = false
		if prompt.onDone != nil {
			prompt.onDone(strings.TrimSpace(string(prompt.buf)))
		}
	case KeyEsc, KeyCtrlC:
		prompt.active = false
	case KeyBackspace:
		if n := len(prompt.buf); n > 0 {
			prompt.buf = prompt.buf[:n-1]
		}
	case KeyRune:
		if len(prompt.buf) < 48 {
			prompt.buf = append(prompt.buf, k.Rune)
		}
	}
}

func (a *app) diffKey(k Key) {
	switch {
	case k.IsRune('f'):
		a.filter = (a.filter + 1) % 3
		a.cursor[viewDiff], a.scroll[viewDiff] = 0, 0
		a.flashf("filter: %s", a.filter)
	case k.Type == KeyEnter:
		if len(a.visibleDiffRows()) > 0 {
			a.goTo(viewDetail)
		}
	}
}

func (a *app) snapsKey(k Key) {
	if len(a.snaps) == 0 {
		return
	}
	i := a.cursor[viewSnaps]
	if i < 0 || i >= len(a.snaps) {
		return
	}
	switch {
	case k.Type == KeyEnter:
		a.compare()
	case k.IsRune('1'):
		a.beforeNm = a.snaps[i].Name
		a.flashf("before = %q", a.beforeNm)
	case k.IsRune('2'):
		a.afterNm = a.snaps[i].Name
		a.flashf("after = %q", a.afterNm)
	}
}

// moveCursor applies list navigation to whichever view is showing a list.
func (a *app) moveCursor(k Key) {
	n := a.listLen(a.view)
	if n == 0 {
		return
	}
	page := a.bodyHeight() - 2
	if page < 1 {
		page = 1
	}
	c := a.cursor[a.view]
	switch {
	case k.Type == KeyUp, k.IsRune('k'):
		c--
	case k.Type == KeyDown, k.IsRune('j'):
		c++
	case k.Type == KeyPgUp:
		c -= page
	case k.Type == KeyPgDn, k.IsRune(' '):
		c += page
	case k.Type == KeyHome, k.IsRune('g'):
		c = 0
	case k.Type == KeyEnd, k.IsRune('G'):
		c = n - 1
	default:
		return
	}
	if c < 0 {
		c = 0
	}
	if c >= n {
		c = n - 1
	}
	a.cursor[a.view] = c
}

func (a *app) listLen(v view) int {
	switch v {
	case viewRoutes:
		return len(a.routes)
	case viewTargets:
		if a.targets == nil {
			return 0
		}
		return len(a.targets.Prefixes)
	case viewSnaps:
		return len(a.snaps)
	case viewDiff:
		return len(a.visibleDiffRows())
	case viewHelp:
		// The help view has no selectable rows; the cursor is its scroll
		// offset, so its "length" is the number of scroll positions.
		if n := len(a.helpLines()) - (a.bodyHeight() - 1) + 1; n > 0 {
			return n
		}
		return 0
	}
	return 0
}

// visibleDiffRows applies the current filter to the diff.
func (a *app) visibleDiffRows() []PrefixDiff {
	if a.diff == nil {
		return nil
	}
	var out []PrefixDiff
	for _, p := range a.diff.Prefixes {
		switch a.filter {
		case filterAll:
		case filterChanges:
			if p.Verdict == VerdictOK || p.Verdict == VerdictStillDown {
				continue
			}
		case filterRegress:
			if !p.Verdict.Regression() {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// selectedDiff is the row the cursor is on, if any.
func (a *app) selectedDiff() (PrefixDiff, bool) {
	rows := a.visibleDiffRows()
	i := a.cursor[viewDiff]
	if i < 0 || i >= len(rows) {
		return PrefixDiff{}, false
	}
	return rows[i], true
}

// exportDiff writes the current comparison to a plain-text file next to the
// snapshots, so it can be pasted into a ticket or a change record.
func (a *app) exportDiff() {
	if a.diff == nil {
		a.flashf("nothing to export — press [c] to compare first")
		return
	}
	path, err := writeDiffReport(a.diff)
	if err != nil {
		a.errorf("export failed: %v", err)
		return
	}
	a.logf("wrote report to %s", path)
	a.flashf("wrote %s", path)
}
