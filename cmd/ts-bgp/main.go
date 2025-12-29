// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// ts-bgp is a daemon that bridges Tailscale routing information with BGP.
// It watches the tailnet for route changes and announces them to BGP peers,
// and learns routes from BGP peers to advertise into the tailnet.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/types/netmap"
)

var (
	configFile   = flag.String("config", "/etc/ts-bgp/config.yaml", "path to config file")
	localAS      = flag.Uint("local-as", 0, "local AS number (required)")
	routerID     = flag.String("router-id", "", "BGP router ID (required)")
	listenAddr   = flag.String("listen", ":179", "BGP listen address")
	metricsAddr  = flag.String("metrics", ":9179", "metrics/health HTTP listen address")
	logLevel     = flag.String("log-level", "info", "log level (debug, info, warn, error)")
	dryRun       = flag.Bool("dry-run", false, "don't actually modify routes, just log")
	exportAll    = flag.Bool("export-all", false, "export all tailnet routes (not just primary routes)")
	importTag    = flag.String("import-tag", "", "only import routes with this BGP community tag")
	nextHopSelf  = flag.Bool("next-hop-self", true, "set next-hop to self for announced routes")
	shutdownWait = flag.Duration("shutdown-wait", 5*time.Second, "time to wait for route withdrawal on shutdown")
)

func main() {
	flag.Parse()

	if *localAS == 0 {
		log.Fatal("--local-as is required")
	}
	if *routerID == "" {
		log.Fatal("--router-id is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
	}()

	daemon := &Daemon{
		localAS:      uint32(*localAS),
		routerID:     *routerID,
		listenAddr:   *listenAddr,
		metricsAddr:  *metricsAddr,
		shutdownWait: *shutdownWait,
		dryRun:       *dryRun,
		exportAll:    *exportAll,
		nextHopSelf:  *nextHopSelf,
		lc:           &local.Client{},
		startTime:    time.Now(),

		tailnetRoutes: make(map[netip.Prefix]RouteInfo),
		bgpRoutes:     make(map[netip.Prefix]RouteInfo),
	}

	if err := daemon.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("daemon error: %v", err)
	}
}

// RouteInfo contains metadata about a route.
type RouteInfo struct {
	Prefix    netip.Prefix
	NextHop   netip.Addr
	Source    string // "tailnet" or "bgp"
	NodeName  string // for tailnet routes, the node advertising it
	ASPath    []uint32
	Community []uint32
}

// Daemon is the main ts-bgp daemon.
type Daemon struct {
	localAS      uint32
	routerID     string
	listenAddr   string
	metricsAddr  string
	shutdownWait time.Duration
	dryRun       bool
	exportAll    bool
	nextHopSelf  bool
	lc           *local.Client

	mu            sync.RWMutex
	tailnetRoutes map[netip.Prefix]RouteInfo
	bgpRoutes     map[netip.Prefix]RouteInfo

	bgpServer *BGPServer

	// Metrics
	startTime            time.Time
	tailnetConnected     atomic.Bool
	routesExported       atomic.Int64
	routesImported       atomic.Int64
	bgpUpdatesReceived   atomic.Int64
	bgpUpdatesSent       atomic.Int64
	lastNetmapUpdate     atomic.Int64 // unix timestamp
}

// Run starts the daemon and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	log.Printf("Starting ts-bgp daemon (AS %d, router-id %s)", d.localAS, d.routerID)

	// Start BGP server
	var err error
	d.bgpServer, err = NewBGPServer(d.localAS, d.routerID, d.listenAddr)
	if err != nil {
		return err
	}

	// Set up route change callback from BGP
	d.bgpServer.OnRouteChange = d.handleBGPRouteChange

	// Start metrics/health HTTP server
	httpServer := d.startMetricsServer()

	var wg sync.WaitGroup

	// Watch tailnet for route changes
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.watchTailnet(ctx)
	}()

	// Periodic reconciliation
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.reconcileLoop(ctx)
	}()

	// Wait for context cancellation
	<-ctx.Done()

	// Graceful shutdown
	d.shutdown(httpServer)

	// Wait for goroutines to finish
	wg.Wait()
	return nil
}

