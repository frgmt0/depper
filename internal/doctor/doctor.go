// Package doctor checks that a deploy host can run depper deployments.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/transport"
)

// Status is the outcome of one check.
type Status string

// Check outcomes.
const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
)

// MinFreeKiB is the free disk space (1 GiB) below which doctor warns.
const MinFreeKiB = 1024 * 1024

// staleLockAge mirrors engine.StaleLockAge (15 minutes).
const staleLockAge = 15 * time.Minute

// Check is one diagnostic result.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Report is the result of Run.
type Report struct {
	Checks []Check `json:"checks"`
}

// Failed reports whether any check failed.
func (r *Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// WriteHuman prints one line per check.
func (r *Report) WriteHuman(w io.Writer) {
	for _, c := range r.Checks {
		_, _ = fmt.Fprintf(w, "%-5s %s: %s\n", strings.ToUpper(string(c.Status)), c.Name, c.Detail)
	}
}

// Doctor runs the checks for one configuration.
type Doctor struct {
	Config *config.Config
	Remote *transport.Remote
	// Now returns the current time (time.Now by default).
	Now func() time.Time
}

func (d *Doctor) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Run gathers every remote fact in one SSH round trip and evaluates the
// checks. A connectivity failure is reported as a single failed check.
func (d *Doctor) Run(ctx context.Context) *Report {
	out, err := d.Remote.Capture(ctx, Script(d.Config))
	if err != nil {
		return &Report{Checks: []Check{{Name: "ssh", Status: Fail, Detail: "cannot run commands on the deploy host: " + clean(err.Error())}}}
	}
	facts := ParseFacts(out)
	if facts["doctor"] != "1" {
		return &Report{Checks: []Check{{Name: "ssh", Status: Fail, Detail: "the deploy host did not run the diagnostic script (is the login shell POSIX sh compatible?)"}}}
	}
	return &Report{Checks: d.evaluate(facts)}
}

// Script returns the POSIX sh script that prints key=value facts about the
// deploy host. It only reads state.
func Script(cfg *config.Config) string {
	var base, legacy string
	if p := cfg.Deploy.Path; p != "" {
		base, legacy = shell.Quote(p), shell.Quote(p)
	} else {
		base = `"$HOME/` + cfg.DefaultBaseRel() + `"`
		legacy = `"$HOME/` + cfg.LegacyDefaultBaseRel() + `"`
	}
	lines := []string{
		"set +xv",
		"set -u",
		`export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"`,
		`export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"`,
		"base=" + base,
		"legacy=" + legacy,
		`user=$(id -un 2>/dev/null || true)`,
		`printf 'user=%s\n' "$user"`,
		`printf 'base=%s\n' "$base"`,
		`printf 'rsync=%s\n' "$(command -v rsync 2>/dev/null || true)"`,
		`printf 'sha256sum=%s\n' "$(command -v sha256sum 2>/dev/null || true)"`,
		`printf 'systemctl=%s\n' "$(command -v systemctl 2>/dev/null || true)"`,
		`if systemctl --user show-environment >/dev/null 2>&1; then printf 'systemd=ok\n'; else printf 'systemd=fail\n'; fi`,
		// Linger: loginctl is authoritative, the linger file is the fallback.
		`linger=$(loginctl show-user "$user" -p Linger --value 2>/dev/null || true)`,
		`if test -z "$linger"; then if test -e "/var/lib/systemd/linger/$user"; then linger=yes; else linger=unknown; fi; fi`,
		`printf 'linger=%s\n' "$linger"`,
		// Free space of the filesystem that holds (or will hold) the base.
		`d=$base; while test ! -d "$d" && test "$d" != / && test "$d" != .; do d=$(dirname "$d"); done`,
		`printf 'disk_kb=%s\n' "$(df -Pk "$d" 2>/dev/null | awk 'NR==2 { print $4 }')"`,
	}
	if c := cfg.Container; c != nil {
		lines = append(lines, `printf 'engine=%s\n' "$(command -v `+shell.Quote(c.Engine)+` 2>/dev/null || true)"`)
	}
	if cfg.Tunnel != nil {
		lines = append(lines, `printf 'cloudflared=%s\n' "$(command -v cloudflared 2>/dev/null || true)"`)
	}
	lines = append(lines,
		// Layout detection mirrors the engine's base-path probe.
		`has_new=no; if test -d "$base/.depper"; then has_new=yes; fi`,
		`has_legacy=no`,
		`if test -e "$legacy/.deployer" || test -L "$legacy/.deployer"; then has_legacy=yes; fi`,
		`if test -e "$HOME/.config/systemd/user/"`+shell.Quote(cfg.LegacyUnitName())+`; then has_legacy=yes; fi`,
		`if test "$has_new" = yes && test "$has_legacy" = yes; then layout=both`,
		`elif test "$has_new" = yes; then layout=depper`,
		`elif test "$has_legacy" = yes; then layout=legacy`,
		`else layout=none; fi`,
		`printf 'layout=%s\n' "$layout"`,
		`lock="$base/.depper/lock"`,
		`if test -d "$lock" && ! test -L "$lock"; then`,
		`  printf 'lock=held\n'`,
		fmt.Sprintf(`  if test -n "$(find "$lock" -prune -mmin +%d 2>/dev/null)"; then printf 'lock_old=yes\n'; else printf 'lock_old=no\n'; fi`, int(staleLockAge/time.Minute)),
		`  if test -f "$lock/info" && ! test -L "$lock/info"; then printf 'lock_info=%s\n' "$(head -c 4096 "$lock/info" | tr '\n' ' ')"; fi`,
		`else printf 'lock=free\n'; fi`,
		`printf 'doctor=1\n'`,
	)
	return shell.Script(lines...)
}

// ParseFacts parses key=value lines; the value is everything after the first
// "=". Lines without "=" are ignored and the last value of a key wins.
func ParseFacts(out string) map[string]string {
	facts := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok && key != "" {
			facts[key] = value
		}
	}
	return facts
}

