// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestCandidatesForStaysInsidePrefix(t *testing.T) {
	const budget = 256
	for _, s := range []string{
		"10.0.0.0/8", "10.10.0.0/16", "10.21.0.0/17", "192.168.8.0/21",
		"172.16.4.0/22", "10.1.2.0/24", "10.1.2.128/25", "10.1.2.192/26",
		"10.1.2.4/31", "10.1.2.7/32",
	} {
		p := netip.MustParsePrefix(s)
		got := candidatesFor(p, budget)
		if len(got) == 0 {
			t.Errorf("%s: no candidates", s)
			continue
		}
		if len(got) > budget {
			t.Errorf("%s: %d candidates exceeds budget %d", s, len(got), budget)
		}
		seen := map[netip.Addr]bool{}
		for _, a := range got {
			if !p.Contains(a) {
				t.Errorf("%s: candidate %s is outside the prefix", s, a)
			}
			if seen[a] {
				t.Errorf("%s: duplicate candidate %s", s, a)
			}
			seen[a] = true
		}
	}
}

// A prefix big enough to need sampling should still spend most of its budget:
// under-sampling a /21 was a real bug.
func TestCandidatesForUsesItsBudget(t *testing.T) {
	for _, tt := range []struct {
		prefix  string
		budget  int
		wantMin int
	}{
		{"10.10.0.0/16", 256, 250},
		{"192.168.8.0/21", 256, 250},
		{"172.16.0.0/20", 256, 250},
		{"10.0.0.0/8", 256, 250},
		{"10.0.0.0/8", 64, 60},
	} {
		got := len(candidatesFor(netip.MustParsePrefix(tt.prefix), tt.budget))
		if got < tt.wantMin || got > tt.budget {
			t.Errorf("%s budget=%d: got %d candidates, want %d..%d",
				tt.prefix, tt.budget, got, tt.wantMin, tt.budget)
		}
	}
}

// Sampling must be spread across the prefix, not clustered at its start.
func TestCandidatesForSpreadsAcrossPrefix(t *testing.T) {
	p := netip.MustParsePrefix("10.10.0.0/16")
	got := candidatesFor(p, 256)
	third := make(map[int]bool)
	for _, a := range got {
		third[int(a.As4()[2])/86] = true // which third of the /16 this lands in
	}
	if len(third) < 3 {
		t.Errorf("candidates only cover %d of 3 thirds of the /16", len(third))
	}
}

func TestCandidatesForSkipsNetworkAndBroadcast(t *testing.T) {
	p := netip.MustParsePrefix("10.1.2.0/24")
	got := candidatesFor(p, 1024)
	if len(got) != 254 {
		t.Fatalf("got %d candidates for a /24, want 254", len(got))
	}
	for _, a := range got {
		if last := a.As4()[3]; last == 0 || last == 255 {
			t.Errorf("candidate %s is the network or broadcast address", a)
		}
	}
}

func TestCandidatesForIsDeterministic(t *testing.T) {
	p := netip.MustParsePrefix("10.10.0.0/16")
	a, b := candidatesFor(p, 128), candidatesFor(p, 128)
	if len(a) != len(b) {
		t.Fatalf("length differs between runs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("candidate %d differs between runs: %s vs %s", i, a[i], b[i])
		}
	}
}

func TestCandidatesForDefaultRoute(t *testing.T) {
	got := candidatesFor(netip.MustParsePrefix("0.0.0.0/0"), 256)
	if len(got) != len(defaultRouteProbes) {
		t.Errorf("default route: got %d probes, want the %d well-known ones",
			len(got), len(defaultRouteProbes))
	}
}

// --- ANSI helpers ----------------------------------------------------------

