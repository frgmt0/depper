package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/frgmt0/depper/internal/shell"
)

// Layout states reported by the base-path probe.
const (
	LayoutNone   = "none"   // nothing deployed yet
	LayoutDepper = "depper" // a depper deployment (<base>/.depper)
	LayoutLegacy = "legacy" // only a Python deployer deployment
	LayoutBoth   = "both"   // both layouts exist
)

// Layout is the resolved remote deployment directory and what the probe
// found there. Paths are absolute remote paths.
type Layout struct {
	// Base is the deployment base directory.
	Base string
	// Home is the remote $HOME.
	Home string
	// State is one of the Layout* constants.
	State string
	// TunnelOwned reports a non-empty <base>/.depper/tunnel.json.
	TunnelOwned bool
	// EnginePath is the absolute path of the container engine (container
	// mode only; "" when it is not installed).
	EnginePath string
}

// Meta is <base>/.depper.
func (l *Layout) Meta() string { return l.Base + "/.depper" }

// Releases is <base>/releases.
func (l *Layout) Releases() string { return l.Base + "/releases" }

// Release is <base>/releases/<id>.
func (l *Layout) Release(id string) string { return l.Releases() + "/" + id }

// Transactions is <base>/.depper/transactions.
func (l *Layout) Transactions() string { return l.Meta() + "/transactions" }

// Transaction is <base>/.depper/transactions/<id>.
func (l *Layout) Transaction(id string) string { return l.Transactions() + "/" + id }

// Environment is the systemd EnvironmentFile: <base>/.depper/environment.
func (l *Layout) Environment() string { return l.Meta() + "/environment" }

// Manifest is <base>/.depper/environment.manifest.
func (l *Layout) Manifest() string { return l.Meta() + "/environment.manifest" }

// Salt is <base>/.depper/fingerprint.salt.
func (l *Layout) Salt() string { return l.Meta() + "/fingerprint.salt" }

// ContainerEnv is <base>/.depper/container.env (docker --env-file format).
func (l *Layout) ContainerEnv() string { return l.Meta() + "/container.env" }

// Lock is the lock directory <base>/.depper/lock.
func (l *Layout) Lock() string { return l.Meta() + "/lock" }

// TunnelState is <base>/.depper/tunnel.json.
func (l *Layout) TunnelState() string { return l.Meta() + "/tunnel.json" }

// TunnelConfig is <base>/.depper/cloudflared.yml.
func (l *Layout) TunnelConfig() string { return l.Meta() + "/cloudflared.yml" }

// Current is the <base>/current symlink.
func (l *Layout) Current() string { return l.Base + "/current" }

// Previous is the <base>/previous symlink.
func (l *Layout) Previous() string { return l.Base + "/previous" }

// Deployed reports whether a depper deployment exists.
func (l *Layout) Deployed() bool { return l.State == LayoutDepper || l.State == LayoutBoth }

// baseAssignments returns shell assignments for the depper base and the
// legacy (Python deployer) base.
func (e *Engine) baseAssignments() (base, legacy string) {
	if p := e.Config.Deploy.Path; p != "" {
		q := shell.Quote(p)
		return q, q
	}
	// Service names are validated ([A-Za-z0-9_.-]), safe inside double quotes.
	return `"$HOME/` + e.Config.DefaultBaseRel() + `"`, `"$HOME/` + e.Config.LegacyDefaultBaseRel() + `"`
}

