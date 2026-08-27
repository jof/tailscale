// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// probeMagic tags our echo payloads so we ignore replies to any other ping
// running on the box.
var probeMagic = [6]byte{'r', 'p', 'r', 'b', 0x01, 0x00}

// payloadLen is magic(6) + nonce(4) + token(4) + sendTime(8).
const payloadLen = 22

// SweepConfig controls a sweep.
type SweepConfig struct {
	Count    int           // echo requests per target
	Timeout  time.Duration // how long to keep listening after the last send
	Interval time.Duration // pause between rounds
	PPS      int           // global send rate ceiling, packets per second
}

func (c SweepConfig) withDefaults() SweepConfig {
	if c.Count <= 0 {
		c.Count = 1
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Second
	}
	if c.PPS <= 0 {
		c.PPS = 2000
	}
	return c
}

// Stat is the outcome for a single probed address.
type Stat struct {
	Addr    netip.Addr      `json:"addr"`
	Sent    int             `json:"sent"`
	Recv    int             `json:"recv"`
	Unreach int             `json:"unreach,omitempty"` // ICMP unreachable / TTL exceeded
	RTTs    []time.Duration `json:"rtts,omitempty"`
}

// Alive reports whether the address answered at least once.
func (s Stat) Alive() bool { return s.Recv > 0 }

// LossPct is the fraction of probes that went unanswered, 0..100.
func (s Stat) LossPct() float64 {
	if s.Sent == 0 {
		return 100
	}
	return float64(s.Sent-s.Recv) / float64(s.Sent) * 100
}

// AvgRTT is the mean round trip of the answered probes, or 0 if none answered.
func (s Stat) AvgRTT() time.Duration {
	if len(s.RTTs) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range s.RTTs {
		total += d
	}
	return total / time.Duration(len(s.RTTs))
}

// MinRTT returns the fastest observed round trip, or 0 if none answered.
func (s Stat) MinRTT() time.Duration {
	var min time.Duration
	for _, d := range s.RTTs {
		if min == 0 || d < min {
			min = d
		}
	}
	return min
}

// MaxRTT returns the slowest observed round trip, or 0 if none answered.
func (s Stat) MaxRTT() time.Duration {
	var max time.Duration
	for _, d := range s.RTTs {
		if d > max {
			max = d
		}
	}
	return max
}

// Progress is a live view of a running sweep, safe to read from another
// goroutine (the TUI polls it every frame rather than being called back).
type Progress struct {
	Total   atomic.Int64
	Sent    atomic.Int64
	Replies atomic.Int64
	Alive   atomic.Int64
	label   atomic.Value // string
}

func (p *Progress) SetLabel(s string) {
	if p != nil {
		p.label.Store(s)
	}
}

func (p *Progress) Label() string {
	if p == nil {
		return ""
	}
	s, _ := p.label.Load().(string)
	return s
}

// Frac is how far along the sweep is, 0..1.
func (p *Progress) Frac() float64 {
	if p == nil {
		return 0
	}
	t := p.Total.Load()
	if t <= 0 {
		return 0
	}
	f := float64(p.Sent.Load()) / float64(t)
	if f > 1 {
		return 1
	}
	return f
}

// Pinger owns the ICMP sockets. One Pinger can run many sweeps sequentially.
type Pinger struct {
	conn4, conn6 *icmp.PacketConn
	raw4, raw6   bool // true if the socket is a raw one (needs root)
	nonce        uint32
	echoID       int
}

// NewPinger opens ICMP sockets, preferring the unprivileged datagram form.
//
// macOS allows unprivileged ICMP datagram sockets outright. Linux allows them
// for GIDs inside net.ipv4.ping_group_range. Where neither applies we fall
// back to raw sockets, which need root or CAP_NET_RAW.
func NewPinger() (*Pinger, error) {
	p := &Pinger{
		nonce:  uint32(time.Now().UnixNano()),
		echoID: os.Getpid() & 0xffff,
	}
	var err4, err6 error
	p.conn4, p.raw4, err4 = listenICMP("udp4", "ip4:icmp", "0.0.0.0")
	p.conn6, p.raw6, err6 = listenICMP("udp6", "ip6:ipv6-icmp", "::")
	if p.conn4 == nil && p.conn6 == nil {
		return nil, fmt.Errorf("could not open an ICMP socket (v4: %v; v6: %v).\n%s", err4, err6, icmpPermHelp())
	}
	return p, nil
}

func listenICMP(dgramNet, rawNet, addr string) (*icmp.PacketConn, bool, error) {
	c, err := icmp.ListenPacket(dgramNet, addr)
	if err == nil {
		return c, false, nil
	}
	dgramErr := err
	c, err = icmp.ListenPacket(rawNet, addr)
	if err == nil {
		return c, true, nil
	}
	return nil, false, fmt.Errorf("%v (raw: %v)", dgramErr, err)
}