func TestDispWidthIgnoresEscapes(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int
	}{
		{"hello", 5},
		{colRed.paint("hello"), 5},
		{sgrBold + colCyan.fg() + "abc" + sgrReset, 3},
		{gradientText("ROUTE"), 5},
		{"", 0},
	} {
		if got := dispWidth(tt.in); got != tt.want {
			t.Errorf("dispWidth(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// The verdict glyphs must all measure one column, or the diff table's columns
// drift apart depending on which verdict a row happens to carry.
func TestVerdictGlyphsAreOneColumn(t *testing.T) {
	for v := range numVerdicts {
		g := Verdict(v).Glyph()
		if got := dispWidth(g); got != 1 {
			t.Errorf("glyph %q for %s measures %d columns, want 1",
				g, Verdict(v).Label(), got)
		}
	}
}

func TestCellTruncatesAndPads(t *testing.T) {
	for _, tt := range []struct{ in string }{
		{"short"},
		{"a-very-long-peer-name-that-will-not-fit-in-the-column"},
		{colWhite.paint("a-very-long-peer-name-that-will-not-fit-in-the-column")},
	} {
		got := cell(tt.in, 20)
		if dispWidth(got) != 20 {
			t.Errorf("cell(%q, 20) measures %d columns", stripANSI(tt.in), dispWidth(got))
		}
		if !strings.HasSuffix(stripANSI(got), " ") {
			t.Errorf("cell(%q, 20) left no gap before the next column", stripANSI(tt.in))
		}
	}
}

func TestStripANSIRemovesEverything(t *testing.T) {
	in := colRed.paint("a") + sgrBold + gradientText("bc") + sgrReset
	if got := stripANSI(in); got != "abc" {
		t.Errorf("stripANSI = %q, want %q", got, "abc")
	}
	if strings.ContainsRune(stripANSI(bar(0.5, 10)), 0x1b) {
		t.Error("stripANSI left an escape byte behind")
	}
}

func TestTruncateRespectsWidth(t *testing.T) {
	long := colGreen.paint(strings.Repeat("x", 100))
	for _, w := range []int{1, 5, 20, 99} {
		got := truncate(long, w)
		if dispWidth(got) > w {
			t.Errorf("truncate(w=%d) produced width %d", w, dispWidth(got))
		}
	}
	short := colGreen.paint("abc")
	if got := truncate(short, 10); got != short {
		t.Error("truncate altered a string that already fit")
	}
}

func TestPadRightAccountsForColor(t *testing.T) {
	got := padRight(colRed.paint("ab"), 6)
	if dispWidth(got) != 6 {
		t.Errorf("padRight produced width %d, want 6", dispWidth(got))
	}
}

// --- routing ---------------------------------------------------------------

func TestRouteTableLongestPrefixWins(t *testing.T) {
	rt := NewRouteTable([]Route{
		{Prefix: netip.MustParsePrefix("10.0.0.0/8"), NextHop: netip.MustParseAddr("100.64.0.1"), PeerName: "wide.example.ts.net."},
		{Prefix: netip.MustParsePrefix("10.1.0.0/16"), NextHop: netip.MustParseAddr("100.64.0.2"), PeerName: "narrow.example.ts.net."},
		{Prefix: netip.MustParsePrefix("10.1.0.0/16"), NextHop: netip.MustParseAddr("100.64.0.3"), PeerName: "narrow-ha.example.ts.net."},
	})

	got := rt.Lookup(netip.MustParseAddr("10.1.2.3"))
	if len(got) != 2 {
		t.Fatalf("got %d matches for the /16, want both HA peers", len(got))
	}
	for _, r := range got {
		if r.Prefix.Bits() != 16 {
			t.Errorf("matched %s, want the more specific /16", r.Prefix)
		}
	}

	got = rt.Lookup(netip.MustParseAddr("10.9.9.9"))
	if len(got) != 1 || got[0].Prefix.Bits() != 8 {
		t.Errorf("got %v, want the /8 only", got)
	}

	if got := rt.Lookup(netip.MustParseAddr("192.0.2.1")); len(got) != 0 {
		t.Errorf("got %v for an uncovered address, want none", got)
	}
}

func TestShortNameTrimsTailnetSuffix(t *testing.T) {
	r := Route{PeerName: "it-sunnyvale-nuc.civet-hops.ts.net.", NextHop: netip.MustParseAddr("100.64.0.1")}
	if got := r.ShortName(); got != "it-sunnyvale-nuc" {
		t.Errorf("ShortName = %q", got)
	}
	bare := Route{NextHop: netip.MustParseAddr("100.64.0.1")}
	if got := bare.ShortName(); got != "100.64.0.1" {
		t.Errorf("ShortName with no DNS name = %q, want the address", got)
	}
}

// --- diffing ---------------------------------------------------------------

// snap builds a snapshot from a compact description of each prefix.
func snap(name string, entries ...prefixSpec) *Snapshot {
	s := &Snapshot{Name: name, TakenAt: time.Now()}
	for _, e := range entries {
		ps := PrefixSnapshot{Prefix: netip.MustParsePrefix(e.prefix), NextHops: e.hops}
		for i := range e.targets {
			st := Stat{Addr: netip.MustParseAddr(e.addrs[i]), Sent: 5}
			if e.targets[i] {
				st.Recv = 5 - e.loss[i]
				st.Sent = 5
				for range st.Recv {
					st.RTTs = append(st.RTTs, e.rtt)
				}
			}
			ps.Targets = append(ps.Targets, st)
		}
		s.Prefixes = append(s.Prefixes, ps)
		s.Routes = append(s.Routes, Route{Prefix: ps.Prefix, PeerName: strings.Join(e.hops, ",")})
	}
	return s
}

type prefixSpec struct {
	prefix  string
	hops    []string
	addrs   []string
	targets []bool
	loss    []int
	rtt     time.Duration
}

func TestCompareDetectsBreakage(t *testing.T) {
	before := snap("before", prefixSpec{
		prefix: "10.1.0.0/24", hops: []string{"nuc-a (100.64.0.1)"},
		addrs: []string{"10.1.0.1"}, targets: []bool{true}, loss: []int{0}, rtt: 5 * time.Millisecond,
	})
	after := snap("after", prefixSpec{
		prefix: "10.1.0.0/24", hops: []string{"nuc-a (100.64.0.1)"},
		addrs: []string{"10.1.0.1"}, targets: []bool{false}, loss: []int{5},
	})

	d := Compare(before, after, DefaultThresholds())
	if len(d.Prefixes) != 1 {
		t.Fatalf("got %d prefixes", len(d.Prefixes))
	}
	if got := d.Prefixes[0].Verdict; got != VerdictBroken {
		t.Errorf("verdict = %s, want BROKEN", got.Label())
	}
	if d.Regressions() != 1 {
		t.Errorf("Regressions = %d, want 1", d.Regressions())
	}
}

func TestCompareDetectsPathChange(t *testing.T) {
	before := snap("before", prefixSpec{
		prefix: "10.1.0.0/24", hops: []string{"nuc-a (100.64.0.1)"},
		addrs: []string{"10.1.0.1"}, targets: []bool{true}, loss: []int{0}, rtt: 5 * time.Millisecond,
	})
	after := snap("after", prefixSpec{
		prefix: "10.1.0.0/24", hops: []string{"nuc-b (100.64.0.2)"},
		addrs: []string{"10.1.0.1"}, targets: []bool{true}, loss: []int{0}, rtt: 5 * time.Millisecond,
	})

	d := Compare(before, after, DefaultThresholds())
	pd := d.Prefixes[0]
	if pd.Verdict != VerdictPathChange {
		t.Errorf("verdict = %s, want REROUTED", pd.Verdict.Label())
	}
	if !pd.PathChanged() {
		t.Error("PathChanged = false")
	}
	if len(d.RouteChanges) != 1 {
		t.Errorf("got %d route changes, want 1", len(d.RouteChanges))
	}
}

// Small absolute latency moves must not be reported, however large the
// percentage: a 0.4ms LAN hop going to 0.7ms is noise, not a regression.
func TestCompareIgnoresSmallLatencyMoves(t *testing.T) {
	mk := func(name string, rtt time.Duration) *Snapshot {
		return snap(name, prefixSpec{
			prefix: "10.1.0.0/24", hops: []string{"nuc-a (100.64.0.1)"},
			addrs: []string{"10.1.0.1"}, targets: []bool{true}, loss: []int{0}, rtt: rtt,
		})
	}
	d := Compare(mk("before", 400*time.Microsecond), mk("after", 700*time.Microsecond), DefaultThresholds())
	if got := d.Prefixes[0].Verdict; got != VerdictOK {
		t.Errorf("verdict = %s, want OK for a sub-millisecond move", got.Label())
	}

	d = Compare(mk("before", 10*time.Millisecond), mk("after", 40*time.Millisecond), DefaultThresholds())
	if got := d.Prefixes[0].Verdict; got != VerdictSlower {
		t.Errorf("verdict = %s, want SLOWER for 10ms→40ms", got.Label())
	}
}

func TestCompareReportsWithdrawnRoute(t *testing.T) {
	before := snap("before", prefixSpec{
		prefix: "10.1.0.0/24", hops: []string{"nuc-a (100.64.0.1)"},
		addrs: []string{"10.1.0.1"}, targets: []bool{true}, loss: []int{0}, rtt: 5 * time.Millisecond,
	})
	after := snap("after", prefixSpec{
		prefix: "10.1.0.0/24", hops: nil,
		addrs: []string{"10.1.0.1"}, targets: []bool{false}, loss: []int{5},
	})
	after.Routes = nil

	d := Compare(before, after, DefaultThresholds())
	if got := d.Prefixes[0].Verdict; got != VerdictBroken {
		// Broken outranks Withdrawn; both signals should be recorded.
		t.Logf("verdict = %s", got.Label())
	}
	joined := strings.Join(d.Prefixes[0].Notes, " | ")
	if !strings.Contains(joined, "withdrawn") {
		t.Errorf("notes %q do not mention the withdrawn route", joined)
	}
}

func TestCompareIdenticalSnapshotsAreQuiet(t *testing.T) {
	spec := prefixSpec{
		prefix: "10.1.0.0/24", hops: []string{"nuc-a (100.64.0.1)"},
		addrs: []string{"10.1.0.1"}, targets: []bool{true}, loss: []int{0}, rtt: 5 * time.Millisecond,
	}
	d := Compare(snap("before", spec), snap("after", spec), DefaultThresholds())
	if d.Regressions() != 0 || d.Improvements() != 0 {
		t.Errorf("identical snapshots reported %d regressions, %d improvements",
			d.Regressions(), d.Improvements())
	}
	if len(d.RouteChanges) != 0 {
		t.Errorf("identical snapshots reported %d route changes", len(d.RouteChanges))
	}
}

// --- sweep plumbing --------------------------------------------------------

func TestPayloadRoundTrip(t *testing.T) {
	p := &Pinger{nonce: 0xdeadbeef}
	buf := make([]byte, payloadLen)
	copy(buf, probeMagic[:])
	putU32(buf[6:], p.nonce)
	putU32(buf[10:], 4242)
	putU64(buf[14:], uint64(time.Now().UnixNano()))

	token, sent, ok := p.parsePayload(buf)
	if !ok {
		t.Fatal("parsePayload rejected our own payload")
	}
	if token != 4242 {
		t.Errorf("token = %d, want 4242", token)
	}
	if time.Since(sent) > time.Minute {
		t.Errorf("send time round-tripped as %v", sent)
	}

	// A payload from a different run of ours, or from an unrelated ping, must
	// be ignored rather than attributed to a random target.
	other := append([]byte(nil), buf...)
	putU32(other[6:], 0x12345678)
	if _, _, ok := p.parsePayload(other); ok {
		t.Error("parsePayload accepted a payload with the wrong nonce")
	}
	if _, _, ok := p.parsePayload([]byte("hi")); ok {
		t.Error("parsePayload accepted a short payload")
	}
	notOurs := append([]byte(nil), buf...)
	notOurs[0] = 'x'
	if _, _, ok := p.parsePayload(notOurs); ok {
		t.Error("parsePayload accepted a payload without our magic")
	}
}

func TestQuotedDest(t *testing.T) {
	// A minimal IPv4 header with 203.0.113.9 as the destination.
	hdr := make([]byte, 20)
	hdr[0] = 0x45
	copy(hdr[16:20], []byte{203, 0, 113, 9})
	got, ok := quotedDest(hdr, false)
	if !ok || got != netip.MustParseAddr("203.0.113.9") {
		t.Errorf("quotedDest = %v, %v", got, ok)
	}
	if _, ok := quotedDest(hdr[:10], false); ok {
		t.Error("quotedDest accepted a truncated header")
	}
}

func TestStatAggregation(t *testing.T) {
	s := Stat{Addr: netip.MustParseAddr("10.0.0.1"), Sent: 4, Recv: 2,
		RTTs: []time.Duration{2 * time.Millisecond, 6 * time.Millisecond}}
	if got := s.AvgRTT(); got != 4*time.Millisecond {
		t.Errorf("AvgRTT = %v, want 4ms", got)
	}
	if got := s.MinRTT(); got != 2*time.Millisecond {
		t.Errorf("MinRTT = %v", got)
	}
	if got := s.MaxRTT(); got != 6*time.Millisecond {
		t.Errorf("MaxRTT = %v", got)
	}
	if got := s.LossPct(); got != 50 {
		t.Errorf("LossPct = %v, want 50", got)
	}
	var empty Stat
	if empty.Alive() || empty.LossPct() != 100 {
		t.Error("a Stat with no probes should be dead with 100% loss")
	}
}

func TestSafeNameCannotEscapeDataDir(t *testing.T) {
	for _, in := range []string{"../../etc/passwd", "a/b", "..", "", "   "} {
		got := safeName(in)
		if strings.ContainsAny(got, `/\`) || got == "." || got == ".." || got == "" {
			t.Errorf("safeName(%q) = %q, which is not safe as a filename", in, got)
		}
	}
	if got := safeName("before"); got != "before" {
		t.Errorf("safeName mangled an ordinary name: %q", got)
	}
}