// probeScript detects the layout (folding in the legacy check) and, when
// create is set and no legacy-only layout was found, creates the private
// metadata directories.
func (e *Engine) probeScript(create bool) string {
	base, legacy := e.baseAssignments()
	lines := []string{
		"set +xv",
		"set -eu",
		"umask 077",
		"base=" + base,
		"legacy=" + legacy,
		`home=$HOME`,
		refuseSymlink(`"$base/.depper"`, "deployment metadata directory"),
		`has_new=no; if test -d "$base/.depper"; then has_new=yes; fi`,
		`has_legacy=no`,
		`if test -e "$legacy/.deployer" || test -L "$legacy/.deployer"; then has_legacy=yes; fi`,
		`if test -e "$HOME/.config/systemd/user/"` + shell.Quote(e.Config.LegacyUnitName()) + `; then has_legacy=yes; fi`,
		`if test "$has_new" = yes && test "$has_legacy" = yes; then state=both`,
		`elif test "$has_new" = yes; then state=depper`,
		`elif test "$has_legacy" = yes; then state=legacy`,
		`else state=none; fi`,
	}
	if create {
		lines = append(lines,
			`if test "$state" != legacy; then`,
			`  mkdir -p "$base/releases" "$base/.depper/transactions"`,
			"  "+refuseSymlink(`"$base/.depper/transactions"`, "deployment transaction directory"),
			"  "+refuseSymlink(`"$base/releases"`, "releases directory"),
			`  chmod 700 "$base/.depper" "$base/.depper/transactions"`,
			`  if test "$state" = none; then state=depper; fi`,
			`fi`,
		)
	}
	lines = append(lines,
		`tunnel=no; if test -s "$base/.depper/tunnel.json"; then tunnel=yes; fi`,
		`printf 'base=%s\n' "$base"`,
		`printf 'home=%s\n' "$home"`,
		`printf 'layout=%s\n' "$state"`,
		`printf 'tunnel=%s\n' "$tunnel"`,
	)
	if c := e.Config.Container; c != nil {
		lines = append(lines, `printf 'engine=%s\n' "$(command -v `+shell.Quote(c.Engine)+` 2>/dev/null || true)"`)
	}
	return shell.Script(lines...)
}

// parseProbe validates probe output.
func parseProbe(out string, wantEngine bool) (*Layout, error) {
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("remote host returned an invalid deployment probe")
		}
		if _, dup := values[key]; dup {
			return nil, fmt.Errorf("remote host returned an invalid deployment probe")
		}
		values[key] = value
	}
	l := &Layout{Base: values["base"], Home: values["home"], State: values["layout"], TunnelOwned: values["tunnel"] == "yes", EnginePath: values["engine"]}
	if !shell.IsPlainPath(l.Base) || strings.HasSuffix(l.Base, "/") {
		return nil, fmt.Errorf("remote host returned an invalid deployment path")
	}
	switch l.State {
	case LayoutNone, LayoutDepper, LayoutLegacy, LayoutBoth:
	default:
		return nil, fmt.Errorf("remote host returned an invalid deployment probe")
	}
	if wantEngine && l.EnginePath != "" && !shell.IsPlainPath(l.EnginePath) {
		return nil, fmt.Errorf("the container engine is not installed at a safe absolute path on the deploy host")
	}
	return l, nil
}

// Probe resolves the remote base path and reports the deployment layout.
// With create, the depper metadata directories are created (never when only
// a legacy layout exists). A legacy-only layout is an error pointing at
// `depper migrate`.
func (e *Engine) Probe(ctx context.Context, create bool) (*Layout, error) {
	out, err := e.Remote.Capture(ctx, e.probeScript(create))
	if err != nil {
		return nil, err
	}
	l, err := parseProbe(out, e.Config.Container != nil)
	if err != nil {
		return nil, err
	}
	if l.State == LayoutLegacy {
		return nil, fmt.Errorf("found a deployment made by the Python deployer for %s; run `depper migrate` to convert it", e.Config.Service.Name)
	}
	return l, nil
}

// requireEngine fails when the configured container engine is missing.
func (e *Engine) requireEngine(l *Layout) error {
	if e.Config.Container == nil || l.EnginePath != "" {
		return nil
	}
	return fmt.Errorf("%s is not installed on the deploy host (required by [container])", e.Config.Container.Engine)
}
