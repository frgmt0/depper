package host

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/transport"
)

// Distribution families depper supports.
const (
	FamilyDebian = "debian"
	FamilyFedora = "fedora"
)

// Privilege of the admin session.
const (
	PrivRoot         = "root"
	PrivSudo         = "sudo"
	PrivSudoPassword = "sudo-password"
	PrivNone         = "none"
)

// Facts is what host init learned about the host in one round trip.
type Facts struct {
	AdminUser   string `json:"admin_user"`
	Privilege   string `json:"privilege"`
	SudoMessage string `json:"sudo_message,omitempty"`

	OSID           string `json:"os_id"`
	OSLike         string `json:"os_id_like"`
	VersionID      string `json:"version_id"`
	Codename       string `json:"version_codename,omitempty"`
	UbuntuCodename string `json:"ubuntu_codename,omitempty"`
	PrettyName     string `json:"pretty_name,omitempty"`
	Family         string `json:"family"`
	Systemd        bool   `json:"systemd"`
	Arch           string `json:"arch"`

	UserExists  bool     `json:"user_exists"`
	UserUID     int      `json:"user_uid,omitempty"`
	UserHome    string   `json:"user_home,omitempty"`
	UserShell   string   `json:"user_shell,omitempty"`
	UserGroups  []string `json:"user_groups,omitempty"`
	KeyPresent  bool     `json:"key_authorized"`
	Linger      bool     `json:"linger"`
	UserManager bool     `json:"user_manager"`
	SubUID      bool     `json:"subuid"`
	SubGID      bool     `json:"subgid"`

	Packages map[string]bool   `json:"packages"`
	Commands map[string]string `json:"commands"`

	DockerActive  bool `json:"docker_active"`
	DockerEnabled bool `json:"docker_enabled"`

	SSHDTest       bool              `json:"sshd_config_valid"`
	SSHDTestOutput string            `json:"-"`
	SSHD           map[string]string `json:"sshd"`
	SSHDPorts      []int             `json:"sshd_ports,omitempty"`
	SSHDInclude    bool              `json:"sshd_includes_dropins"`
	SSHDDropIn     string            `json:"-"`
	SSHDDropInSet  bool              `json:"sshd_dropin_present"`
	SSHDConflicts  []string          `json:"sshd_conflicts,omitempty"`
	SSHDUnit       string            `json:"sshd_unit,omitempty"`

	UFWActive        bool   `json:"ufw_active"`
	UFWPorts         []int  `json:"ufw_ports,omitempty"`
	FirewalldPresent bool   `json:"firewalld_present"`
	FirewalldActive  bool   `json:"firewalld_active"`
	FirewalldPorts   []int  `json:"firewalld_ports,omitempty"`
	FirewalldSSHSvc  bool   `json:"firewalld_ssh_service"`
	AutoUpdatesConf  bool   `json:"auto_updates_configured"`
	AutoUpdatesApply bool   `json:"-"`
	AutoUpdatesTimer string `json:"auto_updates_timer,omitempty"`
	TailscaleUp      bool   `json:"tailscale_up"`

	Record *Record `json:"record,omitempty"`
}

// pkgReq is one package requirement: satisfied when the package is installed
// or, when Cmd is set, when the command exists (curl-minimal and
// coreutils-single provide curl and coreutils on Fedora/RHEL).
type pkgReq struct {
	Pkg string
	Cmd string
}

func (f *Facts) has(r pkgReq) bool {
	if r.Cmd != "" && f.Commands[r.Cmd] != "" {
		return true
	}
	return f.Packages[r.Pkg]
}

// queriedPackages and queriedCommands are probed on every host.
var queriedPackages = []string{
	"rsync", "curl", "ca-certificates", "coreutils",
	"podman", "uidmap", "slirp4netns", "passt", "shadow-utils",
	"docker.io", "moby-engine", "cloudflared", "tailscale",
	"ufw", "firewalld", "unattended-upgrades", "dnf-automatic",
}

var queriedCommands = []string{
	"rsync", "curl", "sha256sum", "podman", "docker", "cloudflared", "tailscale",
	"ufw", "firewall-cmd", "firewall-offline-cmd", "sshd", "runuser", "newuidmap",
}

// classify maps os-release ID / ID_LIKE to a supported family.
func classify(id, like string) string {
	switch id {
	case "debian", "ubuntu":
		return FamilyDebian
	case "fedora", "rhel", "centos", "rocky", "almalinux":
		return FamilyFedora
	}
	for _, l := range strings.Fields(like) {
		switch l {
		case "debian", "ubuntu":
			return FamilyDebian
		case "fedora", "rhel", "centos":
			return FamilyFedora
		}
	}
	return ""
}

