// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Targets is the output of a discovery run: the addresses inside each routed
// prefix that were found to answer ICMP, and are therefore worth using as
// before/after probes.
type Targets struct {
	DiscoveredAt time.Time       `json:"discoveredAt"`
	Host         string          `json:"host"`
	Budget       int             `json:"budget"`
	KeepPer      int             `json:"keepPerPrefix"`
	Prefixes     []PrefixTargets `json:"prefixes"`
}

// PrefixTargets records what discovery learned about one prefix.
type PrefixTargets struct {
	Prefix   netip.Prefix `json:"prefix"`
	NextHops []string     `json:"nextHops"`
	Probed   int          `json:"probed"` // candidate addresses swept
	Alive    []netip.Addr `json:"alive"`  // responders we kept, fastest first
}

// TotalTargets is the number of addresses a capture will probe.
func (t *Targets) TotalTargets() int {
	n := 0
	for _, p := range t.Prefixes {
		n += len(p.Alive)
	}
	return n
}

// Covered is the number of prefixes that have at least one live target. A
// prefix with no live target can't tell us anything in a before/after diff.
func (t *Targets) Covered() int {
	n := 0
	for _, p := range t.Prefixes {
		if len(p.Alive) > 0 {
			n++
		}
	}
	return n
}

// Snapshot is one "before" or "after" measurement: the routing table as it
// stood, plus reachability and latency for every discovered target.
type Snapshot struct {
	Name     string           `json:"name"`
	TakenAt  time.Time        `json:"takenAt"`
	Host     string           `json:"host"`
	Note     string           `json:"note,omitempty"`
	Count    int              `json:"count"` // echo requests per target
	Routes   []Route          `json:"routes"`
	Prefixes []PrefixSnapshot `json:"prefixes"`
}

// PrefixSnapshot is the measured state of one prefix at snapshot time.
type PrefixSnapshot struct {
	Prefix   netip.Prefix `json:"prefix"`
	NextHops []string     `json:"nextHops"`
	Targets  []Stat       `json:"targets"`
}

// AliveCount is how many of this prefix's targets answered.
func (p PrefixSnapshot) AliveCount() int {
	n := 0
	for _, t := range p.Targets {
		if t.Alive() {
			n++
		}
	}
	return n
}

// Up reports whether the prefix is reachable at all.
func (p PrefixSnapshot) Up() bool { return p.AliveCount() > 0 }

// Unreachables is how many ICMP unreachable/TTL-exceeded errors came back for
// this prefix. A non-zero count with no replies is the signature of a routing
// hole rather than a firewall silently dropping pings.
func (p PrefixSnapshot) Unreachables() int {
	n := 0
	for _, t := range p.Targets {
		n += t.Unreach
	}
	return n
}

// RTT is the prefix's representative latency: the median of its live targets'
// average round trips. The median keeps one pathological host from dominating.
func (p PrefixSnapshot) RTT() time.Duration {
	var vals []time.Duration
	for _, t := range p.Targets {
		if t.Alive() {
			vals = append(vals, t.AvgRTT())
		}
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	return vals[len(vals)/2]
}

// LossPct is the aggregate loss across the prefix's targets.
func (p PrefixSnapshot) LossPct() float64 {
	var sent, recv int
	for _, t := range p.Targets {
		sent += t.Sent
		recv += t.Recv
	}
	if sent == 0 {
		return 100
	}
	return float64(sent-recv) / float64(sent) * 100
}

// byPrefix indexes a snapshot for diffing.
func (s *Snapshot) byPrefix() map[netip.Prefix]PrefixSnapshot {
	m := make(map[netip.Prefix]PrefixSnapshot, len(s.Prefixes))
	for _, p := range s.Prefixes {
		m[p.Prefix] = p
	}
	return m
}

// --- on-disk storage -------------------------------------------------------

// dataDir is where targets and snapshots live. Override with ROUTEPROBE_DIR.
func dataDir() (string, error) {
	if d := os.Getenv("ROUTEPROBE_DIR"); d != "" {
		return d, os.MkdirAll(d, 0o755)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "routeprobe")
	return d, os.MkdirAll(d, 0o755)
}

func targetsPath() (string, error) {
	d, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "targets.json"), nil
}

// safeName keeps snapshot names to something that can't escape the data dir.
func safeName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), ".-")
	if s == "" {
		s = "snapshot"
	}
	return s
}

func snapshotPath(name string) (string, error) {
	d, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "snap-"+safeName(name)+".json"), nil
}

// writeJSON writes v atomically, so an interrupted run never leaves a
// half-written snapshot that later fails to parse.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func saveTargets(t *Targets) error {
	p, err := targetsPath()
	if err != nil {
		return err
	}
	return writeJSON(p, t)
}

func loadTargets() (*Targets, error) {
	p, err := targetsPath()
	if err != nil {
		return nil, err
	}
	var t Targets
	if err := readJSON(p, &t); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no targets yet — run discovery first")
		}
		return nil, err
	}
	return &t, nil
}

func saveSnapshot(s *Snapshot) error {
	p, err := snapshotPath(s.Name)
	if err != nil {
		return err
	}
	return writeJSON(p, s)
}

func loadSnapshot(name string) (*Snapshot, error) {
	p, err := snapshotPath(name)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := readJSON(p, &s); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no snapshot named %q", name)
		}
		return nil, err
	}
	return &s, nil
}

// SnapshotInfo is a directory listing entry.
type SnapshotInfo struct {
	Name     string
	TakenAt  time.Time
	Prefixes int
	Up       int
	Note     string
}

// listSnapshots returns saved snapshots, newest first.
func listSnapshots() ([]SnapshotInfo, error) {
	d, err := dataDir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(d)
	if err != nil {
		return nil, err
	}
	var out []SnapshotInfo
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "snap-") || !strings.HasSuffix(n, ".json") {
			continue
		}
		var s Snapshot
		if err := readJSON(filepath.Join(d, n), &s); err != nil {
			continue
		}
		up := 0
		for _, p := range s.Prefixes {
			if p.Up() {
				up++
			}
		}
		out = append(out, SnapshotInfo{
			Name:     s.Name,
			TakenAt:  s.TakenAt,
			Prefixes: len(s.Prefixes),
			Up:       up,
			Note:     s.Note,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TakenAt.After(out[j].TakenAt) })
	return out, nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
