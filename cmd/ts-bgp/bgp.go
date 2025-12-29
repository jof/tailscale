// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"log"
	"net/netip"

	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
	"google.golang.org/protobuf/types/known/anypb"
)

// BGPServer wraps gobgp server functionality.
type BGPServer struct {
	server   *server.BgpServer
	localAS  uint32
	routerID string

	// OnRouteChange is called when a route is learned or withdrawn.
	// If info is nil, the route was withdrawn.
	OnRouteChange func(prefix netip.Prefix, info *RouteInfo)
}

// NewBGPServer creates and starts a new BGP server.
func NewBGPServer(localAS uint32, routerID, listenAddr string) (*BGPServer, error) {
	s := server.NewBgpServer()
	go s.Serve()

	// Start the BGP server
	if err := s.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        localAS,
			RouterId:   routerID,
			ListenPort: 179, // TODO: parse from listenAddr
		},
	}); err != nil {
		return nil, fmt.Errorf("failed to start BGP: %w", err)
	}

	bs := &BGPServer{
		server:   s,
		localAS:  localAS,
		routerID: routerID,
	}

	// Watch for route changes
	go bs.watchRoutes()

	log.Printf("BGP server started (AS %d, router-id %s)", localAS, routerID)
	return bs, nil
}

// Stop shuts down the BGP server.
func (b *BGPServer) Stop() {
	if b.server != nil {
		b.server.Stop()
	}
}

// PeerOptions contains optional settings for a BGP peer.
type PeerOptions struct {
	Description   string
	MD5Password   string
	HoldTime      uint64
	KeepaliveTime uint64
	PassiveMode   bool
}

// AddPeer adds a BGP peer with default options.
func (b *BGPServer) AddPeer(peerAddr string, peerAS uint32) error {
	return b.AddPeerWithOptions(peerAddr, peerAS, PeerOptions{})
}

// AddPeerWithOptions adds a BGP peer with custom options.
func (b *BGPServer) AddPeerWithOptions(peerAddr string, peerAS uint32, opts PeerOptions) error {
	holdTime := opts.HoldTime
	if holdTime == 0 {
		holdTime = 90
	}
	keepaliveTime := opts.KeepaliveTime
	if keepaliveTime == 0 {
		keepaliveTime = 30
	}

	peer := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress: peerAddr,
			PeerAsn:         peerAS,
			Description:     opts.Description,
		},
		Timers: &api.Timers{
			Config: &api.TimersConfig{
				ConnectRetry:           10,
				HoldTime:               holdTime,
				KeepaliveInterval:      keepaliveTime,
				IdleHoldTimeAfterReset: 30,
			},
		},
		Transport: &api.Transport{
			PassiveMode: opts.PassiveMode,
		},
		AfiSafis: []*api.AfiSafi{
			{
				Config: &api.AfiSafiConfig{
					Family: &api.Family{
						Afi:  api.Family_AFI_IP,
						Safi: api.Family_SAFI_UNICAST,
					},
					Enabled: true,
				},
			},
			{
				Config: &api.AfiSafiConfig{
					Family: &api.Family{
						Afi:  api.Family_AFI_IP6,
						Safi: api.Family_SAFI_UNICAST,
					},
					Enabled: true,
				},
			},
		},
	}

	// Add MD5 authentication if configured
	if opts.MD5Password != "" {
		peer.Conf.AuthPassword = opts.MD5Password
	}

	return b.server.AddPeer(context.Background(), &api.AddPeerRequest{Peer: peer})
}