// p quotes an absolute remote path, prefixed with the test root.
func (r *Runner) p(path string) string { return shell.Quote(r.fsRoot + path) }

// raw returns an absolute remote path with the test root, unquoted (for
// building other values that are quoted later).
func (r *Runner) raw(path string) string { return r.fsRoot + path }

const factsMarker = "@@depper-host-facts"

// rootHeredoc is the delimiter of root scripts embedded in admin scripts.
const rootHeredoc = "DEPPER_AS_ROOT_EOF"

// asRoot returns admin-session lines that run the root script body as root:
// directly when the admin is root, else through `sudo -n` (never a password
// prompt). The body travels inside a quoted here-document, so it is part of
// the script on stdin and never part of any argv.
func (r *Runner) asRoot(body []string) ([]string, error) {
	inner := []string{"cd /"}
	if sp := r.systemPath(); sp != "" {
		inner = append(inner, "PATH="+shell.Quote(sp)+`:"$PATH"`, "export PATH")
	}
	inner = append(inner, body...)
	script := shell.Wrap(shell.Script(inner...))
	for _, line := range strings.Split(script, "\n") {
		if line == rootHeredoc {
			return nil, fmt.Errorf("internal error: root script contains its here-document delimiter")
		}
	}
	lines := []string{
		`depper_asroot() { if test "$(id -u)" = 0; then sh -s; else sudo -n sh -s; fi; }`,
		"depper_asroot <<'" + rootHeredoc + "'",
	}
	lines = append(lines, strings.Split(strings.TrimSuffix(script, "\n"), "\n")...)
	return append(lines, rootHeredoc), nil
}

