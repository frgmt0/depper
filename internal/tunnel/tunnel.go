// Package tunnel manages the optional, locally configured Cloudflare Tunnel
// of a depper deployment: one dedicated tunnel per service, recorded in the
// deployment's private state file (<base>/.depper/tunnel.json), routed to
// exactly one hostname, and run by its own systemd user unit
// (depper-tunnel@<name>.service).
//
// depper only ever modifies the tunnel recorded in that state file. It
// refuses to adopt a pre-existing tunnel, to overwrite DNS, to change the
// hostname or origin of a live route, and to replace a connector unit it did
// not write. The remote host needs no Python: the DNS preflight runs locally
// and cloudflared's JSON output is parsed locally.
//
// The package registers itself with the engine (engine.RegisterTunnel) from
// its init function, so linking it in enables [tunnel] for deploys.
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/frgmt0/depper/internal/engine"
	"github.com/frgmt0/depper/internal/shell"
)

// Default is the implementation registered with the engine.
var Default = &Manager{}

func init() { engine.RegisterTunnel(Default) }

// Connection wait defaults: the connector gets ConnectSettle after its
// restart, then up to ConnectAttempts checks of `cloudflared tunnel list`,
// ConnectInterval apart (the Python deployer's `sleep 2` and 5 tries).
const (
	DefaultConnectSettle   = 2 * time.Second
	DefaultConnectAttempts = 5
	DefaultConnectInterval = 2 * time.Second
)

// maxStateBytes bounds how much of tunnel.json is read.
const maxStateBytes = 65536

// Manager implements engine.TunnelHooks and the tunnel commands. The zero
// value is ready to use.
type Manager struct {
	// LookupHost resolves a hostname for the DNS preflight; nil means
	// net.DefaultResolver.LookupHost.
	LookupHost func(ctx context.Context, host string) ([]string, error)
	// ConnectSettle, ConnectAttempts, and ConnectInterval tune the wait for
	// the connector's Cloudflare connection; zero values use the defaults.
	// Waiting uses the engine's (injectable) Sleep.
	ConnectSettle   time.Duration
	ConnectAttempts int
	ConnectInterval time.Duration
}

// Errors with fixed messages (they match the Python deployer's).
var (
	ErrOrphaned       = errors.New("This service owns a public tunnel. Removing [tunnel] does not delete its DNS route or credentials; restore it or clean up the owned tunnel manually.")
	ErrNotConfigured  = errors.New("[tunnel] is required for this command")
	ErrNoOwnedTunnel  = errors.New("No owned tunnel is configured for this service")
	ErrInvalidState   = errors.New("Owned tunnel state is invalid; refusing to modify it")
	ErrInvalidStateID = errors.New("Owned tunnel state is invalid (bad tunnel id); refusing to modify it")
	ErrStateNotJSON   = errors.New("Owned tunnel state is not valid JSON; refusing to modify it")
	ErrForeignState   = errors.New("Owned tunnel state belongs to a different service; refusing to modify it")
	ErrDrift          = errors.New("Changing [tunnel] hostname or service is refused; stop and remove the owned tunnel explicitly before changing it")
	ErrNotConnected   = errors.New("Tunnel service started but did not establish a Cloudflare connection")
)