// AnnounceRoute announces a route to all BGP peers.
func (b *BGPServer) AnnounceRoute(prefix netip.Prefix, nextHopSelf bool) error {
	var family *api.Family
	var nlri *anypb.Any
	var nextHop string

	if prefix.Addr().Is4() {
		family = &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}
		nlri, _ = anypb.New(&api.IPAddressPrefix{
			PrefixLen: uint32(prefix.Bits()),
			Prefix:    prefix.Addr().String(),
		})
		if nextHopSelf {
			nextHop = b.routerID
		} else {
			nextHop = prefix.Addr().String()
		}
	} else {
		family = &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}
		nlri, _ = anypb.New(&api.IPAddressPrefix{
			PrefixLen: uint32(prefix.Bits()),
			Prefix:    prefix.Addr().String(),
		})
		// For IPv6, next-hop is in MP_REACH_NLRI
		nextHop = "::" // Will be set properly via attributes
	}

	origin, _ := anypb.New(&api.OriginAttribute{Origin: 0}) // IGP
	nh, _ := anypb.New(&api.NextHopAttribute{NextHop: nextHop})

	attrs := []*anypb.Any{origin, nh}

	// Add AS_PATH with our AS
	asPath, _ := anypb.New(&api.AsPathAttribute{
		Segments: []*api.AsSegment{
			{
				Type:    api.AsSegment_AS_SEQUENCE,
				Numbers: []uint32{b.localAS},
			},
		},
	})
	attrs = append(attrs, asPath)

	_, err := b.server.AddPath(context.Background(), &api.AddPathRequest{
		TableType: api.TableType_GLOBAL,
		Path: &api.Path{
			Family: family,
			Nlri:   nlri,
			Pattrs: attrs,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to announce route %s: %w", prefix, err)
	}

	log.Printf("BGP: announced %s", prefix)
	return nil
}

// WithdrawRoute withdraws a previously announced route.
func (b *BGPServer) WithdrawRoute(prefix netip.Prefix) error {
	var family *api.Family
	var nlri *anypb.Any

	if prefix.Addr().Is4() {
		family = &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}
		nlri, _ = anypb.New(&api.IPAddressPrefix{
			PrefixLen: uint32(prefix.Bits()),
			Prefix:    prefix.Addr().String(),
		})
	} else {
		family = &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}
		nlri, _ = anypb.New(&api.IPAddressPrefix{
			PrefixLen: uint32(prefix.Bits()),
			Prefix:    prefix.Addr().String(),
		})
	}

	err := b.server.DeletePath(context.Background(), &api.DeletePathRequest{
		TableType: api.TableType_GLOBAL,
		Path: &api.Path{
			Family: family,
			Nlri:   nlri,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to withdraw route %s: %w", prefix, err)
	}

	log.Printf("BGP: withdrew %s", prefix)
	return nil
}

// watchRoutes monitors the BGP RIB for changes from peers.
func (b *BGPServer) watchRoutes() {
	ctx := context.Background()

	err := b.server.WatchEvent(ctx, &api.WatchEventRequest{
		Table: &api.WatchEventRequest_Table{
			Filters: []*api.WatchEventRequest_Table_Filter{
				{
					Type: api.WatchEventRequest_Table_Filter_BEST,
				},
			},
		},
	}, func(r *api.WatchEventResponse) {
		if t := r.GetTable(); t != nil {
			for _, path := range t.Paths {
				b.handlePathChange(path)
			}
		}
	})
	if err != nil {
		log.Printf("BGP watch error: %v", err)
	}
}

// handlePathChange processes a path update from BGP.
func (b *BGPServer) handlePathChange(path *api.Path) {
	if b.OnRouteChange == nil {
		return
	}

	// Skip locally originated routes
	if path.SourceAsn == b.localAS && path.NeighborIp == "" {
		return
	}

	prefix, err := parseNLRI(path.Nlri)
	if err != nil {
		log.Printf("BGP: failed to parse NLRI: %v", err)
		return
	}

	if path.IsWithdraw {
		b.OnRouteChange(prefix, nil)
		return
	}

	info := &RouteInfo{
		Prefix: prefix,
		Source: "bgp",
	}

	// Parse attributes
	for _, attr := range path.Pattrs {
		switch a := attr.MessageName(); a {
		case "gobgp.api.AsPathAttribute":
			var asPath api.AsPathAttribute
			if err := attr.UnmarshalTo(&asPath); err == nil {
				for _, seg := range asPath.Segments {
					info.ASPath = append(info.ASPath, seg.Numbers...)
				}
			}
		case "gobgp.api.NextHopAttribute":
			var nh api.NextHopAttribute
			if err := attr.UnmarshalTo(&nh); err == nil {
				info.NextHop, _ = netip.ParseAddr(nh.NextHop)
			}
		}
	}

	b.OnRouteChange(prefix, info)
}

// parseNLRI extracts the prefix from an NLRI protobuf message.
func parseNLRI(nlri *anypb.Any) (netip.Prefix, error) {
	var ipPrefix api.IPAddressPrefix
	if err := nlri.UnmarshalTo(&ipPrefix); err != nil {
		return netip.Prefix{}, err
	}

	addr, err := netip.ParseAddr(ipPrefix.Prefix)
	if err != nil {
		return netip.Prefix{}, err
	}

	return netip.PrefixFrom(addr, int(ipPrefix.PrefixLen)), nil
}
