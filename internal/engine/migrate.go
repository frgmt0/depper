package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/frgmt0/depper/internal/shell"
)

// MigrateResult describes `depper migrate`. JSON keys are stable.
type MigrateResult struct {
	Service string `json:"service"`
	// Status is "migrated", "planned" (--dry-run), or "already-migrated".
	Status     string        `json:"status"`
	LegacyBase string        `json:"legacy_base"`
	Base       string        `json:"base"`
	Plan       []string      `json:"plan"`
	Health     *HealthResult `json:"health"`
}

// legacyTunnelState is the Python deployer's tunnel.json.
type legacyTunnelState struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Service  string `json:"service"`
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type legacyTx struct {
	ID      string
	HasUnit bool
	Restart string
	Current string // release id the backup's "current" points at ("" if none)
	HasCur  bool
}

// legacyInspection is what the read-only inspection found.
type legacyInspection struct {
	Base, Legacy, Home    string
	New, Old, OldSymlink  bool
	BaseExists, OldLock   bool
	AppUnit, AppActive    bool
	TunnelUnit            bool
	TunnelEnabled         bool
	NewAppUnit, NewTunnel bool
	AppRestart            string
	Cloudflared           string
	Tunnel                *legacyTunnelState
	Current, Previous     string
	PreviousTx            string
	Releases              []string
	StartScripts          map[string]bool // release -> has .deployer-start.sh only
	Transactions          []legacyTx
}

func (e *Engine) inspectScript() string {
	base, legacy := e.baseAssignments()
	appUnit := `"$HOME/.config/systemd/user/"` + shell.Quote(e.Config.LegacyUnitName())
	tunnelUnit := `"$HOME/.config/systemd/user/"` + shell.Quote(e.Config.LegacyTunnelUnitName())
	newApp := `"$HOME/.config/systemd/user/"` + shell.Quote(e.Config.UnitName())
	newTunnel := `"$HOME/.config/systemd/user/"` + shell.Quote(e.Config.TunnelUnitName())
	flag := func(name, test string) string {
		return "if " + test + "; then echo " + name + "=yes; else echo " + name + "=no; fi"
	}
	lines := append([]string{"set +xv"}, shell.Prelude()...)
	lines = append(lines,
		"base="+base,
		"legacy="+legacy,
		`old="$legacy/.deployer"`,
		`echo @@paths`,
		`printf 'base=%s\nlegacy=%s\nhome=%s\n' "$base" "$legacy" "$HOME"`,
		`echo @@flags`,
		flag("new", `test -e "$base/.depper" || test -L "$base/.depper"`),
		flag("old", `test -d "$old" && ! test -L "$old"`),
		flag("old_symlink", `test -L "$old"`),
		flag("base_exists", `test -e "$base" || test -L "$base"`),
		flag("old_lock", `test -e "$old/lock"`),
		flag("app_unit", "test -f "+appUnit),
		flag("app_active", "systemctl --user is-active --quiet "+shell.Quote(e.Config.LegacyUnitName())+" 2>/dev/null"),
		flag("tunnel_unit", "test -f "+tunnelUnit),
		flag("tunnel_enabled", "systemctl --user is-enabled --quiet "+shell.Quote(e.Config.LegacyTunnelUnitName())+" 2>/dev/null"),
		flag("new_app_unit", "test -e "+newApp),
		flag("new_tunnel_unit", "test -e "+newTunnel),
		`echo @@app_restart`,
		`if test -f `+appUnit+`; then sed -n 's/^Restart=//p' `+appUnit+` | head -n 1; fi`,
		`echo @@cloudflared`,
		`command -v cloudflared 2>/dev/null || true`,
		`echo @@tunnel_json`,
		`if test -f "$old/tunnel.json" && ! test -L "$old/tunnel.json"; then head -c 65536 "$old/tunnel.json"; echo; fi`,
		`echo @@current`,
		`if test -L "$legacy/current"; then readlink "$legacy/current"; fi`,
		`echo @@previous`,
		`if test -L "$old/previous"; then readlink "$old/previous"; fi`,
		`echo @@previous_transaction`,
		`if test -f "$old/previous-transaction"; then head -c 4096 "$old/previous-transaction"; echo; fi`,
		`for d in "$legacy"/releases/*; do`,
		`  test -d "$d" || continue`,
		`  printf '@@release %s\n' "${d##*/}"`,
		`  if test -f "$d/.deployer-start.sh" && ! test -e "$d/`+StartScriptName+`"; then echo start=yes; else echo start=no; fi`,
		`done`,
		`for t in "$old"/transactions/*; do`,
		`  test -d "$t" || continue`,
		`  printf '@@tx %s\n' "${t##*/}"`,
		`  if test -f "$t/unit"; then echo unit=yes; printf 'restart=%s\n' "$(sed -n 's/^Restart=//p' "$t/unit" | head -n 1)"; else echo unit=no; fi`,
		`  if test -f "$t/current"; then printf 'current=%s\n' "$(head -c 4096 "$t/current")"; fi`,
		`done`,
	)
	return shell.Script(lines...)
}

