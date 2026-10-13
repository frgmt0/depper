package engine

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/frgmt0/depper/internal/shell"
)

// Status is `depper status`. JSON keys are stable:
//
//	service, target, host, unit, base     identity and location
//	deployed                              a depper deployment exists
//	load_state, active_state, sub_state, main_pid, n_restarts,
//	active_enter_timestamp                systemd unit properties
//	current_release, previous_release     release ids ("" when none)
//	release                               current .depper-release.json (null when absent)
//	endpoint_url, tunnel_url              configured URLs ("" when unset)
//	health                                health result (null when not deployed)
//	health_error                          why the health check could not run
type Status struct {
	Service  string `json:"service"`
	Target   string `json:"target"`
	Host     string `json:"host"`
	Unit     string `json:"unit"`
	Base     string `json:"base"`
	Deployed bool   `json:"deployed"`
	UnitState
	CurrentRelease  string           `json:"current_release"`
	PreviousRelease string           `json:"previous_release"`
	Release         *ReleaseMetadata `json:"release"`
	EndpointURL     string           `json:"endpoint_url"`
	TunnelURL       string           `json:"tunnel_url"`
	Health          *HealthResult    `json:"health"`
	HealthError     string           `json:"health_error"`
}

func (e *Engine) statusScript(l *Layout) string {
	lines := append([]string{"set +xv"}, shell.Prelude()...)
	lines = append(lines, "base="+shell.Quote(l.Base), "echo @@unit")
	lines = append(lines, unitShowLines(e.Config.UnitName())...)
	lines = append(lines,
		`echo @@current`,
		`if test -L "$base/current"; then readlink "$base/current"; fi`,
		`echo @@previous`,
		`if test -L "$base/previous"; then readlink "$base/previous"; fi`,
		`echo @@release`,
		`if test -f "$base/current/`+ReleaseMetadataName+`" && ! test -L "$base/current/`+ReleaseMetadataName+`"; then head -c 65536 "$base/current/`+ReleaseMetadataName+`"; echo; fi`,
	)
	return shell.Script(lines...)
}

// Status reads the unit state, release metadata, and health.
func (e *Engine) Status(ctx context.Context) (*Status, error) {
	cfg := e.Config
	l, err := e.Probe(ctx, false)
	if err != nil {
		return nil, err
	}
	s := &Status{Service: cfg.Service.Name, Target: cfg.Target, Host: cfg.Deploy.Host, Unit: cfg.UnitName(), Base: l.Base, Deployed: l.Deployed()}
	if cfg.Endpoint != nil {
		s.EndpointURL = cfg.Endpoint.URL
	}
	if cfg.Tunnel != nil {
		s.TunnelURL = cfg.Tunnel.URL()
	}
	out, err := e.Remote.Capture(ctx, e.statusScript(l))
	if err != nil {
		return nil, err
	}
	m := sectionMap(out)
	s.UnitState = *parseUnitState(m["unit"])
	s.CurrentRelease = releaseFromLink(m["current"])
	s.PreviousRelease = releaseFromLink(m["previous"])
	s.Release = parseReleaseMetadata(m["release"])
	if s.Deployed && s.CurrentRelease != "" {
		if r, err := e.CheckHealth(ctx, l); err != nil {
			s.HealthError = err.Error()
		} else {
			s.Health = r
		}
	}
	return s, nil
}

// WriteHuman prints the status for people.
func (s *Status) WriteHuman(w io.Writer) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("%s on %s\n", s.Service, s.Host)
	if s.Target != "" {
		p("  target:    %s\n", s.Target)
	}
	p("  unit:      %s (%s/%s", s.Unit, s.ActiveState, s.SubState)
	if s.MainPID != 0 {
		p(", pid %d", s.MainPID)
	}
	p(", %d restarts)\n", s.NRestarts)
	if s.ActiveEnterTimestamp != "" {
		p("  since:     %s\n", s.ActiveEnterTimestamp)
	}
	if !s.Deployed {
		p("  release:   not deployed (%s)\n", s.Base)
	} else {
		p("  release:   %s\n", describeRelease(s.CurrentRelease, s.Release))
		if s.PreviousRelease != "" {
			p("  previous:  %s\n", s.PreviousRelease)
		}
		p("  base:      %s\n", s.Base)
	}
	if s.EndpointURL != "" {
		p("  endpoint:  %s\n", s.EndpointURL)
	}
	if s.TunnelURL != "" {
		p("  tunnel:    %s\n", s.TunnelURL)
	}
	switch {
	case s.Health != nil:
		p("  health:    %s\n", s.Health.Summary())
	case s.HealthError != "":
		p("  health:    could not check (%s)\n", s.HealthError)
	}
}

func describeRelease(id string, m *ReleaseMetadata) string {
	if id == "" {
		return "(none)"
	}
	if m == nil {
		return id
	}
	out := id
	if c := m.ShortCommit(); c != "" {
		out += " commit " + c
		if m.GitRef != "" {
			out += " (" + m.GitRef + ")"
		}
		if m.GitDirty != nil && *m.GitDirty {
			out += " +uncommitted changes"
		}
	}
	if m.Source != "" {
		out += ", source " + m.Source
	}
	if m.DeployedBy != "" {
		out += ", by " + m.DeployedBy
	}
	if m.CreatedAt != "" {
		out += " at " + m.CreatedAt
	}
	return out
}

// Logs streams the unit's journal (journalctl -n lines [-f]) to w.
func (e *Engine) Logs(ctx context.Context, lines int, follow bool, w io.Writer) error {
	if lines < 1 {
		return fmt.Errorf("--lines must be positive")
	}
	if _, err := e.Probe(ctx, false); err != nil {
		return err
	}
	cmd := "journalctl --user-unit " + shell.Quote(e.Config.UnitName()) + " --no-pager -n " + strconv.Itoa(lines)
	if follow {
		cmd += " -f"
	}
	script := append([]string{"set +xv"}, shell.Prelude()...)
	return e.Remote.Stream(ctx, shell.Script(append(script, cmd)...), w)
}

// Control runs `systemctl --user restart|stop` on the app unit.
func (e *Engine) Control(ctx context.Context, action string) error {
	if action != "restart" && action != "stop" {
		return fmt.Errorf("unsupported action %q", action)
	}
	l, err := e.Probe(ctx, false)
	if err != nil {
		return err
	}
	if !l.Deployed() {
		return ErrNotDeployed
	}
	script := append([]string{"set +xv"}, shell.Prelude()...)
	return e.Remote.Run(ctx, shell.Script(append(script, "systemctl --user "+action+" "+shell.Quote(e.Config.UnitName()))...))
}
