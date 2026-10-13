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

// Metadata locations reported by the inspection (legacyInspection.Kind).
const (
	metaNone    = "none"    // no legacy metadata found
	metaLegacy  = "legacy"  // <legacy>/.deployer: not migrated yet
	metaMoved   = "moved"   // <base>/.deployer: base moved, not renamed yet
	metaRenamed = "renamed" // <base>/.depper with a migration marker
)

// legacyInspection is what the read-only inspection found.
type legacyInspection struct {
	Base, Legacy, Home string
	// Kind is where the metadata directory is (meta* constants) and
	// MetaDir its path.
	Kind, MetaDir         string
	New, OldSymlink       bool
	BaseExists, OldLock   bool
	AppUnit, AppActive    bool
	AppEnabled            bool
	TunnelUnit            bool
	TunnelEnabled         bool
	NewAppUnit, NewTunnel bool
	AppRestart            string
	Cloudflared           string
	Tunnel                *legacyTunnelState
	Current, Previous     string
	// PreviousLink is <base>/previous (written by an interrupted migration).
	PreviousLink string
	PreviousTx   string
	// Marker holds the facts recorded when the migration started; nil when
	// no migration is in progress.
	Marker       map[string]string
	Releases     []string
	StartScripts map[string]bool // release -> has .deployer-start.sh only
	Transactions []legacyTx
}

// resuming reports an interrupted migration.
func (in *legacyInspection) resuming() bool { return in.Marker != nil }

