package host

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/shell"
)

// Status check outcomes. "info" marks opt-in features that are not enabled.
const (
	StatusPass = "pass"
	StatusWarn = "warn"
	StatusFail = "fail"
	StatusInfo = "info"
)

// StatusCheck is one `depper host status` line.
type StatusCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// StatusResult is the `depper host status` report (also its --json form).
type StatusResult struct {
	Host   string        `json:"host"`
	User   string        `json:"user"`
	Target string        `json:"target"`
	Record *Record       `json:"record"`
	Checks []StatusCheck `json:"checks"`
}

// Failed reports whether any check failed.
func (s *StatusResult) Failed() bool {
	for _, c := range s.Checks {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}

// WriteHuman prints the report.
func (s *StatusResult) WriteHuman(w io.Writer) {
	if s.Record != nil {
		_, _ = fmt.Fprintf(w, "Prepared by depper %s (created %s, updated %s) for %s\n", s.Record.DepperVersion, s.Record.CreatedAt, s.Record.UpdatedAt, s.Record.DeployUser)
		if len(s.Record.Packages) > 0 {
			_, _ = fmt.Fprintf(w, "Packages installed by depper: %s\n", strings.Join(s.Record.Packages, " "))
		}
	}
	for _, c := range s.Checks {
		_, _ = fmt.Fprintf(w, "%-5s %s: %s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
	}
}

const statusMarker = "@@depper-host-status"

// statusScript reads the host record and live state as the deploy user; it
// needs no administrator rights and changes nothing.
func (r *Runner) statusScript() string {
	lines := append([]string{"set -u"}, shell.SessionEnv()...)
	return shell.Script(append(lines,
		"echo "+statusMarker,
		`u=$(id -un 2>/dev/null || true)`,
		`printf 'user=%s\n' "$u"`,
		`f=`+r.p(RecordPath),
		`if test -r "$f"; then printf 'record=%s\n' "$(base64 < "$f" | tr -d '\n')"; elif test -e "$f"; then echo record_unreadable=1; fi`,
		`if test -e `+r.p("/var/lib/systemd/linger")+`/"$u"; then echo linger=1; else l=$(loginctl show-user "$u" -p Linger --value 2>/dev/null || true); if test "$l" = yes; then echo linger=1; else echo linger=0; fi; fi`,
		`if systemctl --user show-environment >/dev/null 2>&1; then echo usermgr=1; else echo usermgr=0; fi`,
		`for c in podman docker cloudflared tailscale rsync; do printf 'cmd_%s=%s\n' "$c" "$(command -v "$c" 2>/dev/null || true)"; done`,
		`if test -f `+r.p(SSHDropIn)+`; then echo dropin=1; fi`,
		`if test -r `+r.p("/etc/ufw/ufw.conf")+` && grep -q '^ENABLED=yes' `+r.p("/etc/ufw/ufw.conf")+`; then echo ufw=1; fi`,
		`if systemctl is-active --quiet firewalld.service 2>/dev/null; then echo firewalld=1; fi`,
		`if test -f `+r.p("/etc/apt/apt.conf.d/20auto-upgrades")+` && grep -q 'APT::Periodic::Unattended-Upgrade "1"' `+r.p("/etc/apt/apt.conf.d/20auto-upgrades")+`; then echo autoupd=1; fi`,
		`for t in dnf-automatic-install.timer dnf5-automatic.timer; do if systemctl is-enabled --quiet "$t" 2>/dev/null; then echo autoupd=1; fi; done`,
		`if command -v tailscale >/dev/null 2>&1 && tailscale status >/dev/null 2>&1; then echo tailscale_up=1; fi`,
		`echo status_done=1`,
	)...)
}

// Status reports on a host prepared by host init, connecting as the deploy
// user from cfg.
func (r *Runner) Status(ctx context.Context, cfg *config.Config) (*StatusResult, error) {
	rem := r.remote(cfg.SSHTarget())
	out, err := rem.Capture(ctx, r.statusScript())
	if err != nil {
		return nil, fmt.Errorf("cannot connect to %s: %w", cfg.Deploy.Host, err)
	}
	i := strings.Index(out, statusMarker+"\n")
	if i < 0 {
		return nil, fmt.Errorf("the host did not run the status script (is the login shell POSIX sh compatible?)")
	}
	m := map[string]string{}
	for _, line := range strings.Split(out[i+len(statusMarker)+1:], "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok && k != "" {
			m[k] = v
		}
	}
	if m["status_done"] != "1" {
		return nil, fmt.Errorf("the status script did not finish on the host")
	}
	res := &StatusResult{Host: cfg.Deploy.Host, User: clean(m["user"]), Target: cfg.Target, Checks: []StatusCheck{}}
	add := func(name, status, detail string) {
		res.Checks = append(res.Checks, StatusCheck{Name: name, Status: status, Detail: detail})
	}
	if raw := strings.TrimSpace(m["record"]); raw != "" {
		data, err := base64.StdEncoding.DecodeString(raw)
		var rec Record
		if err == nil && json.Unmarshal(data, &rec) == nil {
			res.Record = &rec
		}
	}
	rec := res.Record
	switch {
	case rec != nil:
		add("record", StatusPass, RecordPath+" written by depper "+clean(rec.DepperVersion)+" at "+clean(rec.UpdatedAt))
	case m["record_unreadable"] == "1":
		add("record", StatusWarn, RecordPath+" exists but is not readable by "+res.User)
	default:
		add("record", StatusWarn, "no "+RecordPath+": this host was not prepared with `depper host init`")
	}
	if rec != nil && rec.DeployUser != "" && rec.DeployUser != res.User {
		add("user", StatusWarn, "connected as "+res.User+", but the host was prepared for "+clean(rec.DeployUser))
	} else {
		add("user", StatusPass, res.User+" exists and can log in")
	}
	if m["linger"] == "1" {
		add("linger", StatusPass, "enabled for "+res.User)
	} else {
		add("linger", StatusFail, "disabled for "+res.User+"; run `depper host init` again or `sudo loginctl enable-linger "+res.User+"`")
	}
	if m["usermgr"] == "1" {
		add("systemd", StatusPass, "`systemctl --user` is reachable")
	} else {
		add("systemd", StatusFail, "`systemctl --user` cannot reach the user manager")
	}
	engine := ""
	if cfg.Container != nil {
		engine = cfg.Container.Engine
	} else if rec != nil && rec.Container != "" && rec.Container != ContainerNone {
		engine = rec.Container
	}
	if engine != "" {
		if p := clean(m["cmd_"+engine]); p != "" {
			add("container engine", StatusPass, p)
		} else {
			add("container engine", StatusFail, engine+" is not installed")
		}
	} else {
		add("container engine", StatusInfo, "none configured")
	}
	if cfg.Tunnel != nil || (rec != nil && rec.Cloudflared) {
		if p := clean(m["cmd_cloudflared"]); p != "" {
			add("cloudflared", StatusPass, p)
		} else {
			add("cloudflared", StatusFail, "cloudflared is not installed")
		}
	}
	feature := func(name string, recorded, live bool, on, off string) {
		switch {
		case live:
			add(name, StatusPass, on)
		case recorded:
			add(name, StatusFail, off)
		default:
			add(name, StatusInfo, "not enabled (opt-in)")
		}
	}
	h := Hardening{}
	if rec != nil {
		h = rec.Hardening
	}
	feature("ssh hardening", h.SSH, m["dropin"] == "1", SSHDropIn+" is present", SSHDropIn+" is missing although depper applied it")
	fw := m["ufw"] == "1" || m["firewalld"] == "1"
	fwName := "ufw"
	if m["firewalld"] == "1" {
		fwName = "firewalld"
	}
	feature("firewall", len(h.Firewall) > 0, fw, fwName+" is active", "no firewall is active although depper enabled one (ports "+intsString(h.Firewall)+")")
	feature("auto updates", h.AutoUpdates, m["autoupd"] == "1", "automatic security updates are enabled", "automatic updates are not enabled although depper configured them")
	feature("tailscale", h.Tailscale, m["tailscale_up"] == "1", "tailscale is up", "tailscale is not up although depper joined the tailnet")
	return res, nil
}
