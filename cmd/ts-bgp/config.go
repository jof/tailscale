// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"net/netip"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the ts-bgp configuration file format.
type Config struct {
	// BGP configuration
	BGP BGPConfig `yaml:"bgp"`

	// Tailscale configuration
	Tailscale TailscaleConfig `yaml:"tailscale"`
}

// BGPConfig contains BGP-specific settings.
type BGPConfig struct {
	LocalAS    uint32 `yaml:"local_as"`
	RouterID   string `yaml:"router_id"`
	ListenAddr string `yaml:"listen_addr"`
	ListenPort int    `yaml:"listen_port"`

	// Peers is a list of BGP neighbors.
	Peers []PeerConfig `yaml:"peers"`
}

// PeerConfig defines a BGP peer.
type PeerConfig struct {
	Name        string `yaml:"name"`
	Address     string `yaml:"address"`
	RemoteAS    uint32 `yaml:"remote_as"`
	Description string `yaml:"description"`

	// Timers (optional, defaults provided)
	HoldTime      int `yaml:"hold_time"`
	KeepaliveTime int `yaml:"keepalive_time"`

	// MD5 authentication password (optional)
	// For security, consider using environment variable substitution
	MD5Password string `yaml:"md5_password"`

	// PassiveMode waits for the peer to initiate the connection
	PassiveMode bool `yaml:"passive_mode"`

	// Import/export policy names
	ImportPolicy string `yaml:"import_policy"`
	ExportPolicy string `yaml:"export_policy"`
}

// TailscaleConfig contains Tailscale-specific settings.
type TailscaleConfig struct {
	// Socket path for LocalAPI (optional, uses default if empty)
	Socket string `yaml:"socket"`

	// ExportMode controls which routes to export to BGP
	// "primary" - only routes where this node is primary (default)
	// "all" - all AllowedIPs from peers
	ExportMode string `yaml:"export_mode"`

	// ImportEnabled allows learning routes from BGP
	ImportEnabled bool `yaml:"import_enabled"`

	// NoSNAT disables source NAT for imported routes
	NoSNAT bool `yaml:"no_snat"`

	// NoStatefulFiltering disables stateful filtering for imported routes
	NoStatefulFiltering bool `yaml:"no_stateful_filtering"`
}


// LoadConfig reads and parses a config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

// Validate checks the config for errors.
func (c *Config) Validate() error {
	if c.BGP.LocalAS == 0 {
		return fmt.Errorf("bgp.local_as is required")
	}
	if c.BGP.RouterID == "" {
		return fmt.Errorf("bgp.router_id is required")
	}
	if _, err := netip.ParseAddr(c.BGP.RouterID); err != nil {
		return fmt.Errorf("bgp.router_id must be a valid IP address: %w", err)
	}

	for i, peer := range c.BGP.Peers {
		if peer.Address == "" {
			return fmt.Errorf("bgp.peers[%d].address is required", i)
		}
		if peer.RemoteAS == 0 {
			return fmt.Errorf("bgp.peers[%d].remote_as is required", i)
		}
	}

	switch c.Tailscale.ExportMode {
	case "", "primary", "all":
		// valid
	default:
		return fmt.Errorf("tailscale.export_mode must be 'primary' or 'all'")
	}

	return nil
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		BGP: BGPConfig{
			ListenPort: 179,
		},
		Tailscale: TailscaleConfig{
			ExportMode:    "primary",
			ImportEnabled: true,
		},
	}
}
