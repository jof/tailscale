// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Command routeprobe finds pingable destinations inside every prefix your
// tailnet routes, then captures before/after reachability and latency
// snapshots so you can see exactly what a routing change did.
//
// The usual session is the interactive one:
//
//	routeprobe
//
// which walks you through discover → capture before → make your change →
// capture after → compare. Every step is also a subcommand, so the same
// workflow scripts cleanly:
//
//	routeprobe discover
//	routeprobe before
//	... change something ...
//	routeprobe after
//	routeprobe diff
//
// Probing is plain ICMP echo from this host, so it measures the path your
// kernel actually takes — including whatever Tailscale installed in the
// routing table. No external scanner is required; the sweeper here does the
// same job as an fping or zmap sweep, one socket for the whole run.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "":
		err = runTUI(ctx)
	case "discover", "scan":
		err = cmdDiscover(ctx, args)
	case "capture":
		err = cmdCapture(ctx, args, "")
	case "before":
		err = cmdCapture(ctx, args, "before")
	case "after":
		err = cmdCapture(ctx, args, "after")
	case "diff", "compare":
		err = cmdDiff(ctx, args)
	case "routes":
		err = cmdRoutes(ctx)
	case "targets":
		err = cmdTargets()
	case "list", "snapshots":
		err = cmdList()
	case "where":
		err = cmdWhere(ctx, args)
	case "ping":
		err = cmdPing(ctx, args)
	case "help", "-h", "--help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "\ninterrupted.")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "%s %v\n", colRed.paint("error:"), err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `routeprobe — before/after routing diffs for your tailnet

  routeprobe                     interactive TUI (start here)

  routeprobe discover            sweep every routed prefix for pingable hosts
      -budget N                  candidate addresses per prefix (default 256)
      -keep N                    live targets kept per prefix (default 3)
      -pps N                     send rate ceiling (default 2000)
      -only PREFIX,...           restrict to these prefixes
      -no-default                skip 0.0.0.0/0 exit-node probing

  routeprobe before              capture the "before" snapshot
  routeprobe after               capture the "after" snapshot
  routeprobe capture NAME        capture under an arbitrary name
      -count N                   echo requests per target (default 5)
      -note TEXT                 free-text note stored with the snapshot

  routeprobe diff [BEFORE AFTER] compare two snapshots (default: before after)
      -all                       include unchanged prefixes
      -o FILE                    also write a plain-text report

  routeprobe routes              show the tailnet route table
  routeprobe targets             show the discovered target set
  routeprobe list                list saved snapshots
  routeprobe where ADDR          show which route an address would take
  routeprobe ping ADDR...        sweep specific addresses (checks ICMP works)
      -count N                   echo requests per address (default 3)