func icmpPermHelp() string {
	return "On Linux, allow unprivileged pings with:\n" +
		"    sudo sysctl -w net.ipv4.ping_group_range=\"0 2147483647\"\n" +
		"or run this command under sudo. On macOS it should work as your user."
}

// Has4 and Has6 report which families this Pinger can actually probe.
func (p *Pinger) Has4() bool { return p.conn4 != nil }
func (p *Pinger) Has6() bool { return p.conn6 != nil }

func (p *Pinger) Close() error {
	var err error
	if p.conn4 != nil {
		err = errors.Join(err, p.conn4.Close())
	}
	if p.conn6 != nil {
		err = errors.Join(err, p.conn6.Close())
	}
	return err
}

// putU32 and putU64 write big-endian integers into a probe payload.
func putU32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }

// reply is one decoded inbound ICMP message attributed to a probe.
type reply struct {
	token   uint32
	addr    netip.Addr
	rtt     time.Duration
	unreach bool
}

// Sweep sends cfg.Count echo requests to every target and returns per-target
// statistics. Results are returned in the same order as targets.
//
// The sweep is a single pass over one socket per family: there is no goroutine
// per host, so sweeping tens of thousands of addresses costs a few kilobytes
// rather than a few hundred megabytes.
func (p *Pinger) Sweep(ctx context.Context, targets []netip.Addr, cfg SweepConfig, prog *Progress) ([]Stat, error) {
	cfg = cfg.withDefaults()
	if len(targets) == 0 {
		return nil, nil
	}

	stats := make([]Stat, len(targets))
	byAddr := make(map[netip.Addr]int, len(targets))
	for i, t := range targets {
		stats[i] = Stat{Addr: t}
		byAddr[t] = i
	}

	total := int64(len(targets) * cfg.Count)
	if prog != nil {
		prog.Total.Store(total)
		prog.Sent.Store(0)
		prog.Replies.Store(0)
		prog.Alive.Store(0)
	}

	replies := make(chan reply, 4096)
	readCtx, stopReaders := context.WithCancel(ctx)
	defer stopReaders()

	var readers sync.WaitGroup
	if p.conn4 != nil {
		readers.Add(1)
		go func() { defer readers.Done(); p.readLoop(readCtx, p.conn4, false, byAddr, replies) }()
	}
	if p.conn6 != nil {
		readers.Add(1)
		go func() { defer readers.Done(); p.readLoop(readCtx, p.conn6, true, byAddr, replies) }()
	}

	// Collector folds replies into stats. It owns `stats` for the duration, so
	// nothing else may touch it until the collector exits.
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		for r := range replies {
			idx := int(r.token / uint32(cfg.Count))
			if r.unreach {
				if i, ok := byAddr[r.addr]; ok {
					stats[i].Unreach++
				}
				continue
			}
			if idx < 0 || idx >= len(stats) || stats[idx].Addr != r.addr {
				continue // stale or spoofed; token didn't match the source
			}
			wasAlive := stats[idx].Recv > 0
			stats[idx].Recv++
			stats[idx].RTTs = append(stats[idx].RTTs, r.rtt)
			if prog != nil {
				prog.Replies.Add(1)
				if !wasAlive {
					prog.Alive.Add(1)
				}
			}
		}
	}()

	// Send every round, pacing to cfg.PPS.
	sendErr := p.sendAll(ctx, targets, cfg, prog, stats)

	// Give late replies a chance to land before tearing the readers down.
	select {
	case <-time.After(cfg.Timeout):
	case <-ctx.Done():
	}
	stopReaders()
	readers.Wait()
	close(replies)
	<-collectorDone

	if sendErr != nil && ctx.Err() == nil {
		return stats, sendErr
	}
	return stats, ctx.Err()
}

