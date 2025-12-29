# ts-bgp

A daemon that bridges Tailscale routing information with BGP, enabling dynamic route exchange between your tailnet and traditional network infrastructure.

## Features

- **Export tailnet routes to BGP**: Automatically announces subnet routes from your tailnet to BGP peers
- **Import BGP routes to tailnet**: Learns routes from BGP peers and advertises them into your tailnet
- **Bidirectional sync**: Routes are synchronized in both directions with automatic updates
- **Graceful shutdown**: Withdraws routes before exit to prevent blackholing
- **Metrics/Health**: Prometheus metrics and health endpoints for monitoring
- **MD5 authentication**: Secure BGP sessions with MD5 password authentication

## Requirements

- A running `tailscaled` instance with LocalAPI access
- Appropriate permissions to bind to BGP port (179) or use a non-privileged port
- For importing routes: the node must be approved as a subnet router in Tailscale admin

## Installation

```bash
# Add gobgp dependency
go get github.com/osrg/gobgp/v3@latest

# Build
go build -o ts-bgp ./cmd/ts-bgp
```

## Usage

### Command Line

```bash
# Minimal example
ts-bgp --local-as 65000 --router-id 10.0.0.1

# With config file
ts-bgp --config /etc/ts-bgp/config.yaml

# Dry run (log only, don't modify routes)
ts-bgp --local-as 65000 --router-id 10.0.0.1 --dry-run
```

### Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--config` | Path to YAML config file | `/etc/ts-bgp/config.yaml` |
| `--local-as` | Local AS number (required) | - |
| `--router-id` | BGP router ID (required) | - |
| `--listen` | BGP listen address | `:179` |
| `--metrics` | Metrics/health HTTP listen address | `:9179` |
| `--dry-run` | Don't modify routes, just log | `false` |
| `--export-all` | Export all peer routes (not just primary) | `false` |
| `--next-hop-self` | Set next-hop to self for announced routes | `true` |
| `--shutdown-wait` | Time to wait for route withdrawal on shutdown | `5s` |
| `--log-level` | Log level (debug, info, warn, error) | `info` |

## Configuration File

```yaml
bgp:
  local_as: 65000
  router_id: "10.0.0.1"
  listen_port: 179
  
  # Peers are always eBGP (use different AS than local_as)
  peers:
    - name: "core-router"
      address: "10.0.0.254"
      remote_as: 65001
      md5_password: "secret"  # optional

tailscale:
  export_mode: "primary"  # or "all"
  import_enabled: true
  no_snat: true
  no_stateful_filtering: true
```

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      ts-bgp daemon                          │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ┌─────────────────┐         ┌─────────────────┐           │
│  │  Tailnet Watcher │         │   BGP Server    │           │
│  │                 │         │   (gobgp)       │           │
│  │ WatchIPNBus() ──┼────────►│                 │           │
│  │                 │ export  │ AnnounceRoute() │           │
│  │                 │         │                 │           │
│  │ EditPrefs() ◄───┼─────────┤ OnRouteChange() │           │
│  │                 │ import  │                 │           │
│  └────────┬────────┘         └────────┬────────┘           │
│           │                           │                     │
└───────────┼───────────────────────────┼─────────────────────┘
            │ LocalAPI                  │ BGP (TCP/179)
            ▼                           ▼
     ┌──────────────┐           ┌──────────────┐
     │  tailscaled  │           │  BGP Peers   │
     └──────────────┘           └──────────────┘
```

## Route Flow

### Tailnet → BGP (Export)
1. `ts-bgp` watches the IPN bus for netmap changes
2. When a peer's `PrimaryRoutes` changes, the route is announced/withdrawn via BGP
3. Tailscale IPs (100.64.0.0/10, fd7a::/48) are automatically filtered out

### BGP → Tailnet (Import)
1. BGP peers send route updates
2. Routes are added to `AdvertiseRoutes` via LocalAPI
3. Control plane must approve the routes (unless auto-approve ACLs are configured)

## Tailscale Configuration

For proper transit routing, configure your subnet router node with:

```bash
# Enable subnet routing with proper transit settings
tailscale up --advertise-routes=<initial-routes> \
  --snat-subnet-routes=false \
  --stateful-filtering=false
```

Or via ts-bgp config:
```yaml
tailscale:
  no_snat: true
  no_stateful_filtering: true
```

## Metrics and Health Endpoints

The daemon exposes HTTP endpoints on the metrics address (default `:9179`):

| Endpoint | Description |
|----------|-------------|
| `/health`, `/healthz` | Returns 200 if daemon is running |
| `/ready`, `/readyz` | Returns 200 if connected to tailscaled |
| `/metrics` | Prometheus-format metrics |
| `/status` | JSON status summary |
| `/routes` | JSON list of exported/imported routes |

### Prometheus Metrics

```
tsbgp_uptime_seconds              # Time since daemon started
tsbgp_tailnet_connected           # 1 if connected to tailscaled
tsbgp_routes_exported             # Number of routes exported to BGP
tsbgp_routes_imported             # Number of routes imported from BGP
tsbgp_bgp_updates_received_total  # Total BGP updates received
tsbgp_bgp_updates_sent_total      # Total BGP updates sent
tsbgp_last_netmap_update_timestamp # Unix timestamp of last netmap update
```

## MD5 Authentication

Configure MD5 authentication per-peer in the config file:

```yaml
bgp:
  peers:
    - name: "secure-peer"
      address: "10.0.0.254"
      remote_as: 65001
      md5_password: "shared-secret"
```

For security, avoid committing passwords to version control. Consider using environment variable substitution or a secrets manager.

## Graceful Shutdown

On SIGINT or SIGTERM, the daemon:

1. Withdraws all BGP routes announced to peers
2. Waits for `--shutdown-wait` duration (default 5s) for withdrawals to propagate
3. Removes BGP-learned routes from Tailscale
4. Cleanly shuts down BGP sessions

This prevents traffic blackholing during maintenance.

## Security Considerations

- BGP port 179 requires root or `CAP_NET_BIND_SERVICE`
- Use MD5 authentication for BGP sessions in production
- Consider running in a network namespace for isolation

## Troubleshooting

### Routes not appearing in BGP
- Check `--dry-run` output to see what would be announced
- Verify the node is the primary subnet router for the routes

### Routes not appearing in Tailnet
- Verify routes are approved in Tailscale admin console
- Ensure `import_enabled: true` in config

### BGP session not establishing
- Check firewall allows TCP/179
- Verify AS numbers match peer configuration
- Check router-id is a valid IP address