// compatLinkLines set $compat to yes when $legacy is depper's compatibility
// symlink.
func (e *Engine) compatLinkLines() []string {
	return []string{
		`compat=no`,
		`if test -L "$legacy" && test "$(readlink "$legacy")" = ` + shell.Quote(CompatLinkTarget(e.Config)) + `; then compat=yes; fi`,
	}
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
	lines = append(lines, "base="+base, "legacy="+legacy)
	lines = append(lines, e.compatLinkLines()...)
	lines = append(lines,
		// Where the metadata is: still at the legacy base, at the new base
		// but not renamed (interrupted), renamed with the migration marker
		// (interrupted), or nowhere.
		`if test "$compat" = no && { test -d "$legacy/.deployer" || test -L "$legacy/.deployer"; }; then kind=legacy; src=$legacy; md="$legacy/.deployer"`,
		`elif test -d "$base/.deployer" && ! test -L "$base/.deployer"; then kind=moved; src=$base; md="$base/.deployer"`,
		`elif test -f "$base/.depper/`+MigrationMarker+`"; then kind=renamed; src=$base; md="$base/.depper"`,
		`else kind=none; src=$legacy; md="$legacy/.deployer"; fi`,
		`echo @@paths`,
		`printf 'base=%s\nlegacy=%s\nhome=%s\nkind=%s\nmeta=%s\n' "$base" "$legacy" "$HOME" "$kind" "$md"`,
		`echo @@flags`,
		flag("new", `test -e "$base/.depper" || test -L "$base/.depper"`),
		flag("old_symlink", `test -L "$md"`),
		flag("base_exists", `test -e "$base" || test -L "$base"`),
		flag("old_lock", `test -e "$md/lock"`),
		flag("app_unit", "test -f "+appUnit),
		flag("app_active", "systemctl --user is-active --quiet "+shell.Quote(e.Config.LegacyUnitName())+" 2>/dev/null"),
		flag("app_enabled", "systemctl --user is-enabled --quiet "+shell.Quote(e.Config.LegacyUnitName())+" 2>/dev/null"),
		flag("tunnel_unit", "test -f "+tunnelUnit),
		flag("tunnel_enabled", "systemctl --user is-enabled --quiet "+shell.Quote(e.Config.LegacyTunnelUnitName())+" 2>/dev/null"),
		flag("new_app_unit", "test -e "+newApp),
		flag("new_tunnel_unit", "test -e "+newTunnel),
		`echo @@app_restart`,
		`if test -f `+appUnit+`; then sed -n 's/^Restart=//p' `+appUnit+` | head -n 1; fi`,
		`echo @@cloudflared`,
		`command -v cloudflared 2>/dev/null || true`,
		`echo @@tunnel_json`,
		`if test -f "$md/tunnel.json" && ! test -L "$md/tunnel.json"; then head -c 65536 "$md/tunnel.json"; echo; fi`,
		`echo @@current`,
		`if test -L "$src/current"; then readlink "$src/current"; fi`,
		`echo @@previous`,
		`if test -L "$md/previous"; then readlink "$md/previous"; fi`,
		`echo @@previous_link`,
		`if test -L "$src/previous"; then readlink "$src/previous"; fi`,
		`echo @@previous_transaction`,
		`if test -f "$md/previous-transaction"; then head -c 4096 "$md/previous-transaction"; echo; fi`,
		`if test -f "$md/`+MigrationMarker+`" && ! test -L "$md/`+MigrationMarker+`"; then echo @@marker; head -c 4096 "$md/`+MigrationMarker+`"; echo; fi`,
		`for d in "$src"/releases/*; do`,
		`  test -d "$d" || continue`,
		`  printf '@@release %s\n' "${d##*/}"`,
		`  if test -f "$d/.deployer-start.sh" && ! test -e "$d/`+StartScriptName+`"; then echo start=yes; else echo start=no; fi`,
		`done`,
		`for t in "$md"/transactions/*; do`,
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
			in.Base, in.Legacy, in.Home, in.Kind, in.MetaDir = m["base"], m["legacy"], m["home"], m["kind"], m["meta"]
		case s.Name == "flags":
			m := kv(body)
			yes := func(k string) bool { return m[k] == "yes" }
			in.New, in.OldSymlink = yes("new"), yes("old_symlink")
			in.BaseExists, in.OldLock = yes("base_exists"), yes("old_lock")
			in.AppUnit, in.AppActive, in.AppEnabled = yes("app_unit"), yes("app_active"), yes("app_enabled")
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
		case s.Name == "previous_link":
			in.PreviousLink = strings.TrimSpace(body)
		case s.Name == "previous_transaction":
			in.PreviousTx = strings.TrimSpace(body)
		case s.Name == "marker":
			in.Marker = kv(body)
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
		if !shell.IsSafePath(p) || strings.HasSuffix(p, "/") {
			return nil, fmt.Errorf("the deployment path on the host (%q) is not supported: depper needs an absolute path made only of %s", p, shell.SafePathChars)
		}
	}
	switch {
	case in.Kind == metaLegacy && in.MetaDir == in.Legacy+"/.deployer":
	case in.Kind == metaMoved && in.MetaDir == in.Base+"/.deployer":
	case in.Kind == metaRenamed && in.MetaDir == in.Base+"/.depper":
	case in.Kind == metaNone:
	default:
		return nil, fmt.Errorf("remote host returned an invalid migration inspection")
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
	appRestart  string
	appActive   bool
	appEnabled  bool
	hasTunnel   bool // the legacy tunnel connector unit exists (or existed)
	tunnelOn    bool // ... and was enabled
	tunnelUnit  string
	tunnelYAML  string
	startCopies []string
}

// marker is the content of the migration marker: the facts about the
// legacy units recorded before they were stopped, so a resumed migration
// restores the same state.
func (m *migration) marker() string {
	yn := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	return shell.Script(
		"app_restart="+m.appRestart,
		"app_active="+yn(m.appActive),
		"app_enabled="+yn(m.appEnabled),
		"tunnel_unit="+yn(m.hasTunnel),
		"tunnel_enabled="+yn(m.tunnelOn),
	)
}

func (e *Engine) planMigration(in *legacyInspection) (*migration, error) {
	cfg := e.Config
	if in.OldSymlink {
		return nil, fmt.Errorf("refusing a symlinked legacy metadata directory (%s)", in.MetaDir)
	}
	if (in.Kind == metaLegacy || in.Kind == metaMoved) && in.New {
		return nil, fmt.Errorf("both a depper deployment (%s/.depper) and a Python deployer deployment (%s) exist; refusing to migrate. Remove one of them manually", in.Base, in.MetaDir)
	}
	if in.Kind == metaNone {
		if in.AppUnit {
			return nil, fmt.Errorf("the legacy unit %s exists but there is no legacy deployment directory at %s; remove the unit manually", cfg.LegacyUnitName(), in.Legacy)
		}
		return nil, nil
	}
	resume := in.resuming()
	if in.OldLock {
		return nil, fmt.Errorf("a Python deployer or depper operation is running or left a stale lock (%s/lock); wait for it, or remove that lock directory if nothing is running, then run `depper migrate` again", in.MetaDir)
	}
	if !resume && (in.NewAppUnit || in.NewTunnel) {
		return nil, fmt.Errorf("depper units for %s already exist in ~/.config/systemd/user; refusing to overwrite them", cfg.Service.Name)
	}
	m := &migration{in: in, moveBase: in.Base != in.Legacy}
	if m.moveBase && in.Kind == metaLegacy && in.BaseExists {
		return nil, fmt.Errorf("%s already exists; refusing to move the legacy deployment over it", in.Base)
	}
	if resume {
		f := in.Marker
		m.appRestart = validRestart(f["app_restart"], cfg.Service.Restart)
		m.appActive, m.appEnabled = f["app_active"] == "yes", f["app_enabled"] == "yes"
		m.hasTunnel = in.TunnelUnit || f["tunnel_unit"] == "yes"
		m.tunnelOn = f["tunnel_enabled"] == "yes"
	} else {
		m.appRestart = validRestart(in.AppRestart, cfg.Service.Restart)
		m.appActive, m.appEnabled = in.AppActive, in.AppEnabled
		m.hasTunnel, m.tunnelOn = in.TunnelUnit, in.TunnelEnabled
	}
	if in.Current != "" {
		if m.current = releaseFromLink(in.Current); m.current == "" {
			return nil, fmt.Errorf("the legacy current link (%s) does not point at a release; refusing to migrate", in.Current)
		}
	}
	switch {
	case in.Previous != "":
		m.previous = releaseFromLink(in.Previous)
	case resume && in.PreviousLink != "":
		m.previous = releaseFromLink(in.PreviousLink)
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
		if m.hasTunnel {
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
	if m.hasTunnel {
		units += " and " + cfg.LegacyTunnelUnitName()
	}
	if resume {
		m.plan = append(m.plan, "resume the interrupted migration (marker "+in.MetaDir+"/"+MigrationMarker+"); every step below is skipped when already done")
	}
	m.plan = append(m.plan, "stop and disable "+units)
	if m.moveBase {
		if in.Kind == metaLegacy {
			m.plan = append(m.plan, "move "+in.Legacy+" to "+in.Base)
		}
		m.plan = append(m.plan, "link "+in.Legacy+" -> "+CompatLinkTarget(cfg)+" (relative), so absolute paths recorded inside existing releases keep working (for example the interpreter paths in a Python virtualenv's scripts)")
	}
	if in.Kind != metaRenamed {
		m.plan = append(m.plan, "rename "+in.Base+"/.deployer to .depper")
	}
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
	if m.tunnelUnit != "" && m.tunnelOn {
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
		"mv -Tf " + dest + ".depper-new " + dest,
	}
}

// migrateScript converts the layout. Every step is idempotent and guarded,
// so running it again after an interruption resumes and completes the
// migration. The marker (written into the metadata directory first, and
// moved with it) is removed only at the very end.
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
	)
	lines = append(lines, e.compatLinkLines()...)
	lines = append(lines,
		`if test "$compat" = no && test -d "$legacy/.deployer"; then md="$legacy/.deployer"`,
		`elif test -d "$base/.deployer"; then md="$base/.deployer"`,
		`elif test -d "$meta"; then md="$meta"`,
		`else echo "The deployment metadata directory is missing" >&2; exit 1; fi`,
		`if ! test -f "$md/`+MigrationMarker+`"; then`,
	)
	for _, line := range writeLines(`"$md/`+MigrationMarker+`"`, m.marker(), "600") {
		lines = append(lines, "  "+line)
	}
	lines = append(lines,
		`fi`,
		`mkdir -p "$(dirname "$unit")"`,
		"systemctl --user disable --now "+shell.Quote(cfg.LegacyUnitName())+" >/dev/null 2>&1 || true",
	)
	if m.hasTunnel {
		lines = append(lines, "systemctl --user disable --now "+shell.Quote(cfg.LegacyTunnelUnitName())+" >/dev/null 2>&1 || true")
	}
	if m.moveBase {
		link := shell.Quote(CompatLinkTarget(cfg))
		lines = append(lines,
			`if test "$compat" = no && test -d "$legacy/.deployer"; then`,
			`  test ! -e "$base" || { echo "The depper deployment directory already exists" >&2; exit 1; }`,
			`  mkdir -p "$(dirname "$base")"`,
			`  mv -T "$legacy" "$base"`,
			`fi`,
			// Compatibility link: absolute paths inside existing releases
			// (virtualenv shebangs, generated configs) keep resolving.
			`if ! test -e "$legacy" && ! test -L "$legacy"; then ln -s `+link+` "$legacy"`,
			`elif ! { test -L "$legacy" && test "$(readlink "$legacy")" = `+link+`; }; then echo "Warning: $legacy exists, so the compatibility link to the moved deployment was not created" >&2; fi`,
		)
	}
	lines = append(lines,
		`if test -d "$base/.deployer"; then`,
		`  test ! -e "$meta" || { echo "Both $base/.deployer and $meta exist; refusing to continue" >&2; exit 1; }`,
		`  mv -T "$base/.deployer" "$meta"`,
		`fi`,
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
	lines = append(lines, writeLines(`"$unit"`, ProcessUnit(cfg.Service.Name, m.appRestart, in.Base), "600")...)
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
	if m.tunnelUnit != "" && m.tunnelOn {
		lines = append(lines, "systemctl --user enable --now "+shell.Quote(cfg.TunnelUnitName())+" >/dev/null")
	}
	lines = append(lines, `rm -f "$meta/`+MigrationMarker+`"`)
	return shell.Script(lines...)
}

// legacyRestartScript restarts the legacy units after a failed migration,
// as long as the Python deployer's metadata (.deployer) still exists and
// its unit files are still installed. It prints "restarted" when it did.
func (e *Engine) legacyRestartScript(m *migration) string {
	cfg := e.Config
	appOld := `"$HOME/.config/systemd/user/"` + shell.Quote(cfg.LegacyUnitName())
	tunnelOld := `"$HOME/.config/systemd/user/"` + shell.Quote(cfg.LegacyTunnelUnitName())
	lines := append([]string{"set +xv"}, shell.Prelude()...)
	lines = append(lines,
		"base="+shell.Quote(m.in.Base),
		"legacy="+shell.Quote(m.in.Legacy),
	)
	lines = append(lines, e.compatLinkLines()...)
	lines = append(lines,
		`if ! { test "$compat" = no && test -d "$legacy/.deployer"; } && ! test -d "$base/.deployer"; then exit 0; fi`,
		`if ! test -f `+appOld+`; then exit 0; fi`,
		"systemctl --user daemon-reload",
	)
	if m.appEnabled {
		lines = append(lines, "systemctl --user enable "+shell.Quote(cfg.LegacyUnitName())+" >/dev/null")
	}
	if m.appActive {
		lines = append(lines, "systemctl --user start "+shell.Quote(cfg.LegacyUnitName()))
	}
	if m.hasTunnel && m.tunnelOn {
		lines = append(lines, "if test -f "+tunnelOld+"; then systemctl --user enable --now "+shell.Quote(cfg.LegacyTunnelUnitName())+" >/dev/null; fi")
	}
	lines = append(lines, "echo restarted")
	return shell.Script(lines...)
}

// migrationUnlockScript releases the migration lock wherever the metadata
// directory (which contains the lock) ended up, only while it still carries
// token.
func migrationUnlockScript(in *legacyInspection, token string) string {
	candidates := []string{in.Legacy + "/.deployer/lock", in.Base + "/.deployer/lock", in.Base + "/.depper/lock"}
	quoted := make([]string, len(candidates))
	for i, c := range candidates {
		quoted[i] = shell.Quote(c)
	}
	return shell.Script(
		"set +xv",
		"set -eu",
		`removed=no`,
		`for lock in `+strings.Join(quoted, " ")+`; do`,
		`  if test -d "$lock" && ! test -L "$lock" && grep -Fq -e `+shell.Quote(tokenPattern(token))+` "$lock/info" 2>/dev/null; then rm -f "$lock/info"; rmdir "$lock"; removed=yes; break; fi`,
		`done`,
		`test "$removed" = yes || { echo "The migration lock is now held by someone else (or was removed); it was not removed" >&2; exit 1; }`,
	)
}

// Migrate converts a deployment made by the Python deployer into the depper
// layout: it stops the legacy units, moves or renames the metadata, rewrites
// links, transaction backups, and units, and restarts. With dryRun it only
// inspects the host (read-only) and returns the plan.
//
// An interrupted migration is detected (migration marker) and resumed by
// running Migrate again; if a migration fails while the legacy metadata
// still exists, the legacy units are restarted.
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
	if err := e.lockAt(ctx, in.MetaDir+"/lock"); err != nil {
		return nil, err
	}
	token := e.held.token
	l := &Layout{Base: in.Base, Home: in.Home, State: LayoutDepper}
	if in.resuming() {
		e.printf("Resuming the interrupted migration of %s from %s to %s\n", e.Config.Service.Name, in.Legacy, in.Base)
	} else {
		e.printf("Migrating %s from %s to %s\n", e.Config.Service.Name, in.Legacy, in.Base)
	}
	runErr := e.Remote.Run(ctx, e.migrateScript(m))
	cctx := withoutCancel(ctx)
	restarted := false
	if runErr != nil {
		rout, rerr := e.Remote.Capture(cctx, e.legacyRestartScript(m))
		switch {
		case rerr != nil:
			runErr = joinErrors(runErr, "restarting the legacy units failed", rerr)
		case strings.Contains(rout, "restarted"):
			restarted = true
		}
	}
	if _, uerr := e.Remote.Capture(cctx, migrationUnlockScript(in, token)); uerr != nil {
		runErr = joinErrors(runErr, "releasing the migration lock failed", uerr)
	}
	e.held = nil
	if runErr != nil {
		hint := "run `depper migrate` again to finish it (it resumes where it stopped)"
		if restarted {
			hint = "the Python deployer's units were restarted; " + hint
		}
		return nil, fmt.Errorf("migration failed; %s. Details: %w", hint, runErr)
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
