package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/shell"
)

// Layout states reported by the base-path probe.
const (
	LayoutNone   = "none"   // nothing deployed yet
	LayoutDepper = "depper" // a depper deployment (<base>/.depper)
	LayoutLegacy = "legacy" // only a Python deployer deployment
	LayoutBoth   = "both"   // both layouts exist
	// LayoutMigrating: a `depper migrate` was interrupted (marker in
	// <base>/.depper); rerunning it finishes the migration.
	LayoutMigrating = "migrating"
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

// ContainerEnv is <base>/.depper/container.env, the --env-file of older
// depper versions (removed by the next deploy; kept in old transaction
// backups so rolling back to such a release still works).
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

// MigrationMarker is the file (inside the metadata directory) that marks a
// `depper migrate` in progress; it moves with the metadata directory.
const MigrationMarker = "migration-in-progress"

// CompatLinkTarget is the relative target of the compatibility symlink that
// `depper migrate` leaves at the Python deployer's default base
// (~/.local/share/deployer/<name> -> ../depper/<name>).
func CompatLinkTarget(cfg *config.Config) string { return "../depper/" + cfg.Service.Name }

// BaseAssignments returns shell words for the depper base and the legacy
// (Python deployer) base of cfg.
func BaseAssignments(cfg *config.Config) (base, legacy string) {
	if p := cfg.Deploy.Path; p != "" {
		q := shell.Quote(p)
		return q, q
	}
	// Service names are validated ([A-Za-z0-9_.-]), safe inside double quotes.
	return `"$HOME/` + cfg.DefaultBaseRel() + `"`, `"$HOME/` + cfg.LegacyDefaultBaseRel() + `"`
}

func (e *Engine) baseAssignments() (base, legacy string) { return BaseAssignments(e.Config) }

// LayoutDetectionLines set $state to none, depper, legacy, both, or
// migrating from $base and $legacy (shared by the probe and doctor):
//
//   - the compatibility symlink `depper migrate` leaves at the legacy default
//     base is not a legacy layout (it points at the depper base);
//   - <base>/.deployer is a legacy layout (a migration that moved the base
//     but did not rename the metadata yet);
//   - a migration marker in <base>/.depper means `depper migrate` was
//     interrupted and must be run again.
func LayoutDetectionLines(cfg *config.Config) []string {
	return []string{
		`compat=no`,
		`if test -L "$legacy" && test "$(readlink "$legacy")" = ` + shell.Quote(CompatLinkTarget(cfg)) + `; then compat=yes; fi`,
		`has_new=no; if test -d "$base/.depper"; then has_new=yes; fi`,
		`has_legacy=no`,
		`if test "$compat" = no && { test -e "$legacy/.deployer" || test -L "$legacy/.deployer"; }; then has_legacy=yes; fi`,
		`if test -e "$base/.deployer" || test -L "$base/.deployer"; then has_legacy=yes; fi`,
		`if test -e "$HOME/.config/systemd/user/"` + shell.Quote(cfg.LegacyUnitName()) + `; then has_legacy=yes; fi`,
		`if test -f "$base/.depper/` + MigrationMarker + `"; then state=migrating`,
		`elif test "$has_new" = yes && test "$has_legacy" = yes; then state=both`,
		`elif test "$has_new" = yes; then state=depper`,
		`elif test "$has_legacy" = yes; then state=legacy`,
		`else state=none; fi`,
	}
}

// probeMarker introduces the probe's key=value output, so anything a login
// shell prints first (motd, rc files) is ignored.
const probeMarker = "@@probe"

// probeScript detects the layout (folding in the legacy check) and, when
// create is set and a plain depper (or empty) layout was found, creates the
// private metadata directories.
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
	}
	lines = append(lines, LayoutDetectionLines(e.Config)...)
	if create {
		lines = append(lines,
			`if test "$state" = none || test "$state" = depper; then`,
			`  mkdir -p "$base/releases" "$base/.depper/transactions"`,
			"  "+refuseSymlink(`"$base/.depper/transactions"`, "deployment transaction directory"),
			"  "+refuseSymlink(`"$base/releases"`, "releases directory"),
			`  chmod 700 "$base/.depper" "$base/.depper/transactions"`,
			`  state=depper`,
			`fi`,
		)
	}
	lines = append(lines,
		`tunnel=no; if test -s "$base/.depper/tunnel.json"; then tunnel=yes; fi`,
		`echo `+probeMarker,
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

// afterMarker returns the output after the first line equal to marker, and
// whether that line was found. Output a login shell printed before the
// script ran is skipped this way.
func afterMarker(out, marker string) (string, bool) {
	if strings.HasPrefix(out, marker+"\n") {
		return out[len(marker)+1:], true
	}
	if i := strings.Index(out, "\n"+marker+"\n"); i >= 0 {
		return out[i+len(marker)+2:], true
	}
	return "", false
}

// FromMarker returns the output after the first line equal to marker, or
// the whole output when there is no such line. Scripts print a marker line
// before their machine-readable output, so whatever a login shell prints
// first (motd, rc files) is skipped.
func FromMarker(out, marker string) string {
	if body, ok := afterMarker(out, marker); ok {
		return body
	}
	return out
}

// parseProbe validates probe output.
func parseProbe(out string, wantEngine bool) (*Layout, error) {
	body := FromMarker(out, probeMarker)
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
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
	if !shell.IsSafePath(l.Base) || strings.HasSuffix(l.Base, "/") {
		return nil, fmt.Errorf("the deployment path on the host (%q) is not supported: depper needs an absolute path made only of %s; set [deploy].path", l.Base, shell.SafePathChars)
	}
	switch l.State {
	case LayoutNone, LayoutDepper, LayoutLegacy, LayoutBoth, LayoutMigrating:
	default:
		return nil, fmt.Errorf("remote host returned an invalid deployment probe")
	}
	if wantEngine && l.EnginePath != "" && !shell.IsPlainPath(l.EnginePath) {
		return nil, fmt.Errorf("the container engine is not installed at a safe absolute path on the deploy host")
	}
	return l, nil
}

// Probe resolves the remote base path and reports the deployment layout,
// for read-only commands. With create (deploy, tunnel), the depper metadata
// directories are created and the probe refuses like ProbeForChange.
//
// A legacy-only layout or an interrupted migration is an error pointing at
// `depper migrate`. When both layouts exist, read-only use proceeds with a
// warning; changes are refused (see ProbeForChange).
func (e *Engine) Probe(ctx context.Context, create bool) (*Layout, error) {
	return e.probe(ctx, create, create)
}

// ProbeForChange is Probe for commands that change the deployment without
// creating it (rollback, secrets push, restart, stop, unlock, tunnel stop):
// it also refuses when both a depper and a legacy layout exist.
func (e *Engine) ProbeForChange(ctx context.Context) (*Layout, error) {
	return e.probe(ctx, false, true)
}

func (e *Engine) probe(ctx context.Context, create, change bool) (*Layout, error) {
	out, err := e.Remote.Capture(ctx, e.probeScript(create))
	if err != nil {
		return nil, err
	}
	l, err := parseProbe(out, e.Config.Container != nil)
	if err != nil {
		return nil, err
	}
	switch l.State {
	case LayoutLegacy:
		return nil, fmt.Errorf("found a deployment made by the Python deployer for %s; run `depper migrate` to convert it", e.Config.Service.Name)
	case LayoutMigrating:
		return nil, fmt.Errorf("a `depper migrate` of %s was interrupted; run `depper migrate` again to finish it", e.Config.Service.Name)
	case LayoutBoth:
		if change {
			return nil, fmt.Errorf("both a depper deployment (%s/.depper) and a Python deployer deployment exist for %s; refusing to change either. Run `depper migrate --dry-run` for details and remove the one you no longer need", l.Base, e.Config.Service.Name)
		}
		e.printf("Warning: both a depper deployment (%s/.depper) and a Python deployer deployment exist for %s; changes are refused until one is removed (see `depper migrate --dry-run`).\n", l.Base, e.Config.Service.Name)
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
