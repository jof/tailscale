// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"tailscale.com/client/tailscale"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/paths"
)

// Route is one advertised prefix and the peer advertising it. A prefix may be
// advertised by several peers (exit nodes and HA subnet routers both do this),
// in which case it appears once per peer.
type Route struct {
	Prefix   netip.Prefix `json:"prefix"`
	NextHop  netip.Addr   `json:"nextHop"`
	PeerName string       `json:"peerName"`
	Online   bool         `json:"online"`
	Active   bool         `json:"active"`
}

// String renders a route as it appears in the UI, e.g.
// "10.20.0.0/16 via nuc-109 (100.64.240.78)".
func (r Route) String() string {
	return fmt.Sprintf("%s via %s (%s)", r.Prefix, r.ShortName(), r.NextHop)
}

// ShortName is the peer's hostname without the tailnet suffix or trailing dot,
// which is what fits in a table column.
func (r Route) ShortName() string {
	n := strings.TrimSuffix(r.PeerName, ".")
	if i := strings.Index(n, "."); i > 0 {
		n = n[:i]
	}
	if n == "" {
		return r.NextHop.String()
	}
	return n
}

func localClient() *tailscale.LocalClient {
	return &tailscale.LocalClient{Socket: paths.DefaultTailscaledSocket()}
}

// fetchRoutes returns every prefix advertised by a peer in the tailnet.
//
// Tailscale node IPs (the /32 and /128 for each peer's own address) are always
// excluded: they are not subnet routes, and probing them would just measure
// tailnet liveness rather than the routing we're trying to compare.
func fetchRoutes(ctx context.Context) ([]Route, *ipnstate.Status, error) {
	st, err := localClient().Status(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("talking to tailscaled: %w (is Tailscale running?)", err)
	}
	return routesFromStatus(st), st, nil
}

func routesFromStatus(st *ipnstate.Status) []Route {
	var routes []Route
	for _, peer := range st.Peer {
		if len(peer.TailscaleIPs) == 0 || peer.AllowedIPs == nil {
			continue
		}
		nextHop := peer.TailscaleIPs[0]
		for i := range peer.AllowedIPs.Len() {
			p := peer.AllowedIPs.At(i)
			if isNodeIP(p, peer.TailscaleIPs) {
				continue
			}
			routes = append(routes, Route{
				Prefix:   p,
				NextHop:  nextHop,
				PeerName: peer.DNSName,
				Online:   peer.Online,
				Active:   peer.Active,
			})
		}
	}
	sortRoutes(routes)
	return routes
}

func isNodeIP(p netip.Prefix, nodeIPs []netip.Addr) bool {
	for _, ip := range nodeIPs {
		if p.Bits() == ip.BitLen() && p.Addr() == ip {
			return true
		}
	}
	return false
}

// sortRoutes orders routes by address, then most-specific first, then by peer
// so that repeated runs produce identical output.
func sortRoutes(routes []Route) {
	sort.Slice(routes, func(i, j int) bool {
		a, b := routes[i], routes[j]
		if c := a.Prefix.Addr().Compare(b.Prefix.Addr()); c != 0 {
			return c < 0
		}
		if a.Prefix.Bits() != b.Prefix.Bits() {
			return a.Prefix.Bits() > b.Prefix.Bits()
		}
		return a.PeerName < b.PeerName
	})
}

// RouteTable answers "where would a packet to this address go?" the same way
// the kernel would: longest prefix wins.
type RouteTable struct {
	byLen []Route // sorted by prefix length, descending
}

func NewRouteTable(routes []Route) *RouteTable {
	cp := append([]Route(nil), routes...)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].Prefix.Bits() > cp[j].Prefix.Bits() })
	return &RouteTable{byLen: cp}
}

// Lookup returns every route sharing the most specific prefix that contains
// addr. More than one route comes back when several peers advertise the same
// prefix, which is exactly the case where a routing change is easy to miss.
func (rt *RouteTable) Lookup(addr netip.Addr) []Route {
	var best netip.Prefix
	var out []Route
	for _, r := range rt.byLen {
		if r.Prefix.Addr().Is4() != addr.Is4() {
			continue
		}
		if !r.Prefix.Contains(addr) {
			continue
		}
		if out == nil {
			best = r.Prefix
		} else if r.Prefix != best {
			// byLen is descending, so once the length drops we're done.
			break
		}
		out = append(out, r)
	}
	return out
}

// nextHopSet renders the set of next hops for a prefix as a stable, comparable
// list of strings. Comparing these strings across snapshots is how we detect
// that traffic is now taking a different path.
func nextHopSet(routes []Route) []string {
	seen := make(map[string]bool, len(routes))
	var out []string
	for _, r := range routes {
		s := fmt.Sprintf("%s (%s)", r.ShortName(), r.NextHop)
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// groupByPrefix collapses routes into one entry per distinct prefix, carrying
// all of that prefix's next hops.
func groupByPrefix(routes []Route) ([]netip.Prefix, map[netip.Prefix][]Route) {
	m := make(map[netip.Prefix][]Route)
	var order []netip.Prefix
	for _, r := range routes {
		if _, ok := m[r.Prefix]; !ok {
			order = append(order, r.Prefix)
		}
		m[r.Prefix] = append(m[r.Prefix], r)
	}
	return order, m
}
