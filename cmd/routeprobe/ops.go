// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"time"
)

// DiscoverOptions tunes a discovery sweep.
type DiscoverOptions struct {
	Budget         int           // candidate addresses probed per prefix
	Keep           int           // live targets retained per prefix
	PPS            int           // send rate ceiling
	Timeout        time.Duration // how long to wait for the last replies
	IncludeDefault bool          // also probe 0.0.0.0/0 via public anycast addresses
	Only           []netip.Prefix
}

// DefaultDiscoverOptions are tuned to sweep a few hundred routed prefixes in
// well under a minute without looking like a port scan to anyone's IDS.
func DefaultDiscoverOptions() DiscoverOptions {
	return DiscoverOptions{
		Budget:         256,
		Keep:           3,
		PPS:            2000,
		Timeout:        2 * time.Second,
		IncludeDefault: true,
	}
}

// Discover sweeps every routed prefix looking for addresses that answer ICMP,
// and records the best few per prefix as the fixed target set for later
// before/after captures.
//
// Using a fixed target set is the point: comparing two captures is only
// meaningful if both measured the same destinations.
func Discover(ctx context.Context, p *Pinger, opts DiscoverOptions, prog *Progress) (*Targets, error) {
	if opts.Budget <= 0 {
		opts.Budget = 256
	}
	if opts.Keep <= 0 {
		opts.Keep = 3
	}

	prog.SetLabel("asking tailscaled for routes")
	routes, _, err := fetchRoutes(ctx)
	if err != nil {
		return nil, err
	}
	order, byPfx := groupByPrefix(routes)

	// Restrict to the prefixes we're actually going to probe.
	var probed []netip.Prefix
	for _, pfx := range order {
		if !isSweepable(pfx) {
			continue
		}
		if pfx.Bits() == 0 && !opts.IncludeDefault {
			continue
		}
		if pfx.Addr().Is4() && !p.Has4() {
			continue
		}
		if pfx.Addr().Is6() && !p.Has6() {
			continue
		}
		if len(opts.Only) > 0 && !matchesAny(pfx, opts.Only) {
			continue
		}
		probed = append(probed, pfx)
	}
	if len(probed) == 0 {
		return nil, fmt.Errorf("no probeable prefixes found in the route table")
	}

	// Each candidate address is attributed to the most specific prefix that
	// contains it, so overlapping routes (a /16 plus a /24 carved out of it)
	// don't end up testing each other's path.
	lpm := newPrefixMatcher(probed)
	cands := make(map[netip.Prefix][]netip.Addr, len(probed))
	var all []netip.Addr
	seen := make(map[netip.Addr]bool)
	for _, pfx := range probed {
		var keep []netip.Addr
		for _, a := range candidatesFor(pfx, opts.Budget) {
			if pfx.Bits() > 0 {
				if owner, ok := lpm.lookup(a); ok && owner != pfx {
					continue
				}
			}
			if seen[a] {
				continue
			}
			seen[a] = true
			keep = append(keep, a)
			all = append(all, a)
		}
		cands[pfx] = keep
	}

	prog.SetLabel(fmt.Sprintf("sweeping %d addresses across %d prefixes", len(all), len(probed)))
	stats, err := p.Sweep(ctx, all, SweepConfig{
		Count:   1,
		Timeout: opts.Timeout,
		PPS:     opts.PPS,
	}, prog)
	if err != nil {
		return nil, err
	}

	byAddr := make(map[netip.Addr]Stat, len(stats))
	for _, s := range stats {
		byAddr[s.Addr] = s
	}

	t := &Targets{
		DiscoveredAt: time.Now(),
		Host:         hostname(),
		Budget:       opts.Budget,
		KeepPer:      opts.Keep,
	}
	for _, pfx := range probed {
		var alive []Stat
		for _, a := range cands[pfx] {
			if s, ok := byAddr[a]; ok && s.Alive() {
				alive = append(alive, s)
			}
		}
		// Prefer the fastest responders: they're the least likely to be a
		// congested or rate-limiting host, so their RTT is a cleaner signal.
		sort.Slice(alive, func(i, j int) bool { return alive[i].AvgRTT() < alive[j].AvgRTT() })
		if len(alive) > opts.Keep {
			alive = alive[:opts.Keep]
		}
		pt := PrefixTargets{
			Prefix:   pfx,
			NextHops: nextHopSet(byPfx[pfx]),
			Probed:   len(cands[pfx]),
		}
		for _, s := range alive {
			pt.Alive = append(pt.Alive, s.Addr)
		}
		t.Prefixes = append(t.Prefixes, pt)
	}
	prog.SetLabel(fmt.Sprintf("found live targets in %d of %d prefixes", t.Covered(), len(probed)))
	return t, nil
}