// watchTailnet watches the IPN bus for netmap changes.
func (d *Daemon) watchTailnet(ctx context.Context) {
	for {
		if err := d.doWatchTailnet(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("tailnet watch error (retrying): %v", err)
		}
	}
}

func (d *Daemon) doWatchTailnet(ctx context.Context) error {
	// Request initial netmap and subscribe to updates
	mask := ipn.NotifyInitialNetMap
	watcher, err := d.lc.WatchIPNBus(ctx, mask)
	if err != nil {
		return err
	}
	defer watcher.Close()

	log.Println("Connected to tailscaled, watching for route changes")

	for {
		n, err := watcher.Next()
		if err != nil {
			return err
		}

		if n.NetMap != nil {
			d.processNetMap(n.NetMap)
		}
	}
}

// processNetMap extracts routes from the netmap and updates BGP.
func (d *Daemon) processNetMap(nm *netmap.NetworkMap) {
	newRoutes := make(map[netip.Prefix]RouteInfo)

	for _, peer := range nm.Peers {
		var routes []netip.Prefix
		if d.exportAll {
			// Export all AllowedIPs
			for i := range peer.AllowedIPs().Len() {
				routes = append(routes, peer.AllowedIPs().At(i))
			}
		} else {
			// Only export PrimaryRoutes (subnets this node is primary for)
			for i := range peer.PrimaryRoutes().Len() {
				routes = append(routes, peer.PrimaryRoutes().At(i))
			}
		}

		for _, prefix := range routes {
			// Skip Tailscale IPs (100.x.x.x/32, fd7a::/128)
			if isTailscaleIP(prefix) {
				continue
			}

			newRoutes[prefix] = RouteInfo{
				Prefix:   prefix,
				Source:   "tailnet",
				NodeName: peer.Name(),
			}
		}
	}

	// Diff and update
	d.mu.Lock()
	defer d.mu.Unlock()

	// Find removed routes
	for prefix := range d.tailnetRoutes {
		if _, exists := newRoutes[prefix]; !exists {
			log.Printf("Tailnet route removed: %s", prefix)
			if !d.dryRun {
				d.bgpServer.WithdrawRoute(prefix)
			}
		}
	}

	// Find added/changed routes
	for prefix, info := range newRoutes {
		if existing, exists := d.tailnetRoutes[prefix]; !exists || existing.NodeName != info.NodeName {
			log.Printf("Tailnet route added/changed: %s via %s", prefix, info.NodeName)
			if !d.dryRun {
				d.bgpServer.AnnounceRoute(prefix, d.nextHopSelf)
			}
		}
	}

	d.tailnetRoutes = newRoutes
}

// handleBGPRouteChange is called when BGP learns or withdraws a route.
func (d *Daemon) handleBGPRouteChange(prefix netip.Prefix, info *RouteInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if info == nil {
		// Route withdrawn
		delete(d.bgpRoutes, prefix)
		log.Printf("BGP route withdrawn: %s", prefix)
		d.updateTailscaleRoutes()
	} else {
		// Route learned
		d.bgpRoutes[prefix] = *info
		log.Printf("BGP route learned: %s (AS path: %v)", prefix, info.ASPath)
		d.updateTailscaleRoutes()
	}
}

// updateTailscaleRoutes updates the advertised routes in tailscale.
// Caller must NOT hold d.mu.
func (d *Daemon) updateTailscaleRoutes() {
	d.updateTailscaleRoutesLocked()
}

// updateTailscaleRoutesLocked updates the advertised routes in tailscale.
// Caller must hold d.mu (read or write).
func (d *Daemon) updateTailscaleRoutesLocked() {
	if d.dryRun {
		return
	}

	// Get current prefs
	prefs, err := d.lc.GetPrefs(context.Background())
	if err != nil {
		log.Printf("Failed to get prefs: %v", err)
		return
	}

	// Build new route list: existing non-BGP routes + BGP routes
	var newRoutes []netip.Prefix

	// Keep routes that weren't learned from BGP
	for _, r := range prefs.AdvertiseRoutes {
		if _, isBGP := d.bgpRoutes[r]; !isBGP {
			newRoutes = append(newRoutes, r)
		}
	}

	// Add BGP-learned routes
	for prefix := range d.bgpRoutes {
		newRoutes = append(newRoutes, prefix)
	}

	// Update prefs
	_, err = d.lc.EditPrefs(context.Background(), &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			AdvertiseRoutes: newRoutes,
		},
		AdvertiseRoutesSet: true,
	})
	if err != nil {
		log.Printf("Failed to update advertised routes: %v", err)
	}
}