// factsScript gathers every fact in one admin round trip. It only reads.
func (r *Runner) factsScript(o *Options, key PublicKey) (string, error) {
	u := shell.Quote(o.User)
	lines := []string{
		"set -u",
		"echo " + factsMarker,
		`printf 'admin_user=%s\n' "$(id -un 2>/dev/null || true)"`,
		`depper_uid=$(id -u 2>/dev/null || echo unknown)`,
		`if test "$depper_uid" = 0; then depper_priv=root`,
		`elif command -v sudo >/dev/null 2>&1; then`,
		`  if depper_msg=$(sudo -n true 2>&1 </dev/null); then depper_priv=sudo; else depper_priv=sudo-password; printf 'sudo_msg=%s\n' "$(printf '%s' "$depper_msg" | tr '\n' ' ' | head -c 300)"; fi`,
		`else depper_priv=none; fi`,
		`printf 'priv=%s\n' "$depper_priv"`,
		`osr=` + r.p("/etc/os-release"),
		`test -r "$osr" || osr=` + r.p("/usr/lib/os-release"),
		`if test -r "$osr"; then grep -E '^(ID|ID_LIKE|VERSION_ID|VERSION_CODENAME|UBUNTU_CODENAME|PRETTY_NAME)=' "$osr" | sed 's/^/os_/'; fi`,
		`if test -d ` + r.p("/run/systemd/system") + `; then echo systemd=1; else echo systemd=0; fi`,
		`printf 'arch=%s\n' "$(uname -m 2>/dev/null || true)"`,
	}
	akMatch := shell.Quote(key.Type + " " + key.Blob)
	root := []string{
		"set -u",
		"u=" + u,
		`pw=$(getent passwd "$u" 2>/dev/null || true)`,
		`if test -n "$pw"; then`,
		`  echo user_exists=1`,
		`  uid=$(printf '%s\n' "$pw" | cut -d: -f3)`,
		`  home=$(printf '%s\n' "$pw" | cut -d: -f6)`,
		`  printf 'user_uid=%s\n' "$uid"`,
		`  printf 'user_home=%s\n' "$home"`,
		`  printf 'user_shell=%s\n' "$(printf '%s\n' "$pw" | cut -d: -f7)"`,
		`  printf 'user_groups=%s\n' "$(id -nG "$u" 2>/dev/null || true)"`,
		`  ak="$home/.ssh/authorized_keys"`,
		`  if test -f "$ak" && awk -v k=` + akMatch + ` '{ for (i = 1; i < NF; i++) if ($i " " $(i+1) == k) f = 1 } END { exit !f }' "$ak"; then echo ak_key=1; else echo ak_key=0; fi`,
		`  if test -e ` + r.p("/var/lib/systemd/linger") + `/"$u"; then echo linger=1; else echo linger=0; fi`,
		`  rt=` + r.p("/run/user") + `/"$uid"`,
		`  if runuser -u "$u" -- env XDG_RUNTIME_DIR="$rt" DBUS_SESSION_BUS_ADDRESS="unix:path=$rt/bus" systemctl --user show-environment >/dev/null 2>&1; then echo usermgr=1; else echo usermgr=0; fi`,
		`  if grep -q "^$u:" ` + r.p("/etc/subuid") + ` 2>/dev/null; then echo subuid=1; else echo subuid=0; fi`,
		`  if grep -q "^$u:" ` + r.p("/etc/subgid") + ` 2>/dev/null; then echo subgid=1; else echo subgid=0; fi`,
		`else echo user_exists=0; fi`,
		`for p in ` + shell.Join(queriedPackages...) + `; do`,
		`  if command -v dpkg-query >/dev/null 2>&1; then`,
		`    if dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q 'ok installed'; then printf 'pkg_%s=1\n' "$p"; else printf 'pkg_%s=0\n' "$p"; fi`,
		`  elif command -v rpm >/dev/null 2>&1; then`,
		`    if rpm -q "$p" >/dev/null 2>&1 || rpm -q --whatprovides "$p" >/dev/null 2>&1; then printf 'pkg_%s=1\n' "$p"; else printf 'pkg_%s=0\n' "$p"; fi`,
		`  fi`,
		`done`,
		`for c in ` + shell.Join(queriedCommands...) + `; do printf 'cmd_%s=%s\n' "$c" "$(command -v "$c" 2>/dev/null || true)"; done`,
		`if systemctl is-active --quiet docker.service 2>/dev/null; then echo docker_active=1; fi`,
		`if systemctl is-enabled --quiet docker.service 2>/dev/null; then echo docker_enabled=1; fi`,
		`if command -v sshd >/dev/null 2>&1; then`,
		`  if t=$(sshd -T 2>&1); then`,
		`    echo sshd_t=1`,
		`    printf '%s\n' "$t" | awk '$1 == "passwordauthentication" || $1 == "kbdinteractiveauthentication" || $1 == "permitrootlogin" || $1 == "pubkeyauthentication" { print "sshd_" $1 "=" $2 }'`,
		`    printf 'sshd_ports=%s\n' "$(printf '%s\n' "$t" | awk '$1 == "port" { printf "%s ", $2 }')"`,
		`  else echo sshd_t=0; printf 'sshd_t_msg=%s\n' "$(printf '%s' "$t" | tr '\n' ' ' | head -c 300)"; fi`,
		`fi`,
		`if grep -Eiq '^[[:space:]]*Include[[:space:]]+(/etc/ssh/)?sshd_config\.d/\*\.conf' ` + r.p(sshdMain) + ` 2>/dev/null; then echo sshd_include=1; fi`,
		`if test -f ` + r.p(SSHDropIn) + `; then printf 'sshd_dropin=%s\n' "$(base64 < ` + r.p(SSHDropIn) + ` | tr -d '\n')"; fi`,
		// sshd keeps the first value it reads: the main file up to its
		// Include, then the drop-ins in lexical order. Only those read
		// before depper's drop-in can override it.
		`conflicts=""`,
		`yes_re='^[[:space:]]*(PasswordAuthentication|KbdInteractiveAuthentication|ChallengeResponseAuthentication|PermitRootLogin)[[:space:]]+"?yes"?([[:space:]]|$)'`,
		`if test -f ` + r.p(sshdMain) + ` && awk 'tolower($1) == "include" { exit } { print }' ` + r.p(sshdMain) + ` | grep -Eiq "$yes_re"; then conflicts="sshd_config"; fi`,
		`for f in ` + r.p(sshdDropInD) + `/*.conf; do`,
		`  test -f "$f" || continue`,
		`  b=${f##*/}`,
		`  test "$b" = ` + sshDropInName + ` && continue`,
		`  first=$(printf '%s\n%s\n' "$b" ` + sshDropInName + ` | LC_ALL=C sort | head -n 1)`,
		`  test "$first" = "$b" || continue`,
		`  if grep -Eiq "$yes_re" "$f"; then conflicts="$conflicts sshd_config.d/$b"; fi`,
		`done`,
		`printf 'sshd_conflicts=%s\n' "$conflicts"`,
		`if systemctl is-active --quiet ssh.service 2>/dev/null; then echo sshd_unit=ssh; elif systemctl is-active --quiet sshd.service 2>/dev/null; then echo sshd_unit=sshd; fi`,
		`if command -v ufw >/dev/null 2>&1; then`,
		`  s=$(ufw status 2>/dev/null || true)`,
		`  case "$s" in *"Status: active"*) echo ufw_active=1 ;; esac`,
		`  printf 'ufw_ports=%s\n' "$(printf '%s\n' "$s" | awk '$2 == "ALLOW" && $1 ~ /^[0-9]+(\/tcp)?$/ { sub("/tcp", "", $1); printf "%s ", $1 }')"`,
		`fi`,
		`if command -v firewall-cmd >/dev/null 2>&1 || command -v firewall-offline-cmd >/dev/null 2>&1; then`,
		`  echo firewalld_present=1`,
		`  if systemctl is-active --quiet firewalld.service 2>/dev/null; then echo firewalld_active=1; fc="firewall-cmd --permanent"; else fc=firewall-offline-cmd; fi`,
		`  printf 'firewalld_ports=%s\n' "$($fc --list-ports 2>/dev/null | tr '\n' ' ')"`,
		`  printf 'firewalld_services=%s\n' "$($fc --list-services 2>/dev/null | tr '\n' ' ')"`,
		`fi`,
		`f=` + r.p("/etc/apt/apt.conf.d/20auto-upgrades"),
		`if test -f "$f" && grep -q 'APT::Periodic::Unattended-Upgrade "1"' "$f"; then echo autoupd_apt=1; fi`,
		`f=` + r.p("/etc/dnf/automatic.conf"),
		`if test -f "$f" && grep -Eq '^[[:space:]]*upgrade_type[[:space:]]*=[[:space:]]*security' "$f"; then echo autoupd_security=1; fi`,
		`if test -f "$f" && grep -Eq '^[[:space:]]*apply_updates[[:space:]]*=[[:space:]]*(yes|true|1)' "$f"; then echo autoupd_apply=1; fi`,
		`for t in dnf-automatic-install.timer dnf5-automatic.timer; do if systemctl is-enabled --quiet "$t" 2>/dev/null; then printf 'autoupd_timer=%s\n' "$t"; break; fi; done`,
		`if command -v tailscale >/dev/null 2>&1 && tailscale status >/dev/null 2>&1; then echo tailscale_up=1; fi`,
		`if test -f ` + r.p(RecordPath) + `; then printf 'record=%s\n' "$(base64 < ` + r.p(RecordPath) + ` | tr -d '\n')"; fi`,
		`echo root_facts=1`,
	}
	rootLines, err := r.asRoot(root)
	if err != nil {
		return "", err
	}
	lines = append(lines, `case "$depper_priv" in root|sudo)`)
	lines = append(lines, rootLines...)
	lines = append(lines, ";;", "esac", "exit 0")
	return shell.Script(lines...), nil
}

