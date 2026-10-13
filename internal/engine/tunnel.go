package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/unit"
)

// TunnelHooks is how deploy drives an owned Cloudflare Tunnel. The tunnel
// package implements it and registers it once with RegisterTunnel.
type TunnelHooks interface {
	// AssertNotOrphaned is called before deploying a configuration without
	// [tunnel] (before the lock is taken). It must fail when the deployment
	// still owns a tunnel (<base>/.depper/tunnel.json), because removing
	// [tunnel] does not delete its DNS route or credentials.
	AssertNotOrphaned(ctx context.Context, e *Engine, l *Layout) error
	// Ensure is called after a successful, healthy deploy of a configuration
	// with [tunnel], while the deploy lock is still held. It creates or
	// verifies the owned tunnel and its connector unit.
	Ensure(ctx context.Context, e *Engine, l *Layout) error
}

var (
	tunnelMu    sync.RWMutex
	tunnelHooks TunnelHooks
)

// RegisterTunnel installs the tunnel implementation (call it once, from the
// tunnel package's init function).
func RegisterTunnel(h TunnelHooks) {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	tunnelHooks = h
}

// RegisteredTunnel returns the registered tunnel implementation, or nil.
func RegisteredTunnel() TunnelHooks {
	tunnelMu.RLock()
	defer tunnelMu.RUnlock()
	return tunnelHooks
}

// errNoTunnelSupport is returned when [tunnel] is configured but no tunnel
// implementation is linked into this binary.
var errNoTunnelSupport = fmt.Errorf("[tunnel] is configured but this depper build has no tunnel support registered; refusing to deploy without managing the tunnel")

// errOrphanedTunnel matches the Python deployer's refusal.
var errOrphanedTunnel = fmt.Errorf("This service owns a public tunnel. Removing [tunnel] does not delete its DNS route or credentials; restore it or clean up the owned tunnel manually.")

// assertTunnelNotOrphaned runs the registered hook, or the built-in check
// based on the probe's tunnel.json detection.
func (e *Engine) assertTunnelNotOrphaned(ctx context.Context, l *Layout) error {
	if e.Config.Tunnel != nil {
		return nil
	}
	if h := RegisteredTunnel(); h != nil {
		return h.AssertNotOrphaned(ctx, e, l)
	}
	if l.TunnelOwned {
		return errOrphanedTunnel
	}
	return nil
}

// TunnelUnitMarker is the Description line that marks a connector unit as
// owned by depper for service name.
func TunnelUnitMarker(name string) string {
	return "Description=depper managed Cloudflare Tunnel: " + name
}

// LegacyTunnelUnitMarker is the Python deployer's marker line.
func LegacyTunnelUnitMarker(name string) string {
	return "Description=deployer managed Cloudflare Tunnel: " + name
}

// TunnelUnitFile renders the cloudflared connector unit
// (depper-tunnel@<name>.service). cloudflared and configPath are absolute
// remote paths; tunnelID is the tunnel UUID.
func TunnelUnitFile(name, cloudflared, configPath, tunnelID string) string {
	return shell.Script(
		"[Unit]",
		TunnelUnitMarker(name),
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		"ExecStart="+unit.Arg(cloudflared)+" --no-autoupdate --config "+unit.Arg(configPath)+" tunnel run "+tunnelID,
		"Restart=always",
		"RestartSec=3",
		"",
		"[Install]",
		"WantedBy=default.target",
	)
}

type cloudflaredIngress struct {
	Hostname string `json:"hostname,omitempty"`
	Service  string `json:"service"`
}

type cloudflaredConfig struct {
	Tunnel          string               `json:"tunnel"`
	CredentialsFile string               `json:"credentials-file"`
	Ingress         []cloudflaredIngress `json:"ingress"`
}

// CloudflaredConfig renders cloudflared.yml as JSON (valid YAML), exactly as
// the Python deployer did: credentials in <home>/.cloudflared/<id>.json and
// a catch-all 404 rule.
func CloudflaredConfig(tunnelID, home, hostname, service string) string {
	data, _ := json.MarshalIndent(cloudflaredConfig{
		Tunnel:          tunnelID,
		CredentialsFile: home + "/.cloudflared/" + tunnelID + ".json",
		Ingress:         []cloudflaredIngress{{Hostname: hostname, Service: service}, {Service: "http_status:404"}},
	}, "", "  ")
	return string(data) + "\n"
}
