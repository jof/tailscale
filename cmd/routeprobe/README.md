# routeprobe

Before/after routing diffs for your tailnet.

`tailscale route-table` tells you what your tailnet *claims* the routing is.
`routeprobe` tells you what actually changed when you touched it: it finds
real, pingable destinations inside every routed prefix, measures them, lets you
make your change, measures them again, and reports what broke.

```
go run ./cmd/routeprobe
```

## The workflow

```
routeprobe discover     # sweep every routed prefix for hosts that answer ICMP
routeprobe before       # measure them all
  ... change a route, move a subnet router, flip an exit node ...
routeprobe after        # measure them again
routeprobe diff         # what changed
```

The interactive TUI (`routeprobe` with no arguments) drives the same five steps
with live progress, a scrollable comparison, and per-prefix drill-down. Every
step is also a subcommand, so the flow scripts cleanly — `routeprobe diff`
exits non-zero when it finds regressions, which makes it usable as a
post-change gate.

## How it finds targets

Discovery is the interesting half. For each prefix in the route table:

- **/32 and /31** — probe the address(es) directly.
- **Anything that fits the budget** (a /24 is 254 usable addresses) — sweep it
  exhaustively.
- **Anything larger** — sample. Walk the /24s inside the prefix, striding when
  there are more of them than the budget allows, and inside each one try the
  addresses operators actually assign: `.1`, `.254`, `.10`, `.100` first, then
  a stride-spread of the rest. Leftover budget is spent on more addresses per
  subnet rather than being dropped, so a /21 gets 256 probes and not 32.
- **Default routes** (`0.0.0.0/0`, `::/0`) — can't be swept, so they're tested
  against public anycast resolvers. Either your exit node carries you to the
  internet or it doesn't.

Every candidate is attributed to the *most specific* prefix that contains it,
so a /16 and a /24 carved out of it don't end up testing each other's path.
Sampling is deterministic: re-running discovery probes the same addresses.

The fastest few responders per prefix are kept as that prefix's fixed target
list. Both captures probe that same list — which is the whole reason the
comparison means anything.

## The sweeper

Pure Go ICMP, no external scanner. One socket per address family for the entire
run, with probes paced to a packets-per-second ceiling and attributed by a
magic-tagged payload rather than by ICMP sequence number — so a 20,000-address
sweep costs a few kilobytes and won't pick up replies meant for some other ping
on the box.

It prefers unprivileged ICMP datagram sockets, which work as your own user on
macOS and on Linux for GIDs inside `net.ipv4.ping_group_range`, and falls back
to raw sockets when it must. If neither is available it says so and tells you
how to fix it. `routeprobe ping 1.1.1.1` is the quickest way to check.

ICMP *unreachable* and *TTL exceeded* replies are recorded too, recovered from
the IP header the router quotes back. "This destination started returning
unreachable" is a routing hole; "this destination went silent" might just be a
firewall. The diff distinguishes them.

## Reading the diff

Verdicts are ordered worst-first, so the top of the list is what to look at:

| | | |
|---|---|---|
| `✖ BROKEN` | answered before, silent after | the thing you're looking for |
| `⊘ WITHDRAWN` | no peer advertises the prefix any more | |
| `▼ LOSSY` | reachable, dropping meaningfully more | |
| `⇄ REROUTED` | reachable, but via a different peer | |
| `↑ SLOWER` | latency up past both thresholds | |
| `✔ FIXED` `↓ FASTER` `▲ CLEANER` | it got better | |
| `· DOWN` | unreachable in both captures | probably firewalled, not broken |

Latency has to move by both a relative *and* an absolute amount to be reported,
so a 0.4ms LAN hop drifting to 0.6ms doesn't show up as "+50% slower".

The route table is diffed separately as well, covering prefixes that had
nothing pingable in them — a route can change without any measurable effect,
and that's still worth knowing.

## Other commands

```
routeprobe routes           the tailnet route table
routeprobe targets          the discovered target set
routeprobe list             saved snapshots
routeprobe where 10.1.2.3   which subnet router that address would go through
routeprobe ping ADDR...     sweep specific addresses
```

Snapshots live in `$ROUTEPROBE_DIR`, or the OS cache directory if that's unset.