// sshdMetaKeys are sshd_* facts that are not `sshd -T` settings.
var sshdMetaKeys = map[string]bool{
	"sshd_t": true, "sshd_t_msg": true, "sshd_ports": true, "sshd_include": true,
	"sshd_dropin": true, "sshd_conflicts": true, "sshd_unit": true,
}

// parseFacts turns the facts output into Facts.
func parseFacts(out string) (*Facts, map[string]string, error) {
	i := strings.Index(out, factsMarker+"\n")
	if i < 0 {
		return nil, nil, fmt.Errorf("the host did not run the facts script (is the admin login shell POSIX sh compatible?)")
	}
	m := map[string]string{}
	for _, line := range strings.Split(out[i+len(factsMarker)+1:], "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok && k != "" {
			m[k] = v
		}
	}
	f := &Facts{
		AdminUser:        clean(m["admin_user"]),
		Privilege:        m["priv"],
		SudoMessage:      clean(m["sudo_msg"]),
		OSID:             osValue(m["os_ID"]),
		OSLike:           osValue(m["os_ID_LIKE"]),
		VersionID:        osValue(m["os_VERSION_ID"]),
		Codename:         osValue(m["os_VERSION_CODENAME"]),
		UbuntuCodename:   osValue(m["os_UBUNTU_CODENAME"]),
		PrettyName:       osValue(m["os_PRETTY_NAME"]),
		Systemd:          m["systemd"] == "1",
		Arch:             clean(m["arch"]),
		UserExists:       m["user_exists"] == "1",
		UserHome:         clean(m["user_home"]),
		UserShell:        clean(m["user_shell"]),
		UserGroups:       strings.Fields(clean(m["user_groups"])),
		KeyPresent:       m["ak_key"] == "1",
		Linger:           m["linger"] == "1",
		UserManager:      m["usermgr"] == "1",
		SubUID:           m["subuid"] == "1",
		SubGID:           m["subgid"] == "1",
		Packages:         map[string]bool{},
		Commands:         map[string]string{},
		DockerActive:     m["docker_active"] == "1",
		DockerEnabled:    m["docker_enabled"] == "1",
		SSHDTest:         m["sshd_t"] == "1",
		SSHDTestOutput:   clean(m["sshd_t_msg"]),
		SSHD:             map[string]string{},
		SSHDInclude:      m["sshd_include"] == "1",
		SSHDConflicts:    strings.Fields(clean(m["sshd_conflicts"])),
		SSHDUnit:         m["sshd_unit"],
		UFWActive:        m["ufw_active"] == "1",
		UFWPorts:         ports(m["ufw_ports"]),
		FirewalldPresent: m["firewalld_present"] == "1",
		FirewalldActive:  m["firewalld_active"] == "1",
		FirewalldPorts:   ports(m["firewalld_ports"]),
		AutoUpdatesConf:  m["autoupd_apt"] == "1" || m["autoupd_security"] == "1",
		AutoUpdatesApply: m["autoupd_apply"] == "1",
		AutoUpdatesTimer: clean(m["autoupd_timer"]),
		TailscaleUp:      m["tailscale_up"] == "1",
	}
	f.Family = classify(f.OSID, f.OSLike)
	if uid, err := strconv.Atoi(m["user_uid"]); err == nil {
		f.UserUID = uid
	}
	for _, s := range strings.Fields(m["firewalld_services"]) {
		if s == "ssh" {
			f.FirewalldSSHSvc = true
		}
	}
	for k, v := range m {
		switch {
		case strings.HasPrefix(k, "pkg_"):
			f.Packages[strings.TrimPrefix(k, "pkg_")] = v == "1"
		case strings.HasPrefix(k, "cmd_"):
			f.Commands[strings.TrimPrefix(k, "cmd_")] = clean(v)
		case strings.HasPrefix(k, "sshd_") && !sshdMetaKeys[k]:
			f.SSHD[strings.TrimPrefix(k, "sshd_")] = clean(v)
		}
	}
	f.SSHDPorts = ports(m["sshd_ports"])
	if v, ok := m["sshd_dropin"]; ok {
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		if err != nil {
			return nil, nil, fmt.Errorf("the host returned an unreadable %s", SSHDropIn)
		}
		f.SSHDDropIn, f.SSHDDropInSet = string(data), true
	}
	if v, ok := m["record"]; ok && strings.TrimSpace(v) != "" {
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		if err == nil {
			var rec Record
			if json.Unmarshal(data, &rec) == nil {
				f.Record = &rec
			}
		}
	}
	return f, m, nil
}

