// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/binary"
	"net/netip"
)

// defaultRouteProbes are the destinations used to test a 0.0.0.0/0 or ::/0
// route. A default route can't be swept, but an exit node either carries you
// to the public internet or it doesn't, and these anycast resolvers answer
// ICMP reliably from everywhere.
var defaultRouteProbes = []netip.Addr{
	netip.MustParseAddr("1.1.1.1"),
	netip.MustParseAddr("8.8.8.8"),
	netip.MustParseAddr("9.9.9.9"),
	netip.MustParseAddr("208.67.222.222"),
}

var defaultRouteProbes6 = []netip.Addr{
	netip.MustParseAddr("2606:4700:4700::1111"),
	netip.MustParseAddr("2001:4860:4860::8888"),
}

// gatewayOffsets are the host offsets tried first inside a sampled subnet.
// Routers, switches and hypervisors cluster at the bottom of a /24, and the
// broadcast-adjacent .254 is the second most common gateway convention.
var gatewayOffsets = []uint32{1, 254, 10, 100}

// candidatesFor returns up to budget addresses worth probing inside p.
//
// Small prefixes are enumerated exhaustively. Anything too large to sweep is
// sampled: we walk the /24s inside it (striding when there are more /24s than
// budget allows) and try the addresses most likely to host something that
// answers ICMP. The result is deterministic, so re-running discovery on the
// same prefix probes the same addresses.
func candidatesFor(p netip.Prefix, budget int) []netip.Addr {
	p = p.Masked()
	if budget <= 0 {
		budget = 1
	}
	if p.Bits() == 0 {
		if p.Addr().Is4() {
			return defaultRouteProbes
		}
		return defaultRouteProbes6
	}
	if p.Addr().Is4() {
		return candidates4(p, budget)
	}
	return candidates6(p, budget)
}

func candidates4(p netip.Prefix, budget int) []netip.Addr {
	bits := p.Bits()
	hostBits := 32 - bits
	base4 := p.Addr().As4()
	base := binary.BigEndian.Uint32(base4[:])

	at := func(off uint32) netip.Addr {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], base+off)
		return netip.AddrFrom4(b)
	}

	switch {
	case hostBits == 0: // /32
		return []netip.Addr{p.Addr()}
	case hostBits == 1: // /31, point-to-point: both addresses are usable
		return []netip.Addr{at(0), at(1)}
	}

	// size is the number of addresses in the prefix; usable excludes the
	// network and broadcast addresses.
	size := uint32(1) << hostBits
	usable := size - 2

	if usable <= uint32(budget) {
		out := make([]netip.Addr, 0, usable)
		for off := uint32(1); off < size-1; off++ {
			out = append(out, at(off))
		}
		return out
	}

	// Too big to enumerate: sample across the /24s inside it.
	//
	// Two knobs interact. `stride` spreads the sample over the whole prefix
	// when there are more /24s than the budget can visit; `perSubnet` spends
	// whatever budget is left over on extra addresses within each /24 we do
	// visit. Without the second one a /21 would get only 32 of its 256 probes.
	minPer := uint32(len(gatewayOffsets))
	numSubnets := uint32(1) << (24 - bits) // bits < 24 here: a /24 enumerates fully
	visit := uint32(budget) / minPer
	if visit == 0 {
		visit = 1
	}
	stride := uint32(1)
	if numSubnets > visit {
		stride = (numSubnets + visit - 1) / visit
	}
	visited := (numSubnets + stride - 1) / stride
	perSubnet := uint32(budget) / visited
	if perSubnet < minPer {
		perSubnet = minPer
	}
	if perSubnet > 254 {
		perSubnet = 254
	}
	offsets := subnetOffsets(perSubnet)

	out := make([]netip.Addr, 0, budget)
	for s := uint32(0); s < numSubnets && len(out) < budget; s += stride {
		subBase := s << 8
		for _, g := range offsets {
			off := subBase + g
			if off == 0 || off >= size-1 {
				continue
			}
			out = append(out, at(off))
			if len(out) >= budget {
				break
			}
		}
	}
	return out
}

// subnetOffsets returns n host offsets to try inside a /24: the gateway
// conventions first, then a stride-spread of the rest so the sample covers the
// whole subnet rather than clustering at its bottom.
func subnetOffsets(n uint32) []uint32 {
	out := append([]uint32(nil), gatewayOffsets...)
	if n <= uint32(len(out)) {
		return out[:n]
	}
	used := make(map[uint32]bool, n)
	for _, g := range out {
		used[g] = true
	}
	remaining := n - uint32(len(out))
	stride := uint32(254) / (remaining + 1)
	if stride < 1 {
		stride = 1
	}
	for o := uint32(1); o <= 254 && uint32(len(out)) < n; o += stride {
		if !used[o] {
			used[o] = true
			out = append(out, o)
		}
	}
	// The stride can leave us short when it doesn't divide evenly; top up.
	for o := uint32(1); o <= 254 && uint32(len(out)) < n; o++ {
		if !used[o] {
			used[o] = true
			out = append(out, o)
		}
	}
	return out
}

// candidates6 samples an IPv6 prefix. Sweeping IPv6 is not a thing you can do,
// so we probe the handful of addresses that operators actually assign: the
// subnet-router anycast address (all-zero host part) and the first few hosts.
func candidates6(p netip.Prefix, budget int) []netip.Addr {
	if p.Bits() == 128 {
		return []netip.Addr{p.Addr()}
	}
	out := []netip.Addr{p.Addr()} // subnet-router anycast
	a := p.Addr()
	for len(out) < budget && len(out) < 8 {
		a = a.Next()
		if !p.Contains(a) {
			break
		}
		out = append(out, a)
	}
	return out
}

// sweepCost reports how many probes candidatesFor would produce, without
// building the slice. Used to show the user what a discovery run will cost
// before they commit to it.
func sweepCost(p netip.Prefix, budget int) int {
	return len(candidatesFor(p, budget))
}

// isSweepable reports whether a prefix is worth attempting at all. Prefixes
// that only cover the tailnet's own CGNAT space are skipped: they're node
// addresses, not routed subnets.
func isSweepable(p netip.Prefix) bool {
	if !p.IsValid() {
		return false
	}
	if p.Addr().Is4() && p.Bits() == 0 {
		return true // default route: handled via defaultRouteProbes
	}
	// 100.64.0.0/10 is Tailscale's own address space.
	tsRange := netip.MustParsePrefix("100.64.0.0/10")
	if p.Addr().Is4() && tsRange.Contains(p.Addr()) && p.Bits() >= tsRange.Bits() {
		return false
	}
	return true
}