// reconcileLoop periodically reconciles routes.
func (d *Daemon) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Periodic health check / reconciliation
			d.mu.RLock()
			exported := len(d.tailnetRoutes)
			imported := len(d.bgpRoutes)
			d.mu.RUnlock()
			d.routesExported.Store(int64(exported))
			d.routesImported.Store(int64(imported))
		}
	}
}

// shutdown performs graceful shutdown with route withdrawal.
func (d *Daemon) shutdown(httpServer *http.Server) {
	log.Println("Starting graceful shutdown...")

	// Withdraw all BGP routes we announced
	d.mu.RLock()
	routesToWithdraw := make([]netip.Prefix, 0, len(d.tailnetRoutes))
	for prefix := range d.tailnetRoutes {
		routesToWithdraw = append(routesToWithdraw, prefix)
	}
	d.mu.RUnlock()

	if !d.dryRun && len(routesToWithdraw) > 0 {
		log.Printf("Withdrawing %d routes from BGP...", len(routesToWithdraw))
		for _, prefix := range routesToWithdraw {
			if err := d.bgpServer.WithdrawRoute(prefix); err != nil {
				log.Printf("Failed to withdraw %s: %v", prefix, err)
			}
		}

		// Wait for withdrawals to propagate
		log.Printf("Waiting %v for route withdrawals to propagate...", d.shutdownWait)
		time.Sleep(d.shutdownWait)
	}

	// Remove BGP-learned routes from Tailscale
	d.mu.Lock()
	if len(d.bgpRoutes) > 0 {
		log.Printf("Removing %d BGP-learned routes from Tailscale...", len(d.bgpRoutes))
		d.bgpRoutes = make(map[netip.Prefix]RouteInfo)
		d.updateTailscaleRoutesLocked()
	}
	d.mu.Unlock()

	// Stop HTTP server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	// Stop BGP server
	d.bgpServer.Stop()
	log.Println("Shutdown complete")
}

// startMetricsServer starts the HTTP server for metrics and health endpoints.
func (d *Daemon) startMetricsServer() *http.Server {
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/health", d.handleHealth)
	mux.HandleFunc("/healthz", d.handleHealth)

	// Ready check (are we connected to tailscaled?)
	mux.HandleFunc("/ready", d.handleReady)
	mux.HandleFunc("/readyz", d.handleReady)

	// Metrics endpoint (Prometheus format)
	mux.HandleFunc("/metrics", d.handleMetrics)

	// Status endpoint (JSON)
	mux.HandleFunc("/status", d.handleStatus)

	// Routes endpoint (JSON)
	mux.HandleFunc("/routes", d.handleRoutes)

	server := &http.Server{
		Addr:    d.metricsAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("Metrics server listening on %s", d.metricsAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Metrics server error: %v", err)
		}
	}()

	return server
}

// handleHealth returns 200 if the daemon is running.
func (d *Daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

// handleReady returns 200 if connected to tailscaled.
func (d *Daemon) handleReady(w http.ResponseWriter, r *http.Request) {
	if d.tailnetConnected.Load() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready\n"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("not connected to tailscaled\n"))
	}
}