// sendAll paces the outbound probes. stats is written only for the Sent
// counter, which the collector never touches, so this is safe to run alongside
// the collector goroutine.
func (p *Pinger) sendAll(ctx context.Context, targets []netip.Addr, cfg SweepConfig, prog *Progress, stats []Stat) error {
	perPacket := time.Second / time.Duration(cfg.PPS)
	start := time.Now()
	n := 0

	for round := range cfg.Count {
		if round > 0 && cfg.Interval > 0 {
			select {
			case <-time.After(cfg.Interval):
			case <-ctx.Done():
				return ctx.Err()
			}
			start = time.Now()
			n = 0
		}
		for i, t := range targets {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Pace: only actually sleep when we're meaningfully ahead, so we
			// don't burn a syscall per packet at high rates.
			if due := start.Add(time.Duration(n) * perPacket); time.Until(due) > 250*time.Microsecond {
				select {
				case <-time.After(time.Until(due)):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			n++

			token := uint32(i)*uint32(cfg.Count) + uint32(round)
			if err := p.send(t, token); err == nil {
				stats[i].Sent++
			}
			if prog != nil {
				prog.Sent.Add(1)
			}
		}
	}
	return nil
}

func (p *Pinger) send(dst netip.Addr, token uint32) error {
	conn := p.conn4
	raw := p.raw4
	msgType := icmp.Type(ipv4.ICMPTypeEcho)
	if dst.Is6() {
		conn = p.conn6
		raw = p.raw6
		msgType = ipv6.ICMPTypeEchoRequest
	}
	if conn == nil {
		return errors.New("no socket for address family")
	}

	payload := make([]byte, payloadLen)
	copy(payload, probeMagic[:])
	putU32(payload[6:], p.nonce)
	putU32(payload[10:], token)
	putU64(payload[14:], uint64(time.Now().UnixNano()))

	wm := icmp.Message{
		Type: msgType,
		Code: 0,
		Body: &icmp.Echo{
			ID: p.echoID,
			// Sequence is only a hint; attribution is done from the payload.
			Seq:  int(token & 0xffff),
			Data: payload,
		},
	}
	b, err := wm.Marshal(nil)
	if err != nil {
		return err
	}

	ip := net.IP(dst.AsSlice())
	var to net.Addr
	if raw {
		to = &net.IPAddr{IP: ip, Zone: dst.Zone()}
	} else {
		to = &net.UDPAddr{IP: ip, Zone: dst.Zone()}
	}
	_, err = conn.WriteTo(b, to)
	return err
}

// readLoop decodes inbound ICMP until ctx is cancelled.
func (p *Pinger) readLoop(ctx context.Context, conn *icmp.PacketConn, v6 bool, byAddr map[netip.Addr]int, out chan<- reply) {
	proto := 1 // ICMPv4
	if v6 {
		proto = 58 // ICMPv6
	}
	buf := make([]byte, 1500)
	for ctx.Err() == nil {
		if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			return
		}
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		src, ok := addrFromNetAddr(peer)
		if !ok {
			continue
		}
		msg, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		now := time.Now()

		switch body := msg.Body.(type) {
		case *icmp.Echo:
			if msg.Type != ipv4.ICMPTypeEchoReply && msg.Type != ipv6.ICMPTypeEchoReply {
				continue // our own outbound echo, looped back
			}
			token, sent, ok := p.parsePayload(body.Data)
			if !ok {
				continue
			}
			select {
			case out <- reply{token: token, addr: src, rtt: now.Sub(sent)}:
			case <-ctx.Done():
				return
			}

		case *icmp.DstUnreach:
			p.reportUnreachable(ctx, body.Data, v6, byAddr, out)
		case *icmp.TimeExceeded:
			p.reportUnreachable(ctx, body.Data, v6, byAddr, out)
		}
	}
}

// parsePayload validates that a payload is ours and extracts its token and
// send timestamp.
func (p *Pinger) parsePayload(b []byte) (token uint32, sent time.Time, ok bool) {
	if len(b) < payloadLen {
		return 0, time.Time{}, false
	}
	if string(b[:6]) != string(probeMagic[:]) {
		return 0, time.Time{}, false
	}
	if binary.BigEndian.Uint32(b[6:]) != p.nonce {
		return 0, time.Time{}, false // a probe from an earlier run of ours
	}
	token = binary.BigEndian.Uint32(b[10:])
	sent = time.Unix(0, int64(binary.BigEndian.Uint64(b[14:])))
	return token, sent, true
}

// reportUnreachable attributes an ICMP error to a target.
//
// Routers quote only the offending IP header plus 8 bytes, which is not enough
// to recover our payload token, so we recover the destination address from the
// quoted header instead. That's the signal that matters: "this destination
// became unreachable" is a routing change, not a dropped packet.
func (p *Pinger) reportUnreachable(ctx context.Context, quoted []byte, v6 bool, byAddr map[netip.Addr]int, out chan<- reply) {
	dst, ok := quotedDest(quoted, v6)
	if !ok {
		return
	}
	if _, known := byAddr[dst]; !known {
		return
	}
	select {
	case out <- reply{addr: dst, unreach: true}:
	case <-ctx.Done():
	}
}

// quotedDest pulls the destination address out of an IP header quoted inside
// an ICMP error message.
func quotedDest(b []byte, v6 bool) (netip.Addr, bool) {
	if v6 {
		if len(b) < 40 {
			return netip.Addr{}, false
		}
		return netip.AddrFrom16([16]byte(b[24:40])), true
	}
	if len(b) < 20 || b[0]>>4 != 4 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte(b[16:20])), true
}

func addrFromNetAddr(a net.Addr) (netip.Addr, bool) {
	switch v := a.(type) {
	case *net.UDPAddr:
		ip, ok := netip.AddrFromSlice(v.IP)
		return ip.Unmap(), ok
	case *net.IPAddr:
		ip, ok := netip.AddrFromSlice(v.IP)
		return ip.Unmap(), ok
	}
	return netip.Addr{}, false
}