// State is the owned tunnel recorded in <base>/.depper/tunnel.json. Field
// order gives the same sorted keys the Python deployer wrote.
type State struct {
	Hostname string `json:"hostname"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Service  string `json:"service"`
}

// Name is the Cloudflare tunnel name depper creates for service name.
func Name(service string) string { return "depper-" + service }

// LegacyName is the tunnel name the Python deployer created. Migrated
// deployments keep it (the Cloudflare-side name is never changed).
func LegacyName(service string) string { return "deployer-" + service }

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ParseState validates tunnel.json content. Empty (or blank) content means
// no owned tunnel and returns nil, nil.
func ParseState(raw string) (*State, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if !json.Valid([]byte(raw)) {
		return nil, ErrStateNotJSON
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || len(fields) != 4 {
		return nil, ErrInvalidState
	}
	values := map[string]string{}
	for _, key := range []string{"name", "id", "hostname", "service"} {
		data, ok := fields[key]
		if !ok {
			return nil, ErrInvalidState
		}
		var s string
		if err := json.Unmarshal(data, &s); err != nil || s == "" || shell.HasControl(s) {
			return nil, ErrInvalidState
		}
		values[key] = s
	}
	if !uuidRe.MatchString(values["id"]) {
		return nil, ErrInvalidStateID
	}
	return &State{Name: values["name"], ID: values["id"], Hostname: values["hostname"], Service: values["service"]}, nil
}

// encode renders the state file.
func (s *State) encode() []byte {
	data, _ := json.Marshal(s)
	return append(data, '\n')
}

// URL is the public URL of the routed hostname.
func (s *State) URL() string { return "https://" + s.Hostname }

func (m *Manager) lookup(ctx context.Context, host string) ([]string, error) {
	if m.LookupHost != nil {
		return m.LookupHost(ctx, host)
	}
	return net.DefaultResolver.LookupHost(ctx, host)
}

func (m *Manager) settle() time.Duration {
	if m.ConnectSettle > 0 {
		return m.ConnectSettle
	}
	return DefaultConnectSettle
}

func (m *Manager) attempts() int {
	if m.ConnectAttempts > 0 {
		return m.ConnectAttempts
	}
	return DefaultConnectAttempts
}

func (m *Manager) interval() time.Duration {
	if m.ConnectInterval > 0 {
		return m.ConnectInterval
	}
	return DefaultConnectInterval
}

// stateLines print the state file (refusing a symlink) after a "@@state"
// marker, so the file content is always the last section.
func stateLines(l *engine.Layout) []string {
	return []string{
		"state=" + shell.Quote(l.TunnelState()),
		`printf '@@state\n'`,
		`if test -L "$state"; then echo 'Refusing a symlinked tunnel state file' >&2; exit 1; fi`,
		`if test -f "$state"; then head -c ` + fmt.Sprint(maxStateBytes) + ` "$state"; fi`,
	}
}

// ReadState reads and validates the owned tunnel state (nil when none).
func ReadState(ctx context.Context, e *engine.Engine, l *engine.Layout) (*State, error) {
	lines := append([]string{"set +xv", "set -eu"}, stateLines(l)...)
	out, err := e.Remote.Capture(ctx, shell.Script(lines...))
	if err != nil {
		return nil, err
	}
	_, raw, ok := strings.Cut(out, "@@state\n")
	if !ok {
		return nil, fmt.Errorf("remote host returned an invalid tunnel state response")
	}
	return ParseState(raw)
}

// inspect reads the cloudflared path and the owned state in one round trip.
func inspect(ctx context.Context, e *engine.Engine, l *engine.Layout) (cloudflared string, state *State, err error) {
	lines := []string{
		"set +xv",
		"set -eu",
		`printf '@@cloudflared\n'`,
		`command -v cloudflared 2>/dev/null || true`,
	}
	out, err := e.Remote.Capture(ctx, shell.Script(append(lines, stateLines(l)...)...))
	if err != nil {
		return "", nil, err
	}
	head, raw, ok := strings.Cut(out, "@@state\n")
	path, found := strings.CutPrefix(head, "@@cloudflared\n")
	if !ok || !found {
		return "", nil, fmt.Errorf("remote host returned an invalid tunnel state response")
	}
	state, err = ParseState(raw)
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(path), state, nil
}

// AssertNotOrphaned implements engine.TunnelHooks: deploying without
// [tunnel] is refused while the deployment owns a tunnel.
func (m *Manager) AssertNotOrphaned(ctx context.Context, e *engine.Engine, l *engine.Layout) error {
	if e.Config.Tunnel != nil {
		return nil
	}
	if !l.TunnelOwned {
		// The probe found no non-empty tunnel.json.
		return nil
	}
	state, err := ReadState(ctx, e, l)
	if err != nil {
		return err
	}
	if state != nil {
		return ErrOrphaned
	}
	return nil
}

// Ensure implements engine.TunnelHooks.
func (m *Manager) Ensure(ctx context.Context, e *engine.Engine, l *engine.Layout) error {
	_, err := m.EnsureState(ctx, e, l)
	return err
}

// EnsureState creates (first run) or verifies the owned tunnel, writes its
// cloudflared configuration, routes its hostname without overwriting DNS,
// and installs and starts its connector unit. The caller holds the
// deployment lock.
func (m *Manager) EnsureState(ctx context.Context, e *engine.Engine, l *engine.Layout) (*State, error) {
	cfg := e.Config
	wanted := cfg.Tunnel
	if wanted == nil {
		return nil, ErrNotConfigured
	}
	if !shell.IsPlainPath(l.Home) {
		return nil, fmt.Errorf("remote host returned an invalid home directory for cloudflared")
	}
	cloudflared, state, err := inspect(ctx, e, l)
	if err != nil {
		return nil, err
	}
	if state != nil {
		if state.Name != Name(cfg.Service.Name) && state.Name != LegacyName(cfg.Service.Name) {
			return nil, ErrForeignState
		}
		if state.Hostname != wanted.Hostname || state.Service != wanted.Service {
			return nil, ErrDrift
		}
	}
	if !shell.IsPlainPath(cloudflared) {
		return nil, fmt.Errorf("cloudflared is not installed at a safe absolute path on the deploy host")
	}
	cf := &cloudflaredCLI{e: e, path: cloudflared}
	if state == nil {
		if state, err = m.create(ctx, e, l, cf, wanted.Hostname, wanted.Service); err != nil {
			return nil, err
		}
	}
	if err := e.Upload(ctx, []byte(engine.CloudflaredConfig(state.ID, l.Home, state.Hostname, state.Service)), l.TunnelConfig(), "600"); err != nil {
		return nil, fmt.Errorf("writing the cloudflared configuration failed: %w", err)
	}
	if err := cf.run(ctx, "--config", l.TunnelConfig(), "tunnel", "ingress", "validate"); err != nil {
		return nil, err
	}
	// --overwrite-dns=false (with the inherited force variable cleared)
	// never replaces an existing record.
	if err := cf.run(ctx, "tunnel", "route", "dns", "--overwrite-dns=false", state.ID, state.Hostname); err != nil {
		return nil, err
	}
	if err := m.installUnit(ctx, e, l, cf, state); err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(e.Out, "Tunnel %s (%s) is connected: %s -> %s\n", state.Name, state.ID, state.URL(), state.Service)
	return state, nil
}

// create makes a new tunnel named depper-<name> and records it, refusing a
// hostname that already resolves and a same-named tunnel it does not own.
func (m *Manager) create(ctx context.Context, e *engine.Engine, l *engine.Layout, cf *cloudflaredCLI, hostname, service string) (*State, error) {
	if err := m.preflightHostname(ctx, hostname); err != nil {
		return nil, err
	}
	name := Name(e.Config.Service.Name)
	tunnels, err := cf.list(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tunnels {
		if t.Name == name {
			return nil, fmt.Errorf("Tunnel %q already exists but is not owned by this deployment; refusing to adopt it", name)
		}
	}
	if err := cf.create(ctx, name); err != nil {
		return nil, err
	}
	tunnels, err = cf.list(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w after creation", err)
	}
	var ids []string
	for _, t := range tunnels {
		if t.Name == name && t.HasID {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) != 1 {
		return nil, fmt.Errorf("Created tunnel could not be identified; it was not adopted")
	}
	state := &State{Name: name, ID: ids[0], Hostname: hostname, Service: service}
	if _, err := ParseState(string(state.encode())); err != nil {
		return nil, err
	}
	if err := e.Upload(ctx, state.encode(), l.TunnelState(), "600"); err != nil {
		return nil, fmt.Errorf("recording the owned tunnel state failed: %w", err)
	}
	_, _ = fmt.Fprintf(e.Out, "Created Cloudflare Tunnel %s (%s)\n", name, state.ID)
	return state, nil
}

// preflightHostname refuses a hostname that already resolves. A positive
// answer may be an inherited wildcard record; fail closed before creating
// anything.
func (m *Manager) preflightHostname(ctx context.Context, hostname string) error {
	addrs, err := m.lookup(ctx, hostname)
	if err == nil {
		if len(addrs) == 0 {
			return nil
		}
		return fmt.Errorf("%s already resolves; refusing to risk a DNS conflict", hostname)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return nil
	}
	return fmt.Errorf("DNS lookup failed: %w", err)
}

// installUnit writes the connector unit (refusing to replace one without
// depper's marker), restarts it, and waits for a Cloudflare connection.
func (m *Manager) installUnit(ctx context.Context, e *engine.Engine, l *engine.Layout, cf *cloudflaredCLI, state *State) error {
	cfg := e.Config
	unitName := cfg.TunnelUnitName()
	candidate := l.Meta() + "/tunnel-unit.next"
	contents := engine.TunnelUnitFile(cfg.Service.Name, cf.path, l.TunnelConfig(), state.ID)
	if err := e.Upload(ctx, []byte(contents), candidate, "600"); err != nil {
		return fmt.Errorf("writing the tunnel unit failed: %w", err)
	}
	lines := append([]string{"set +xv"}, shell.Prelude()...)
	lines = append(lines,
		"umask 077",
		`unit="$HOME/.config/systemd/user/"`+shell.Quote(unitName),
		"candidate="+shell.Quote(candidate),
		`mkdir -p "$(dirname "$unit")"`,
		`if test -L "$unit" || { test -e "$unit" && ! grep -Fxq -e `+shell.Quote(engine.TunnelUnitMarker(cfg.Service.Name))+` "$unit"; }; then`,
		`  rm -f "$candidate"`,
		`  echo 'Refusing to overwrite an unowned tunnel unit' >&2`,
		`  exit 1`,
		`fi`,
		`mv -f "$candidate" "$unit"`,
		"systemctl --user daemon-reload",
		"systemctl --user enable "+shell.Quote(unitName)+" >/dev/null",
		"systemctl --user restart "+shell.Quote(unitName),
	)
	if err := runScript(ctx, e, shell.Script(lines...)); err != nil {
		return err
	}
	if err := e.Sleep(ctx, m.settle()); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		connected, err := m.connected(ctx, e, cf, state.ID)
		if err != nil {
			return err
		}
		if connected {
			return nil
		}
		if attempt >= m.attempts() {
			return ErrNotConnected
		}
		if err := e.Sleep(ctx, m.interval()); err != nil {
			return err
		}
	}
}

// connected requires the connector unit to be active and reports whether
// cloudflared lists a connection for tunnel id. A failing or unparsable
// listing counts as not connected yet.
func (m *Manager) connected(ctx context.Context, e *engine.Engine, cf *cloudflaredCLI, id string) (bool, error) {
	unitName := e.Config.TunnelUnitName()
	lines := append([]string{"set +xv"}, shell.Prelude()...)
	lines = append(lines,
		"unset TUNNEL_FORCE_PROVISIONING_DNS",
		"systemctl --user is-active --quiet "+shell.Quote(unitName)+" || { echo "+shell.Quote(unitName+" is not active after restart")+" >&2; exit 1; }",
		shell.Quote(cf.path)+" tunnel list --output json 2>/dev/null || true",
	)
	out, err := e.Remote.Capture(ctx, shell.Script(lines...))
	if err != nil {
		return false, err
	}
	tunnels, err := parseList(out)
	if err != nil {
		return false, nil
	}
	for _, t := range tunnels {
		if t.HasID && t.ID == id && t.Connections > 0 {
			return true, nil
		}
	}
	return false, nil
}

// Run implements `depper tunnel`: ensure the owned tunnel under the
// deployment lock.
func (m *Manager) Run(ctx context.Context, e *engine.Engine) (*State, error) {
	if e.Config.Tunnel == nil {
		return nil, ErrNotConfigured
	}
	l, err := e.Probe(ctx, true)
	if err != nil {
		return nil, err
	}
	var state *State
	err = e.WithLock(ctx, l, func() error {
		var err error
		state, err = m.EnsureState(ctx, e, l)
		return err
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

func userScript(command string) string {
	lines := append([]string{"set +xv"}, shell.Prelude()...)
	return shell.Script(append(lines, command)...)
}

// Status streams `systemctl --user status` of the connector unit.
func Status(ctx context.Context, e *engine.Engine) error {
	if _, err := e.Probe(ctx, false); err != nil {
		return err
	}
	return e.Remote.Run(ctx, userScript("systemctl --user status "+shell.Quote(e.Config.TunnelUnitName())+" --no-pager"))
}

// Logs streams the connector unit's journal to w.
func Logs(ctx context.Context, e *engine.Engine, lines int, w io.Writer) error {
	if lines < 1 {
		return fmt.Errorf("--lines must be a positive integer")
	}
	if _, err := e.Probe(ctx, false); err != nil {
		return err
	}
	return e.Remote.Stream(ctx, userScript("journalctl --user-unit "+shell.Quote(e.Config.TunnelUnitName())+" --no-pager -n "+fmt.Sprint(lines)), w)
}

// Stop disables and stops the connector unit. The tunnel, its state, and
// its DNS route are kept; a later deploy starts the connector again.
func Stop(ctx context.Context, e *engine.Engine) error {
	l, err := e.Probe(ctx, false)
	if err != nil {
		return err
	}
	state, err := ReadState(ctx, e, l)
	if err != nil {
		return err
	}
	if state == nil {
		return ErrNoOwnedTunnel
	}
	return e.Remote.Run(ctx, userScript("systemctl --user disable --now "+shell.Quote(e.Config.TunnelUnitName())))
}