// CaptureOptions tunes a before/after measurement.
type CaptureOptions struct {
	Name     string
	Note     string
	Count    int // echo requests per target
	PPS      int
	Timeout  time.Duration
	Interval time.Duration // gap between rounds
}

// DefaultCaptureOptions send five probes per target, which is enough to see
// partial loss and to average out a single unlucky round trip.
func DefaultCaptureOptions() CaptureOptions {
	return CaptureOptions{
		Count:    5,
		PPS:      1000,
		Timeout:  2 * time.Second,
		Interval: 200 * time.Millisecond,
	}
}

// Capture measures the current reachability and latency of every discovered
// target, alongside the route table as it stands right now.
//
// The route table is re-read at capture time rather than reused from
// discovery: a change to routing is exactly what we expect to happen between
// the two captures, and recording it is half the diff.
func Capture(ctx context.Context, p *Pinger, tg *Targets, opts CaptureOptions, prog *Progress) (*Snapshot, error) {
	if opts.Count <= 0 {
		opts.Count = 5
	}
	prog.SetLabel("reading current route table")
	routes, _, err := fetchRoutes(ctx)
	if err != nil {
		return nil, err
	}
	_, byPfx := groupByPrefix(routes)

	var all []netip.Addr
	for _, pt := range tg.Prefixes {
		all = append(all, pt.Alive...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no live targets to probe — re-run discovery")
	}

	prog.SetLabel(fmt.Sprintf("probing %d targets x%d", len(all), opts.Count))
	stats, err := p.Sweep(ctx, all, SweepConfig{
		Count:    opts.Count,
		Timeout:  opts.Timeout,
		Interval: opts.Interval,
		PPS:      opts.PPS,
	}, prog)
	if err != nil {
		return nil, err
	}
	byAddr := make(map[netip.Addr]Stat, len(stats))
	for _, s := range stats {
		byAddr[s.Addr] = s
	}

	s := &Snapshot{
		Name:    opts.Name,
		TakenAt: time.Now(),
		Host:    hostname(),
		Note:    opts.Note,
		Count:   opts.Count,
		Routes:  routes,
	}
	for _, pt := range tg.Prefixes {
		if len(pt.Alive) == 0 {
			continue // nothing measurable; leave it out of the diff entirely
		}
		ps := PrefixSnapshot{
			Prefix:   pt.Prefix,
			NextHops: nextHopSet(byPfx[pt.Prefix]),
		}
		for _, a := range pt.Alive {
			if st, ok := byAddr[a]; ok {
				ps.Targets = append(ps.Targets, st)
			}
		}
		s.Prefixes = append(s.Prefixes, ps)
	}
	prog.SetLabel("capture complete")
	return s, nil
}

// prefixMatcher does longest-prefix matching over a fixed set of prefixes.
type prefixMatcher struct {
	sorted []netip.Prefix // descending by prefix length
}

func newPrefixMatcher(pfxs []netip.Prefix) *prefixMatcher {
	cp := append([]netip.Prefix(nil), pfxs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Bits() > cp[j].Bits() })
	return &prefixMatcher{sorted: cp}
}

func (m *prefixMatcher) lookup(a netip.Addr) (netip.Prefix, bool) {
	for _, p := range m.sorted {
		if p.Addr().Is4() != a.Is4() {
			continue
		}
		if p.Contains(a) {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

func matchesAny(p netip.Prefix, set []netip.Prefix) bool {
	for _, s := range set {
		if s == p || (s.Addr().Is4() == p.Addr().Is4() && s.Bits() <= p.Bits() && s.Contains(p.Addr())) {
			return true
		}
	}
	return false
}