// handleMetrics returns Prometheus-format metrics.
func (d *Daemon) handleMetrics(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	exported := len(d.tailnetRoutes)
	imported := len(d.bgpRoutes)
	d.mu.RUnlock()

	uptime := time.Since(d.startTime).Seconds()
	lastUpdate := d.lastNetmapUpdate.Load()
	connected := 0
	if d.tailnetConnected.Load() {
		connected = 1
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP tsbgp_uptime_seconds Time since daemon started\n")
	fmt.Fprintf(w, "# TYPE tsbgp_uptime_seconds gauge\n")
	fmt.Fprintf(w, "tsbgp_uptime_seconds %f\n", uptime)

	fmt.Fprintf(w, "# HELP tsbgp_tailnet_connected Whether connected to tailscaled\n")
	fmt.Fprintf(w, "# TYPE tsbgp_tailnet_connected gauge\n")
	fmt.Fprintf(w, "tsbgp_tailnet_connected %d\n", connected)

	fmt.Fprintf(w, "# HELP tsbgp_routes_exported Number of routes exported to BGP\n")
	fmt.Fprintf(w, "# TYPE tsbgp_routes_exported gauge\n")
	fmt.Fprintf(w, "tsbgp_routes_exported %d\n", exported)

	fmt.Fprintf(w, "# HELP tsbgp_routes_imported Number of routes imported from BGP\n")
	fmt.Fprintf(w, "# TYPE tsbgp_routes_imported gauge\n")
	fmt.Fprintf(w, "tsbgp_routes_imported %d\n", imported)

	fmt.Fprintf(w, "# HELP tsbgp_bgp_updates_received_total Total BGP updates received\n")
	fmt.Fprintf(w, "# TYPE tsbgp_bgp_updates_received_total counter\n")
	fmt.Fprintf(w, "tsbgp_bgp_updates_received_total %d\n", d.bgpUpdatesReceived.Load())

	fmt.Fprintf(w, "# HELP tsbgp_bgp_updates_sent_total Total BGP updates sent\n")
	fmt.Fprintf(w, "# TYPE tsbgp_bgp_updates_sent_total counter\n")
	fmt.Fprintf(w, "tsbgp_bgp_updates_sent_total %d\n", d.bgpUpdatesSent.Load())

	fmt.Fprintf(w, "# HELP tsbgp_last_netmap_update_timestamp Unix timestamp of last netmap update\n")
	fmt.Fprintf(w, "# TYPE tsbgp_last_netmap_update_timestamp gauge\n")
	fmt.Fprintf(w, "tsbgp_last_netmap_update_timestamp %d\n", lastUpdate)
}

// StatusResponse is the JSON response for /status.
type StatusResponse struct {
	Uptime         string `json:"uptime"`
	LocalAS        uint32 `json:"local_as"`
	RouterID       string `json:"router_id"`
	TailnetConnected bool   `json:"tailnet_connected"`
	RoutesExported int    `json:"routes_exported"`
	RoutesImported int    `json:"routes_imported"`
	BGPUpdatesSent int64  `json:"bgp_updates_sent"`
	BGPUpdatesRecv int64  `json:"bgp_updates_received"`
}

// handleStatus returns daemon status as JSON.
func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	exported := len(d.tailnetRoutes)
	imported := len(d.bgpRoutes)
	d.mu.RUnlock()

	status := StatusResponse{
		Uptime:           time.Since(d.startTime).Round(time.Second).String(),
		LocalAS:          d.localAS,
		RouterID:         d.routerID,
		TailnetConnected: d.tailnetConnected.Load(),
		RoutesExported:   exported,
		RoutesImported:   imported,
		BGPUpdatesSent:   d.bgpUpdatesSent.Load(),
		BGPUpdatesRecv:   d.bgpUpdatesReceived.Load(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// RoutesResponse is the JSON response for /routes.
type RoutesResponse struct {
	Exported []RouteInfo `json:"exported"`
	Imported []RouteInfo `json:"imported"`
}

// handleRoutes returns current routes as JSON.
func (d *Daemon) handleRoutes(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	resp := RoutesResponse{
		Exported: make([]RouteInfo, 0, len(d.tailnetRoutes)),
		Imported: make([]RouteInfo, 0, len(d.bgpRoutes)),
	}

	for _, info := range d.tailnetRoutes {
		resp.Exported = append(resp.Exported, info)
	}
	for _, info := range d.bgpRoutes {
		resp.Imported = append(resp.Imported, info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// isTailscaleIP returns true if the prefix is a Tailscale-assigned IP.
func isTailscaleIP(p netip.Prefix) bool {
	addr := p.Addr()
	// 100.64.0.0/10 (CGNAT range used by Tailscale)
	if addr.Is4() {
		a := addr.As4()
		if a[0] == 100 && (a[1]&0xC0) == 64 {
			return true
		}
	}
	// fd7a:115c:a1e0::/48 (Tailscale IPv6 ULA)
	if addr.Is6() {
		a := addr.As16()
		if a[0] == 0xfd && a[1] == 0x7a && a[2] == 0x11 && a[3] == 0x5c {
			return true
		}
	}
	return false
}