func kv(body string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

func parseInspection(out string) (*legacyInspection, error) {
	in := &legacyInspection{StartScripts: map[string]bool{}}
	for _, s := range parseSections(out, "") {
		body := strings.TrimSuffix(s.Body, "\n")
		switch {
		case s.Name == "paths":
			m := kv(body)
			in.Base, in.Legacy, in.Home = m["base"], m["legacy"], m["home"]
		case s.Name == "flags":
			m := kv(body)
			yes := func(k string) bool { return m[k] == "yes" }
			in.New, in.Old, in.OldSymlink = yes("new"), yes("old"), yes("old_symlink")
			in.BaseExists, in.OldLock = yes("base_exists"), yes("old_lock")
			in.AppUnit, in.AppActive = yes("app_unit"), yes("app_active")
			in.TunnelUnit, in.TunnelEnabled = yes("tunnel_unit"), yes("tunnel_enabled")
			in.NewAppUnit, in.NewTunnel = yes("new_app_unit"), yes("new_tunnel_unit")
		case s.Name == "app_restart":
			in.AppRestart = strings.TrimSpace(body)
		case s.Name == "cloudflared":
			in.Cloudflared = strings.TrimSpace(body)
		case s.Name == "tunnel_json":
			if raw := strings.TrimSpace(body); raw != "" {
				var t legacyTunnelState
				if err := json.Unmarshal([]byte(raw), &t); err != nil {
					return nil, fmt.Errorf("the owned tunnel state (tunnel.json) is not valid JSON; refusing to migrate it")
				}
				in.Tunnel = &t
			}
		case s.Name == "current":
			in.Current = strings.TrimSpace(body)
		case s.Name == "previous":
			in.Previous = strings.TrimSpace(body)
		case s.Name == "previous_transaction":
			in.PreviousTx = strings.TrimSpace(body)
		case strings.HasPrefix(s.Name, "release "):
			id := strings.TrimPrefix(s.Name, "release ")
			if !ValidReleaseID(id) {
				continue
			}
			in.Releases = append(in.Releases, id)
			if kv(body)["start"] == "yes" {
				in.StartScripts[id] = true
			}
		case strings.HasPrefix(s.Name, "tx "):
			id := strings.TrimPrefix(s.Name, "tx ")
			if !ValidReleaseID(id) {
				continue
			}
			m := kv(body)
			cur, hasCur := m["current"]
			in.Transactions = append(in.Transactions, legacyTx{ID: id, HasUnit: m["unit"] == "yes", Restart: m["restart"], Current: releaseFromLink(cur), HasCur: hasCur})
		}
	}
	for _, p := range []string{in.Base, in.Legacy} {
		if !shell.IsPlainPath(p) || strings.HasSuffix(p, "/") {
			return nil, fmt.Errorf("remote host returned an invalid deployment path")
		}
	}
	sort.Strings(in.Releases)
	return in, nil
}

func validRestart(value, fallback string) string {
	switch value {
	case "on-failure", "always", "no":
		return value
	}
	return fallback
}

// migration is a validated conversion plan.
type migration struct {
	in          *legacyInspection
	plan        []string
	current     string
	previous    string
	previousTx  string
	moveBase    bool
	tunnelUnit  string
	tunnelYAML  string
	startCopies []string
}

func (e *Engine) planMigration(in *legacyInspection) (*migration, error) {
	cfg := e.Config
	if in.OldSymlink {
		return nil, fmt.Errorf("refusing a symlinked legacy metadata directory (%s/.deployer)", in.Legacy)
	}
	if in.New && in.Old {
		return nil, fmt.Errorf("both a depper deployment (%s/.depper) and a Python deployer deployment (%s/.deployer) exist; refusing to migrate. Remove one of them manually", in.Base, in.Legacy)
	}
	if !in.Old {
		if in.AppUnit {
			return nil, fmt.Errorf("the legacy unit %s exists but there is no legacy deployment directory at %s; remove the unit manually", cfg.LegacyUnitName(), in.Legacy)
		}
		return nil, nil
	}
	if in.OldLock {
		return nil, fmt.Errorf("a Python deployer operation is running or left a stale lock (%s/.deployer/lock); wait for it or remove the lock directory", in.Legacy)
	}
	if in.NewAppUnit || in.NewTunnel {
		return nil, fmt.Errorf("depper units for %s already exist in ~/.config/systemd/user; refusing to overwrite them", cfg.Service.Name)
	}
	m := &migration{in: in, moveBase: in.Base != in.Legacy}
	if m.moveBase && in.BaseExists {
		return nil, fmt.Errorf("%s already exists; refusing to move the legacy deployment over it", in.Base)
	}
	if in.Current != "" {
		if m.current = releaseFromLink(in.Current); m.current == "" {
			return nil, fmt.Errorf("the legacy current link (%s) does not point at a release; refusing to migrate", in.Current)
		}
	}
	if in.Previous != "" {
		m.previous = releaseFromLink(in.Previous)
	}
	if in.PreviousTx != "" {
		id := path.Base(in.PreviousTx)
		for _, tx := range in.Transactions {
			if tx.ID == id {
				m.previousTx = id
			}
		}
	}
	if t := in.Tunnel; t != nil {
		if !uuidRe.MatchString(t.ID) || t.Name == "" || shell.HasControl(t.Name+t.Hostname+t.Service) || t.Hostname == "" || t.Service == "" {
			return nil, fmt.Errorf("the owned tunnel state (tunnel.json) is invalid; refusing to migrate it")
		}
		if !shell.IsPlainPath(in.Home) {
			return nil, fmt.Errorf("remote host returned an invalid home directory for cloudflared")
		}
		m.tunnelYAML = CloudflaredConfig(t.ID, in.Home, t.Hostname, t.Service)
		if in.TunnelUnit {
			if !shell.IsPlainPath(in.Cloudflared) {
				return nil, fmt.Errorf("cloudflared is not installed at a safe absolute path on the deploy host; it is required to migrate the tunnel connector")
			}
			m.tunnelUnit = TunnelUnitFile(cfg.Service.Name, in.Cloudflared, in.Base+"/.depper/cloudflared.yml", t.ID)
		}
	}
	for _, id := range in.Releases {
		if in.StartScripts[id] {
			m.startCopies = append(m.startCopies, id)
		}
	}
	units := cfg.LegacyUnitName()
	if in.TunnelUnit {
		units += " and " + cfg.LegacyTunnelUnitName()
	}
	m.plan = append(m.plan, "stop and disable "+units)
	if m.moveBase {
		m.plan = append(m.plan, "move "+in.Legacy+" to "+in.Base)
	}
	m.plan = append(m.plan, "rename "+in.Base+"/.deployer to .depper")
	if m.current != "" {
		m.plan = append(m.plan, "point current at releases/"+m.current+" (relative)")
	}
	if m.previous != "" {
		m.plan = append(m.plan, "move .deployer/previous to previous -> releases/"+m.previous+" (relative)")
	}
	rewritten := 0
	for _, tx := range in.Transactions {
		if tx.HasUnit || tx.Current != "" {
			rewritten++
		}
	}
	if m.previousTx != "" || rewritten > 0 {
		m.plan = append(m.plan, fmt.Sprintf("rewrite previous-transaction and %d transaction backups (release links, unit files)", rewritten))
	}
	if len(m.startCopies) > 0 {
		m.plan = append(m.plan, fmt.Sprintf("add %s next to .deployer-start.sh in %d releases", StartScriptName, len(m.startCopies)))
	}
	m.plan = append(m.plan, "install "+cfg.UnitName())
	if m.tunnelYAML != "" {
		m.plan = append(m.plan, "regenerate cloudflared.yml (tunnel "+in.Tunnel.Name+" "+in.Tunnel.ID+" is kept unchanged)")
	}
	if m.tunnelUnit != "" {
		m.plan = append(m.plan, "install "+cfg.TunnelUnitName())
	}
	m.plan = append(m.plan, "remove "+units+" and reload systemd")
	start := "start " + cfg.UnitName()
	if m.tunnelUnit != "" && in.TunnelEnabled {
		start += " and " + cfg.TunnelUnitName()
	}
	m.plan = append(m.plan, start)
	return m, nil
}

// writeLines writes content to the file at dest (a shell word) atomically.
func writeLines(dest, content, mode string) []string {
	return []string{
		"printf %s " + shell.Quote(base64.StdEncoding.EncodeToString([]byte(content))) + " | base64 -d > " + dest + ".depper-new",
		"chmod " + mode + " " + dest + ".depper-new",
		"mv -f " + dest + ".depper-new " + dest,
	}
}

func (e *Engine) migrateScript(m *migration) string {
	cfg := e.Config
	in := m.in
	appOld := `"$HOME/.config/systemd/user/"` + shell.Quote(cfg.LegacyUnitName())
	tunnelOld := `"$HOME/.config/systemd/user/"` + shell.Quote(cfg.LegacyTunnelUnitName())
	tunnelNew := `"$HOME/.config/systemd/user/"` + shell.Quote(cfg.TunnelUnitName())
	lines := append([]string{"set +xv"}, shell.Prelude()...)
	lines = append(lines,
		"umask 077",
		"base="+shell.Quote(in.Base),
		"legacy="+shell.Quote(in.Legacy),
		`meta="$base/.depper"`,
		"unit="+unitPathExpr(cfg.UnitName()),
		`mkdir -p "$(dirname "$unit")"`,
		"systemctl --user disable --now "+shell.Quote(cfg.LegacyUnitName())+" >/dev/null 2>&1 || true",
	)
	if in.TunnelUnit {
		lines = append(lines, "systemctl --user disable --now "+shell.Quote(cfg.LegacyTunnelUnitName())+" >/dev/null 2>&1 || true")
	}
	if m.moveBase {
		lines = append(lines,
			`test ! -e "$base" || { echo "The depper deployment directory already exists" >&2; exit 1; }`,
			`mkdir -p "$(dirname "$base")"`,
			`mv "$legacy" "$base"`,
		)
	}
	lines = append(lines,
		`mv "$base/.deployer" "$meta"`,
		`chmod 700 "$meta"`,
		`mkdir -p "$meta/transactions" "$base/releases"`,
		`chmod 700 "$meta/transactions"`,
		`for f in "$meta"/environment.next-* "$meta"/unit.next-* "$meta"/tunnel-unit.next; do rm -f "$f"; done`,
	)
	if m.current != "" {
		lines = append(lines, setLinkLines("current", "releases/"+m.current, false)...)
	}
	lines = append(lines, `rm -f "$meta/previous"`)
	if m.previous != "" {
		lines = append(lines, setLinkLines("previous", "releases/"+m.previous, false)...)
	}
	if m.previousTx != "" {
		lines = append(lines, writeLines(`"$meta/previous-transaction"`, "transactions/"+m.previousTx+"\n", "600")...)
	} else {
		lines = append(lines, `rm -f "$meta/previous-transaction"`)
	}
	for _, tx := range in.Transactions {
		dir := `"$meta/transactions/"` + shell.Quote(tx.ID)
		if tx.HasCur {
			if tx.Current != "" {
				lines = append(lines, writeLines(dir+`"/current"`, "releases/"+tx.Current+"\n", "600")...)
			} else {
				lines = append(lines, "rm -f "+dir+`"/current"`, ": > "+dir+`"/no-current"`)
			}
		}
		if tx.HasUnit {
			restart := validRestart(tx.Restart, cfg.Service.Restart)
			lines = append(lines, writeLines(dir+`"/unit"`, ProcessUnit(cfg.Service.Name, restart, in.Base), "600")...)
		}
	}
	for _, id := range m.startCopies {
		rel := `"$base/releases/"` + shell.Quote(id)
		lines = append(lines, "cp -p "+rel+`"/.deployer-start.sh" `+rel+`"/`+StartScriptName+`"`)
	}
	restart := validRestart(in.AppRestart, cfg.Service.Restart)
	lines = append(lines, writeLines(`"$unit"`, ProcessUnit(cfg.Service.Name, restart, in.Base), "600")...)
	if m.tunnelYAML != "" {
		lines = append(lines, writeLines(`"$meta/cloudflared.yml"`, m.tunnelYAML, "600")...)
	}
	if m.tunnelUnit != "" {
		lines = append(lines, writeLines(tunnelNew, m.tunnelUnit, "600")...)
	}
	lines = append(lines,
		"rm -f "+appOld+" "+tunnelOld,
		"systemctl --user daemon-reload",
		"systemctl --user enable "+shell.Quote(cfg.UnitName())+" >/dev/null",
	)
	if m.current != "" {
		lines = append(lines, snapshotLines(m.current)...)
		lines = append(lines, "systemctl --user restart "+shell.Quote(cfg.UnitName()))
	}
	if m.tunnelUnit != "" && in.TunnelEnabled {
		lines = append(lines, "systemctl --user enable --now "+shell.Quote(cfg.TunnelUnitName())+" >/dev/null")
	}
	return shell.Script(lines...)
}

// Migrate converts a deployment made by the Python deployer into the depper
// layout: it stops the legacy units, moves or renames the metadata, rewrites
// links, transaction backups, and units, and restarts. With dryRun it only
// inspects the host (read-only) and returns the plan.
func (e *Engine) Migrate(ctx context.Context, dryRun bool) (*MigrateResult, error) {
	out, err := e.Remote.Capture(ctx, e.inspectScript())
	if err != nil {
		return nil, err
	}
	in, err := parseInspection(out)
	if err != nil {
		return nil, err
	}
	result := &MigrateResult{Service: e.Config.Service.Name, LegacyBase: in.Legacy, Base: in.Base, Plan: []string{}}
	m, err := e.planMigration(in)
	if err != nil {
		return nil, err
	}
	if m == nil {
		if in.New {
			result.Status = "already-migrated"
			return result, nil
		}
		return nil, fmt.Errorf("no Python deployer deployment was found for %s (looked in %s)", e.Config.Service.Name, in.Legacy)
	}
	result.Plan = m.plan
	if dryRun {
		result.Status = "planned"
		return result, nil
	}
	legacyLock := in.Legacy + "/.deployer/lock"
	if err := e.lockAt(ctx, legacyLock); err != nil {
		return nil, err
	}
	l := &Layout{Base: in.Base, Home: in.Home, State: LayoutDepper}
	e.printf("Migrating %s from %s to %s\n", e.Config.Service.Name, in.Legacy, in.Base)
	runErr := e.Remote.Run(ctx, e.migrateScript(m))
	unlockScript := shell.Script(
		"set +xv", "set -eu",
		`for lock in `+shell.Quote(l.Lock())+" "+shell.Quote(legacyLock)+`; do if test -d "$lock"; then rm -f "$lock/info"; rmdir "$lock"; fi; done`,
	)
	if uerr := e.Remote.Run(withoutCancel(ctx), unlockScript); uerr != nil {
		runErr = joinErrors(runErr, "releasing the migration lock failed", uerr)
	}
	if runErr != nil {
		return nil, fmt.Errorf("migration failed; inspect %s and %s on the host: %w", in.Legacy, in.Base, runErr)
	}
	result.Status = "migrated"
	if m.current != "" {
		health, err := e.WaitHealthy(ctx, l)
		result.Health = health
		if err != nil {
			return result, fmt.Errorf("migration finished, but the service is not healthy: %w", err)
		}
	}
	return result, nil
}
