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
	"github.com/frgmt0/depper/internal/engine"
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

// factsMarker precedes the facts, so output a login shell printed before
// the script ran is ignored.
const factsMarker = "@@doctor"

// Script returns the POSIX sh script that prints key=value facts about the
// deploy host. It only reads state.
func Script(cfg *config.Config) string {
	base, legacy := engine.BaseAssignments(cfg)
	lines := append([]string{"set +xv", "set -u"}, shell.SessionEnv()...)
	lines = append(lines,
		"base="+base,
		"legacy="+legacy,
		`echo `+factsMarker,
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
	)
	if c := cfg.Container; c != nil {
		engineName := shell.Quote(c.Engine)
		lines = append(lines, `printf 'engine=%s\n' "$(command -v `+engineName+` 2>/dev/null || true)"`)
		if c.Mode() == config.ModeCompose {
			// Only with the engine present (otherwise the container engine
			// check already fails). The first meaningful output line: podman
			// announces its external compose provider on stderr first, and
			// PODMAN_COMPOSE_WARNING_LOGS=false asks it not to.
			lines = append(lines,
				`if command -v `+engineName+` >/dev/null 2>&1; then`,
				`  if compose_out=$(PODMAN_COMPOSE_WARNING_LOGS=false `+engineName+` compose version 2>&1 </dev/null); then compose_rc=0; else compose_rc=$?; fi`,
				`  printf 'compose_rc=%s\n' "$compose_rc"`,
				`  printf 'compose_out=%s\n' "$(printf '%s\n' "$compose_out" | sed -e '/^>>>> /d' -e '/^[[:space:]]*$/d' | head -n 1 | cut -c 1-`+strconv.Itoa(maxComposeLine)+`)"`,
				`fi`,
			)
		}
	}
	if cfg.Tunnel != nil {
		lines = append(lines, `printf 'cloudflared=%s\n' "$(command -v cloudflared 2>/dev/null || true)"`)
	}
	// Layout detection and lock age are the engine's own logic.
	lines = append(lines, engine.LayoutDetectionLines(cfg)...)
	lines = append(lines,
		`printf 'layout=%s\n' "$state"`,
		`lock="$base/.depper/lock"`,
		`if test -d "$lock" && ! test -L "$lock"; then`,
		`  printf 'lock=held\n'`,
	)
	for _, line := range engine.LockAgeLines() {
		lines = append(lines, "  "+line)
	}
	lines = append(lines,
		`  if test -f "$lock/info" && ! test -L "$lock/info"; then printf 'lock_info=%s\n' "$(head -c 4096 "$lock/info" | tr '\n' ' ')"; fi`,
		`else printf 'lock=free\n'; fi`,
		`printf 'doctor=1\n'`,
	)
	return shell.Script(lines...)
}

// ParseFacts parses the key=value lines after the facts marker (all lines
// when there is none); the value is everything after the first "=". Lines
// without "=" are ignored and the last value of a key wins.
func ParseFacts(out string) map[string]string {
	facts := map[string]string{}
	for _, line := range strings.Split(engine.FromMarker(out, factsMarker), "\n") {
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
			if c.Mode() == config.ModeCompose {
				status, detail := composeCheck(c.Engine, f)
				add("compose", status, detail)
			}
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
	case "migrating":
		add("layout", Warn, "a `depper migrate` was interrupted; run `depper migrate` again to finish it")
	case "both":
		add("layout", Warn, "both a depper and a legacy Python deployer deployment exist; `depper migrate` refuses this state, so remove the legacy one by hand")
	case "depper":
		add("layout", Pass, "depper deployment at "+clean(f["base"]))
	default:
		add("layout", Pass, "no deployment yet; the first deploy creates "+clean(f["base"]))
	}
	return checks
}

// maxComposeLine bounds the `compose version` line doctor reports.
const maxComposeLine = 200

// composeCheck evaluates `<engine> compose version`, which doctor runs for a
// compose-mode [container] when the engine is installed.
func composeCheck(engineName string, f map[string]string) (Status, string) {
	msg := truncate(clean(f["compose_out"]), maxComposeLine)
	if f["compose_rc"] == "0" {
		if msg == "" {
			msg = "`" + engineName + " compose version` succeeded"
		}
		return Pass, msg
	}
	if msg == "" {
		if rc := clean(f["compose_rc"]); rc != "" {
			msg = "exit status " + rc
		} else {
			msg = "no result from the deploy host"
		}
	}
	return Fail, "`" + engineName + " compose version` failed: " + msg + " (install a compose provider: podman-compose for podman, the docker compose plugin for docker; `depper host init` installs one)"
}

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
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
	if info.CreatedAt != "" {
		holder += " since " + clean(info.CreatedAt)
	}
	// Staleness uses the deploy host's clock and the lock directory's
	// mtime, which running operations refresh (the same rule as `depper
	// unlock`).
	age, stale := engine.LockAge(f["lock_now"], f["lock_mtime"], f["lock_old"])
	if age >= 0 {
		holder += fmt.Sprintf(", last refreshed %s ago", (time.Duration(age) * time.Second).String())
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