// osValue unquotes an os-release value.
func osValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return clean(v)
}

// ports parses "22 80/tcp 443" into sorted, deduplicated TCP ports.
func ports(s string) []int {
	seen := map[int]bool{}
	var out []int
	for _, f := range strings.Fields(s) {
		if strings.Contains(f, "/") {
			if !strings.HasSuffix(f, "/tcp") {
				continue
			}
			f = strings.TrimSuffix(f, "/tcp")
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > 65535 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// gatherFacts runs the facts script as the admin.
func (r *Runner) gatherFacts(ctx context.Context, admin *transport.Remote, o *Options, key PublicKey) (*Facts, error) {
	script, err := r.factsScript(o, key)
	if err != nil {
		return nil, err
	}
	out, err := admin.Capture(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to %s as %s: %w", o.Host, o.Admin, err)
	}
	f, m, err := parseFacts(out)
	if err != nil {
		return nil, err
	}
	switch f.Privilege {
	case PrivRoot, PrivSudo:
		if m["root_facts"] != "1" {
			return nil, fmt.Errorf("gathering facts as root failed on the host")
		}
	case PrivSudoPassword:
		msg := ""
		if f.SudoMessage != "" {
			msg = " (sudo said: " + f.SudoMessage + ")"
		}
		return nil, fmt.Errorf("%s needs a password for sudo%s; depper connects with BatchMode and cannot type one. Connect as root, or allow passwordless sudo for %s (for example a file in /etc/sudoers.d with `%s ALL=(ALL) NOPASSWD: ALL`)", o.Admin, msg, o.Admin, o.Admin)
	default:
		return nil, fmt.Errorf("%s is not root and sudo is not installed; connect as root or as a user with passwordless sudo", o.Admin)
	}
	return f, nil
}