// clean removes control characters from text that came from the remote host
// so it cannot manipulate the terminal.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

type lockInfo struct {
	CreatedAt string `json:"created_at"`
	Holder    string `json:"holder"`
	PID       int    `json:"pid"`
	Command   string `json:"command"`
}

func (d *Doctor) evaluate(f map[string]string) []Check {
	checks := []Check{{Name: "ssh", Status: Pass, Detail: "connected to " + d.Config.Deploy.Host + " as " + clean(f["user"])}}
	add := func(name string, status Status, detail string) {
		checks = append(checks, Check{Name: name, Status: status, Detail: detail})
	}
	binary := func(name, fact, hint string) {
		if path := clean(f[fact]); path != "" {
			add(name, Pass, path)
		} else {
			add(name, Fail, name+" is not installed on the deploy host"+hint)
		}
	}

	binary("rsync", "rsync", " (install it with your package manager)")
	binary("sha256sum", "sha256sum", " (part of coreutils)")

	switch {
	case f["systemctl"] == "":
		add("systemd", Fail, "systemctl is not installed; depper needs a systemd user manager")
	case f["systemd"] != "ok":
		add("systemd", Fail, "`systemctl --user` cannot reach the user manager (is systemd the init system and the user session available?)")
	default:
		add("systemd", Pass, "`systemctl --user` is reachable")
	}

	user := clean(f["user"])
	switch f["linger"] {
	case "yes":
		add("linger", Pass, "enabled for "+user)
	case "no":
		add("linger", Warn, "disabled for "+user+": services stop at logout and do not start at boot; run `sudo loginctl enable-linger "+user+"`")
	default:
		add("linger", Warn, "could not determine whether linger is enabled for "+user+"; check `loginctl show-user "+user+" -p Linger`")
	}

	kb, err := strconv.ParseInt(strings.TrimSpace(f["disk_kb"]), 10, 64)
	switch {
	case err != nil || kb < 0:
		add("disk", Warn, "could not determine free disk space under "+clean(f["base"]))
	case kb < MinFreeKiB:
		add("disk", Warn, fmt.Sprintf("only %s free under %s (less than 1 GiB)", humanKiB(kb), clean(f["base"])))
	default:
		add("disk", Pass, fmt.Sprintf("%s free under %s", humanKiB(kb), clean(f["base"])))
	}

	if c := d.Config.Container; c != nil {
		if path := clean(f["engine"]); path != "" {
			add("container engine", Pass, path)
		} else {
			add("container engine", Fail, c.Engine+" is not installed on the deploy host (required by [container])")
		}
	}
	if d.Config.Tunnel != nil {
		if path := clean(f["cloudflared"]); path != "" {
			add("cloudflared", Pass, path)
		} else {
			add("cloudflared", Fail, "cloudflared is not installed on the deploy host (required by [tunnel])")
		}
	}

	lock := d.lockStatus(f)
	add("lock", lock.Status, lock.Detail)

	switch f["layout"] {
	case "legacy":
		add("layout", Warn, "found a deployment made by the Python deployer; run `depper migrate` to convert it")
	case "both":
		add("layout", Warn, "both a depper and a legacy Python deployer deployment exist; `depper migrate` refuses this state, so remove the legacy one by hand")
	case "depper":
		add("layout", Pass, "depper deployment at "+clean(f["base"]))
	default:
		add("layout", Pass, "no deployment yet; the first deploy creates "+clean(f["base"]))
	}
	return checks
}

func (d *Doctor) lockStatus(f map[string]string) Check {
	if f["lock"] != "held" {
		return Check{Status: Pass, Detail: "no deployment lock is held"}
	}
	var info lockInfo
	if raw := strings.TrimSpace(f["lock_info"]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &info)
	}
	holder := ""
	if info.Command != "" || info.Holder != "" {
		holder = fmt.Sprintf(" by `depper %s` (%s, pid %d)", clean(info.Command), clean(info.Holder), info.PID)
	}
	stale := f["lock_old"] == "yes"
	if created, err := time.Parse(time.RFC3339, info.CreatedAt); err == nil {
		stale = d.now().Sub(created) >= staleLockAge
		holder += " since " + clean(info.CreatedAt)
	}
	if stale {
		return Check{Status: Warn, Detail: "a stale deployment lock is held" + holder + "; run `depper unlock` if nothing is running"}
	}
	return Check{Status: Pass, Detail: "a deployment is in progress" + holder}
}

func humanKiB(kb int64) string {
	switch {
	case kb >= 1024*1024:
		return fmt.Sprintf("%.1f GiB", float64(kb)/(1024*1024))
	case kb >= 1024:
		return fmt.Sprintf("%.0f MiB", float64(kb)/1024)
	}
	return fmt.Sprintf("%d KiB", kb)
}