Snapshots live in $ROUTEPROBE_DIR, or the OS cache dir if that is unset.
"routeprobe diff" exits non-zero when it finds regressions, so it works as a
post-change gate in a script.
`)
}

// --- subcommands -----------------------------------------------------------

func runTUI(ctx context.Context) error {
	p, err := NewPinger()
	if err != nil {
		return err
	}
	defer p.Close()

	scr, err := NewScreen()
	if err != nil {
		return err
	}
	// Restore the terminal even if a render panics; otherwise the user's shell
	// is left in raw mode with no cursor.
	defer scr.Close()

	newApp(ctx, scr, p).Run()
	return nil
}

func cmdDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	opts := DefaultDiscoverOptions()
	fs.IntVar(&opts.Budget, "budget", opts.Budget, "candidate addresses probed per prefix")
	fs.IntVar(&opts.Keep, "keep", opts.Keep, "live targets kept per prefix")
	fs.IntVar(&opts.PPS, "pps", opts.PPS, "send rate ceiling, packets per second")
	only := fs.String("only", "", "comma-separated prefixes to restrict discovery to")
	noDefault := fs.Bool("no-default", false, "skip default-route (exit node) probing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts.IncludeDefault = !*noDefault
	var err error
	if opts.Only, err = parsePrefixes(*only); err != nil {
		return err
	}

	p, err := NewPinger()
	if err != nil {
		return err
	}
	defer p.Close()

	prog := &Progress{}
	done := startCLIProgress(ctx, prog, "discovering")
	t, err := Discover(ctx, p, opts, prog)
	done()
	if err != nil {
		return err
	}
	if err := saveTargets(t); err != nil {
		return err
	}
	fmt.Printf("%s %d live targets across %d of %d prefixes\n",
		colGreen.paint("✔ discovered"), t.TotalTargets(), t.Covered(), len(t.Prefixes))

	// Call out prefixes we couldn't get a foothold in: they'll be invisible in
	// every later comparison, and that's worth knowing up front.
	var blind []string
	for _, pt := range t.Prefixes {
		if len(pt.Alive) == 0 {
			blind = append(blind, pt.Prefix.String())
		}
	}
	if len(blind) > 0 {
		fmt.Printf("%s %d prefix(es) had no responder and will not be compared:\n  %s\n",
			colYellow.paint("!"), len(blind), strings.Join(blind, " "))
	}
	return nil
}

func cmdCapture(ctx context.Context, args []string, name string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	opts := DefaultCaptureOptions()
	fs.IntVar(&opts.Count, "count", opts.Count, "echo requests per target")
	fs.IntVar(&opts.PPS, "pps", opts.PPS, "send rate ceiling, packets per second")
	fs.StringVar(&opts.Note, "note", "", "note stored alongside the snapshot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" {
		if fs.NArg() < 1 {
			return fmt.Errorf("capture needs a snapshot name")
		}
		name = fs.Arg(0)
	}
	opts.Name = name

	tg, err := loadTargets()
	if err != nil {
		return err
	}
	p, err := NewPinger()
	if err != nil {
		return err
	}
	defer p.Close()

	prog := &Progress{}
	done := startCLIProgress(ctx, prog, "capturing "+name)
	s, err := Capture(ctx, p, tg, opts, prog)
	done()
	if err != nil {
		return err
	}
	if err := saveSnapshot(s); err != nil {
		return err
	}
	up := 0
	for _, pf := range s.Prefixes {
		if pf.Up() {
			up++
		}
	}
	fmt.Printf("%s snapshot %s — %d/%d prefixes reachable\n",
		colGreen.paint("✔ captured"), colWhite.paint(name), up, len(s.Prefixes))
	return nil
}

func cmdDiff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	all := fs.Bool("all", false, "include unchanged prefixes")
	outPath := fs.String("o", "", "also write a plain-text report to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	beforeName, afterName := "before", "after"
	if fs.NArg() >= 2 {
		beforeName, afterName = fs.Arg(0), fs.Arg(1)
	} else if fs.NArg() == 1 {
		return fmt.Errorf("diff needs either no snapshot names or two of them")
	}

	before, err := loadSnapshot(beforeName)
	if err != nil {
		return err
	}
	after, err := loadSnapshot(afterName)
	if err != nil {
		return err
	}
	d := Compare(before, after, DefaultThresholds())
	printDiff(os.Stdout, d, useColor(), *all)

	if *outPath != "" {
		var b strings.Builder
		printDiff(&b, d, false, true)
		if err := os.WriteFile(*outPath, []byte(b.String()), 0o644); err != nil {
			return err
		}
		fmt.Printf("  wrote %s\n", *outPath)
	}
	if d.Regressions() > 0 {
		os.Exit(1)
	}
	_ = ctx
	return nil
}

func cmdRoutes(ctx context.Context) error {
	routes, _, err := fetchRoutes(ctx)
	if err != nil {
		return err
	}
	printRoutes(os.Stdout, routes, useColor())
	return nil
}

func cmdTargets() error {
	t, err := loadTargets()
	if err != nil {
		return err
	}
	printTargets(os.Stdout, t, useColor())
	return nil
}

func cmdList() error {
	snaps, err := listSnapshots()
	if err != nil {
		return err
	}
	printSnapshots(os.Stdout, snaps, useColor())
	return nil
}

// cmdWhere answers "which subnet router would this address go through?", which
// is the question you ask right before and right after changing a route.
func cmdWhere(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("where needs exactly one IP address")
	}
	addr, err := netip.ParseAddr(args[0])
	if err != nil {
		return err
	}
	routes, _, err := fetchRoutes(ctx)
	if err != nil {
		return err
	}
	matches := NewRouteTable(routes).Lookup(addr)
	if len(matches) == 0 {
		fmt.Printf("%s no tailnet route covers %s\n", colYellow.paint("·"), addr)
		return nil
	}
	fmt.Printf("%s %s matches %s\n", colGreen.paint("→"), colWhite.paint(addr.String()),
		colCyan.paint(matches[0].Prefix.String()))
	for _, m := range matches {
		state := colGreen.paint("online")
		if !m.Online {
			state = colRed.paint("offline")
		}
		fmt.Printf("    via %s  %s  %s\n",
			cell(colBlue.paint(m.NextHop.String()), 18),
			cell(colWhite.paint(m.ShortName()), 40), state)
	}
	return nil
}

// cmdPing sweeps the addresses named on the command line. It exists mostly to
// answer "can this host send ICMP at all?" before blaming the network, and to
// spot-check a single destination without disturbing a saved snapshot.
func cmdPing(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ping", flag.ExitOnError)
	count := fs.Int("count", 3, "echo requests per address")
	pps := fs.Int("pps", 200, "send rate ceiling, packets per second")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("ping needs at least one address")
	}
	var addrs []netip.Addr
	for _, a := range fs.Args() {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return fmt.Errorf("bad address %q: %w", a, err)
		}
		addrs = append(addrs, ip)
	}

	p, err := NewPinger()
	if err != nil {
		return err
	}
	defer p.Close()
	fmt.Printf("%s ipv4=%v ipv6=%v raw=%v\n", colGray.paint("sockets:"), p.Has4(), p.Has6(), p.raw4)

	stats, err := p.Sweep(ctx, addrs, SweepConfig{
		Count:    *count,
		PPS:      *pps,
		Timeout:  2 * time.Second,
		Interval: 200 * time.Millisecond,
	}, nil)
	if err != nil {
		return err
	}
	for _, st := range stats {
		fmt.Printf("  %s%s\n", padRight(colCyan.paint(st.Addr.String()), 42), statText(&st))
	}
	return nil
}

// --- shared CLI helpers ----------------------------------------------------

func useColor() bool {
	return supportsColor() && isatty.IsTerminal(os.Stdout.Fd())
}

func parsePrefixes(s string) ([]netip.Prefix, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("bad prefix %q: %w", part, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// startCLIProgress draws a live progress line on stderr and returns a function
// that stops it. On a non-terminal stderr it prints nothing, so piping the
// output stays clean.
func startCLIProgress(ctx context.Context, prog *Progress, what string) func() {
	if !isatty.IsTerminal(os.Stderr.Fd()) || !supportsColor() {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		tick := 0
		for {
			select {
			case <-stop:
				fmt.Fprint(os.Stderr, "\r\x1b[K")
				return
			case <-ctx.Done():
				return
			case <-t.C:
				tick++
				line := fmt.Sprintf("\r\x1b[K %s %s %s %s %s",
					colMagenta.paint(spinner(tick)),
					colWhite.paint(what),
					bar(prog.Frac(), 24),
					colCyan.paintf("%.0f%%", prog.Frac()*100),
					colGray.paint(prog.Label()))
				fmt.Fprint(os.Stderr, line)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
