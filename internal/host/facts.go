package host

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
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
	FamilyArch   = "arch"
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

	UserExists bool     `json:"user_exists"`
	UserUID    int      `json:"user_uid,omitempty"`
	UserHome   string   `json:"user_home,omitempty"`
	UserShell  string   `json:"user_shell,omitempty"`
	UserGroups []string `json:"user_groups,omitempty"`
	// UserPassword classifies the shadow password field without revealing
	// it: "locked" (exactly "!" or "!!"), "star" ("*"), "empty", or "set".
	UserPassword string `json:"user_password,omitempty"`
	// KeyPresent is true when an uncommented authorized_keys line holds the
	// key without options; KeyRestricted when the only matches carry
	// options (command=, from=, restrict, ...).
	KeyPresent    bool `json:"key_authorized"`
	KeyRestricted bool `json:"key_restricted"`
	Linger        bool `json:"linger"`
	UserManager   bool `json:"user_manager"`
	SubUID        bool `json:"subuid"`
	SubGID        bool `json:"subgid"`

	Packages map[string]bool   `json:"packages"`
	Commands map[string]string `json:"commands"`

	// DockerActive and DockerEnabled are docker.service's is-active and
	// is-enabled; DockerSocketActive and DockerSocketEnabled docker.socket's
	// (socket activation, Arch's default: docker.service stays disabled).
	DockerActive        bool `json:"docker_active"`
	DockerEnabled       bool `json:"docker_enabled"`
	DockerSocketActive  bool `json:"docker_socket_active"`
	DockerSocketEnabled bool `json:"docker_socket_enabled"`
	// DockerGID is the gid of the docker group ("" when there is none).
	DockerGID string `json:"docker_gid,omitempty"`
	// ManagerPID is the main pid of user@UID.service (0 when it is not
	// running) and ManagerGroups the supplementary gids of that process
	// (ManagerGroupsKnown when /proc could be read): its services inherit
	// them, so a manager started before the user joined a group lacks it.
	ManagerPID         int      `json:"user_manager_pid,omitempty"`
	ManagerGroups      []string `json:"user_manager_groups,omitempty"`
	ManagerGroupsKnown bool     `json:"user_manager_groups_known"`
	// ComposePlugin is the docker-compose CLI plugin or binary on the host
	// ("" when there is none); podman runs it as its compose provider.
	ComposePlugin string `json:"compose_plugin,omitempty"`
	// PodmanSocketEnabled is what `systemctl --user is-enabled
	// podman.socket` prints for the deploy user ("" when the unit is not
	// found or the user manager is not running), PodmanSocketActive whether
	// it is active.
	PodmanSocketEnabled string `json:"podman_socket_enabled,omitempty"`
	PodmanSocketActive  bool   `json:"podman_socket_active"`

	SSHDTest       bool              `json:"sshd_config_valid"`
	SSHDTestOutput string            `json:"-"`
	SSHD           map[string]string `json:"sshd"`
	SSHDPorts      []int             `json:"sshd_ports,omitempty"`
	SSHDInclude    bool              `json:"sshd_includes_dropins"`
	SSHDDropIn     string            `json:"-"`
	SSHDDropInSet  bool              `json:"sshd_dropin_present"`
	SSHDConflicts  []string          `json:"sshd_conflicts,omitempty"`
	SSHDUnit       string            `json:"sshd_unit,omitempty"`
	// SSHDMatch lists "file:line keyword" for every password,
	// keyboard-interactive, challenge-response or root-login setting inside
	// a Match block; hardening does not cover the connections they match.
	SSHDMatch []string `json:"sshd_match,omitempty"`

	// SSHLocalAddr is the server address of the admin connection
	// (SSH_CONNECTION) and SSHInterface the interface that holds it.
	SSHLocalAddr string `json:"ssh_local_addr,omitempty"`
	SSHInterface string `json:"ssh_interface,omitempty"`

	UFWActive        bool  `json:"ufw_active"`
	UFWPorts         []int `json:"ufw_ports,omitempty"`
	FirewalldPresent bool  `json:"firewalld_present"`
	FirewalldActive  bool  `json:"firewalld_active"`
	// FirewalldZone is the zone of SSHInterface, or the default zone when
	// FirewalldZoneDefault is set.
	FirewalldZone        string `json:"firewalld_zone,omitempty"`
	FirewalldZoneDefault bool   `json:"firewalld_zone_is_default"`
	// FirewalldPorts are the TCP ports open in both the runtime and the
	// permanent configuration of FirewalldZone (only the permanent one
	// while firewalld is stopped).
	FirewalldPorts  []int `json:"firewalld_ports,omitempty"`
	FirewalldSSHSvc bool  `json:"firewalld_ssh_service"`
	// UFWEnabled and FirewalldEnabled report that ufw.service and
	// firewalld.service start at boot.
	UFWEnabled       bool `json:"ufw_enabled"`
	FirewalldEnabled bool `json:"firewalld_enabled"`
	// RulesetServices lists the active nftables.service, iptables.service
	// and ip6tables.service whose ruleset is not empty (or could not be
	// read), as "UNIT (N rules)".
	RulesetServices []string `json:"ruleset_services,omitempty"`

	// Pacman is set when pacman is installed. PacmanSync maps each package
	// depper may install on Arch to its version in the local sync database
	// (absent when the database does not have it); PacmanSyncEmpty reports
	// that there is no sync database at all.
	Pacman          bool              `json:"pacman,omitempty"`
	PacmanSync      map[string]string `json:"pacman_sync,omitempty"`
	PacmanSyncEmpty bool              `json:"pacman_sync_empty,omitempty"`

	// Compose maps each compose provider package to its installed and
	// candidate versions ("" when absent); AptListsEmpty reports that apt
	// has no package lists to look them up in.
	Compose          map[string]PkgVersions `json:"compose_packages,omitempty"`
	AptListsEmpty    bool                   `json:"apt_lists_empty,omitempty"`
	AutoUpdatesConf  bool                   `json:"auto_updates_configured"`
	AutoUpdatesApply bool                   `json:"-"`
	AutoUpdatesTimer string                 `json:"auto_updates_timer,omitempty"`
	TailscaleUp      bool                   `json:"tailscale_up"`

	// SystemdVersion is the major version from `systemctl --version`
	// (0 when unknown).
	SystemdVersion int `json:"systemd_version,omitempty"`
	// SSHDRootBase is the PermitRootLogin sshd would use without depper's
	// drop-in ("" when it could not be determined).
	SSHDRootBase string `json:"sshd_permitrootlogin_without_dropin,omitempty"`
	// SSHSocketPorts are the ports of ssh.socket and sshd.socket when they
	// are active (socket-activated sshd listens there, whatever sshd -T
	// says).
	SSHSocketPorts []int `json:"ssh_socket_ports,omitempty"`
	// Guards is the state of each automatic revert (depper-ssh-revert,
	// depper-firewall-revert) left on the host.
	Guards map[string]GuardState `json:"guards,omitempty"`

	Record *Record `json:"record,omitempty"`
}

// GuardState is what the host says about one automatic revert.
type GuardState struct {
	// Timer is the SubState of UNIT.timer ("waiting" while armed).
	Timer string `json:"timer"`
	// Service is the ActiveState of UNIT.service.
	Service string `json:"service"`
	// Markers is true when revert markers of an unverified change exist.
	Markers bool `json:"markers"`
}

// Pending reports that a guarded change of an earlier run was neither
// verified nor reverted.
func (g GuardState) Pending() bool {
	return g.Timer == "waiting" || g.Service == "activating" || g.Markers
}

// pkgReq is one package requirement: satisfied when the package is installed
// or, when Cmd is set, when the command exists (curl-minimal and
// coreutils-single provide curl and coreutils on Fedora/RHEL).
type pkgReq struct {
	Pkg string
	Cmd string
	// Installed marks a requirement already known to be satisfied.
	Installed bool
}

func (f *Facts) has(r pkgReq) bool {
	if r.Installed {
		return true
	}
	if r.Cmd != "" && f.Commands[r.Cmd] != "" {
		return true
	}
	return f.Packages[r.Pkg]
}

// PkgVersions are the installed and candidate (available) versions of a
// package; each is "" when there is none.
type PkgVersions struct {
	Installed string `json:"installed,omitempty"`
	Candidate string `json:"candidate,omitempty"`
}

// composePackages are the compose provider packages host init looks up when
// the configuration uses [container].compose.
var composePackages = []string{"podman-compose", "docker-compose-plugin", "docker-compose-v2", "docker-compose"}

// queriedPackages and queriedCommands are probed on every host.
var queriedPackages = []string{
	"rsync", "curl", "ca-certificates", "coreutils",
	"podman", "uidmap", "slirp4netns", "passt", "shadow-utils",
	"docker.io", "moby-engine", "docker", "cloudflared", "tailscale",
	"ufw", "firewalld", "unattended-upgrades", "dnf-automatic",
}

var queriedCommands = []string{
	"rsync", "curl", "sha256sum", "podman", "docker", "cloudflared", "tailscale",
	"ufw", "firewall-cmd", "firewall-offline-cmd", "sshd", "runuser", "newuidmap", "systemd-run",
}

// archPackages are the packages depper may install on Arch Linux; the facts
// script asks pacman which are installed (pacman -Q) and which the local
// sync database has (pacman -Si), without refreshing it.
var archPackages = []string{
	"rsync", "curl", "ca-certificates", "coreutils", "shadow",
	"podman", "passt", "slirp4netns", "podman-compose", "docker", "docker-compose",
	"cloudflared", "tailscale", "ufw",
}

// classify maps os-release ID / ID_LIKE to a supported family.
func classify(id, like string) string {
	switch id {
	case "debian", "ubuntu":
		return FamilyDebian
	case "fedora", "rhel", "centos", "rocky", "almalinux":
		return FamilyFedora
	case "arch":
		return FamilyArch
	}
	for _, l := range strings.Fields(like) {
		switch l {
		case "debian", "ubuntu":
			return FamilyDebian
		case "fedora", "rhel", "centos":
			return FamilyFedora
		case "arch":
			return FamilyArch
		}
	}
	return ""
}

// pacmanListAwk turns `pacman -Q` output ("NAME VERSION" lines) into
// "NAME=VERSION " pairs.
const pacmanListAwk = `NF >= 2 { printf "%s=%s ", $1, $2 }`

// pacmanInfoAwk turns `pacman -Si` output into "NAME=VERSION " pairs, the
// first repository's entry for each name.
const pacmanInfoAwk = `/^Name[[:space:]]*:/ { sub(/^Name[[:space:]]*:[[:space:]]*/, ""); n = $1 } /^Version[[:space:]]*:/ { sub(/^Version[[:space:]]*:[[:space:]]*/, ""); if (n != "" && !(n in seen)) { seen[n] = 1; printf "%s=%s ", n, $1 } n = "" }`

// pacmanPairs parses "NAME=VERSION ..." into a map, keeping only the names
// in archPackages.
func pacmanPairs(s string) map[string]string {
	out := map[string]string{}
	for _, w := range strings.Fields(s) {
		name, version, ok := strings.Cut(w, "=")
		if ok && contains(archPackages, name) {
			out[name] = clean(version)
		}
	}
	return out
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
	return r.asRootArgs(body)
}

// asRootArgs is asRoot with positional parameters for the root script. Each
// arg is a shell word of the admin script (for example `"$depper_iface"`),
// so values computed by the unprivileged part reach the root part as $1...
// even through sudo, which resets the environment.
func (r *Runner) asRootArgs(body []string, args ...string) ([]string, error) {
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
	call := "depper_asroot"
	for _, a := range args {
		call += " " + a
	}
	lines := []string{
		`depper_asroot() { if test "$(id -u)" = 0; then sh -s -- "$@"; else sudo -n sh -s -- "$@"; fi; }`,
		call + " <<'" + rootHeredoc + "'",
	}
	lines = append(lines, strings.Split(strings.TrimSuffix(script, "\n"), "\n")...)
	return append(lines, rootHeredoc), nil
}

// authorizedKeyAwk classifies an authorized_keys file for the key "TYPE
// BLOB" in k: it prints "plain" when an uncommented line holds the key
// without options, "restricted" when the only matches carry options
// (command=, from=, restrict, no-pty, ...), and "none" otherwise.
const authorizedKeyAwk = `/^[[:space:]]*#/ { next } { for (i = 1; i < NF; i++) if ($i " " $(i+1) == k) { if (i == 1) p = 1; else r = 1 } } END { if (p) print "plain"; else if (r) print "restricted"; else print "none" }`

// matchAwk prints "FILE:LINE keyword" for every hardening-relevant setting
// inside a Match block (a block runs to the next Match or the end of the
// file).
const matchAwk = `FNR == 1 { m = 0 } { k = tolower($1) } k == "match" { m = 1; next } m && (k == "passwordauthentication" || k == "kbdinteractiveauthentication" || k == "challengeresponseauthentication" || k == "permitrootlogin") { printf "%s:%d %s;", FILENAME, FNR, $1 }`

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
		// The interface the admin connection arrives on (sudo drops
		// SSH_CONNECTION, so it is read here and passed to the root part).
		`depper_iface=""`,
		`if test -n "${SSH_CONNECTION:-}"; then`,
		`  set -- $SSH_CONNECTION`,
		`  depper_laddr=${3:-}`,
		`  case "$depper_laddr" in *[!0-9A-Fa-f.:]*) depper_laddr="" ;; esac`,
		`  printf 'ssh_laddr=%s\n' "$depper_laddr"`,
		`  if test -n "$depper_laddr"; then depper_iface=$(PATH="$PATH:/usr/sbin:/sbin" ip -o addr show 2>/dev/null | awk -v a="$depper_laddr" '{ split($4, x, "/"); if (x[1] == a) { sub(/@.*/, "", $2); print $2; exit } }'); fi`,
		`  case "$depper_iface" in *[!A-Za-z0-9_.:-]*) depper_iface="" ;; esac`,
		`  printf 'ssh_iface=%s\n' "$depper_iface"`,
		`fi`,
	}
	akMatch := shell.Quote(key.Type + " " + key.Blob)
	root := []string{
		"set -u",
		`depper_iface=${1:-}`,
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
		// Only the class of the password field is printed, never the field.
		`  case "$(getent shadow "$u" 2>/dev/null | cut -d: -f2)" in '!'|'!!') echo user_pw=locked ;; '*') echo user_pw=star ;; '') echo user_pw=empty ;; *) echo user_pw=set ;; esac`,
		`  ak="$home/.ssh/authorized_keys"`,
		`  if test -f "$ak"; then printf 'ak_key=%s\n' "$(awk -v k=` + akMatch + ` ` + shell.Quote(authorizedKeyAwk) + ` "$ak")"; else echo ak_key=none; fi`,
		`  if test -e ` + r.p("/var/lib/systemd/linger") + `/"$u"; then echo linger=1; else echo linger=0; fi`,
		`  rt=` + r.p("/run/user") + `/"$uid"`,
		`  if runuser -u "$u" -- env XDG_RUNTIME_DIR="$rt" DBUS_SESSION_BUS_ADDRESS="unix:path=$rt/bus" systemctl --user show-environment >/dev/null 2>&1; then echo usermgr=1; else echo usermgr=0; fi`,
		// The groups of the running user manager, whose services inherit them.
		`  depper_mpid=$(systemctl show -p MainPID --value "user@$uid.service" 2>/dev/null || true)`,
		`  case "$depper_mpid" in ""|*[!0-9]*) depper_mpid=0 ;; esac`,
		`  printf 'usermgr_pid=%s\n' "$depper_mpid"`,
		`  if test "$depper_mpid" != 0 && test -r ` + r.p("/proc") + `/"$depper_mpid/status"; then printf 'usermgr_groups=%s\n' "$(awk '$1 == "Groups:" { $1 = ""; print }' ` + r.p("/proc") + `/"$depper_mpid/status" | tr -cd '0-9 ')"; echo usermgr_groups_read=1; fi`,
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
		// pacman reports separately, so a dpkg or rpm that happens to be
		// installed on Arch (or pacman elsewhere) does not mislead; Go picks
		// by the family. The sync database is only read, never refreshed.
		`if command -v pacman >/dev/null 2>&1; then`,
		`  echo pacman=1`,
		`  printf 'pac_q=%s\n' "$(LC_ALL=C pacman -Q ` + shell.Join(archPackages...) + ` 2>/dev/null | awk ` + shell.Quote(pacmanListAwk) + `)"`,
		`  printf 'pac_si=%s\n' "$(LC_ALL=C pacman -Si ` + shell.Join(archPackages...) + ` 2>/dev/null | awk ` + shell.Quote(pacmanInfoAwk) + `)"`,
		`  if ! ls ` + r.p("/var/lib/pacman/sync") + `/*.db >/dev/null 2>&1; then echo pacman_sync_empty=1; fi`,
		`fi`,
		`if systemctl is-active --quiet docker.service 2>/dev/null; then echo docker_active=1; fi`,
		`if systemctl is-enabled --quiet docker.service 2>/dev/null; then echo docker_enabled=1; fi`,
		`if systemctl is-active --quiet docker.socket 2>/dev/null; then echo docker_socket_active=1; fi`,
		`if systemctl is-enabled --quiet docker.socket 2>/dev/null; then echo docker_socket_enabled=1; fi`,
		`printf 'docker_gid=%s\n' "$(getent group docker 2>/dev/null | cut -d: -f3)"`,
		`if command -v sshd >/dev/null 2>&1; then`,
		`  if t=$(sshd -T 2>&1); then`,
		`    echo sshd_t=1`,
		`    printf '%s\n' "$t" | awk '$1 == "passwordauthentication" || $1 == "kbdinteractiveauthentication" || $1 == "permitrootlogin" || $1 == "pubkeyauthentication" || $1 == "usepam" { print "sshd_" $1 "=" $2 }'`,
		// Port lines, and the port of every ListenAddress (host:port,
		// [v6]:port), which sshd -T always prints with a port.
		`    printf 'sshd_ports=%s\n' "$(printf '%s\n' "$t" | awk '$1 == "port" { printf "%s ", $2 } $1 == "listenaddress" { n = split($2, a, ":"); if (n > 1) printf "%s ", a[n] }')"`,
		`  else echo sshd_t=0; printf 'sshd_t_msg=%s\n' "$(printf '%s' "$t" | tr '\n' ' ' | head -c 300)"; fi`,
		// PermitRootLogin as sshd would read it without depper's drop-in:
		// sshd -T on a copy of the configuration that leaves it out.
		`  if test -f ` + r.p(SSHDropIn) + `; then`,
		`    depper_bt=$(mktemp -d) && mkdir "$depper_bt/d" && {`,
		`      for f in ` + r.p(sshdDropInD) + `/*.conf; do test -f "$f" || continue; test "${f##*/}" = ` + sshDropInName + ` && continue; cp -p "$f" "$depper_bt/d/"; done`,
		`      awk -v d="$depper_bt/d/*.conf" 'tolower($1) == "include" { for (i = 2; i <= NF; i++) if ($i ~ /^(\/etc\/ssh\/)?sshd_config\.d\/\*\.conf$/) $i = d } { print }' ` + r.p(sshdMain) + ` > "$depper_bt/sshd_config"`,
		`      printf 'sshd_root_base=%s\n' "$(sshd -T -f "$depper_bt/sshd_config" 2>/dev/null | awk '$1 == "permitrootlogin" { print $2 }')"`,
		`    }`,
		`    rm -rf "$depper_bt"`,
		`  fi`,
		`fi`,
		`depper_listen=""`,
		`for s in ssh.socket sshd.socket; do if systemctl is-active --quiet "$s" 2>/dev/null; then depper_listen="$depper_listen $(systemctl show -p Listen --value "$s" 2>/dev/null | tr '\n' ' ')"; fi; done`,
		`if test -n "$depper_listen"; then printf 'ssh_socket_listen=%s\n' "$depper_listen"; fi`,
		`printf 'systemd_version=%s\n' "$(systemctl --version 2>/dev/null | awk 'NR == 1 { print $2 }')"`,
		`for g in ` + shell.Join(guardUnits...) + `; do`,
		`  mk=0; for m in ` + shell.Join(guardMarkers...) + `; do test -e ` + r.p(guardStateDir) + `/"$g/$m" && mk=1; done`,
		`  printf 'guard_%s=%s|%s|%s\n' "$g" "$(systemctl show -p SubState --value "$g.timer" 2>/dev/null)" "$(systemctl show -p ActiveState --value "$g.service" 2>/dev/null)" "$mk"`,
		`done`,
		`if grep -Eiq '^[[:space:]]*Include[[:space:]]+(/etc/ssh/)?sshd_config\.d/\*\.conf' ` + r.p(sshdMain) + ` 2>/dev/null; then echo sshd_include=1; fi`,
		`if test -f ` + r.p(SSHDropIn) + `; then printf 'sshd_dropin=%s\n' "$(base64 < ` + r.p(SSHDropIn) + ` | tr -d '\n')"; fi`,
		// sshd keeps the first value it reads: the main file up to its
		// Include, then the drop-ins in lexical order. Only those read
		// before depper's drop-in can override it.
		`conflicts=""`,
		`yes_re='^[[:space:]]*(PasswordAuthentication|KbdInteractiveAuthentication|ChallengeResponseAuthentication|PermitRootLogin)[[:space:]]+"?yes"?([[:space:]]|$)'`,
		`if test -f ` + r.p(sshdMain) + ` && awk 'tolower($1) == "include" || tolower($1) == "match" { exit } { print }' ` + r.p(sshdMain) + ` | grep -Eiq "$yes_re"; then conflicts="sshd_config"; fi`,
		`for f in ` + r.p(sshdDropInD) + `/*.conf; do`,
		`  test -f "$f" || continue`,
		`  b=${f##*/}`,
		`  test "$b" = ` + sshDropInName + ` && continue`,
		`  first=$(printf '%s\n%s\n' "$b" ` + sshDropInName + ` | LC_ALL=C sort | head -n 1)`,
		`  test "$first" = "$b" || continue`,
		`  if awk 'tolower($1) == "match" { exit } { print }' "$f" | grep -Eiq "$yes_re"; then conflicts="$conflicts sshd_config.d/$b"; fi`,
		`done`,
		`printf 'sshd_conflicts=%s\n' "$conflicts"`,
		`depper_match=""`,
		`for f in ` + r.p(sshdMain) + ` ` + r.p(sshdDropInD) + `/*.conf; do`,
		`  test -f "$f" || continue`,
		`  depper_match="$depper_match$(awk ` + shell.Quote(matchAwk) + ` "$f" 2>/dev/null)"`,
		`done`,
		`printf 'sshd_match=%s\n' "$depper_match"`,
		`if systemctl is-active --quiet ssh.service 2>/dev/null; then echo sshd_unit=ssh; elif systemctl is-active --quiet sshd.service 2>/dev/null; then echo sshd_unit=sshd; fi`,
		`if command -v ufw >/dev/null 2>&1; then`,
		`  s=$(ufw status 2>/dev/null || true)`,
		`  case "$s" in *"Status: active"*) echo ufw_active=1 ;; esac`,
		`  printf 'ufw_ports=%s\n' "$(printf '%s\n' "$s" | awk '$2 == "ALLOW" && $1 ~ /^[0-9]+(\/tcp)?$/ { sub("/tcp", "", $1); printf "%s ", $1 }')"`,
		`fi`,
		`if systemctl is-enabled --quiet ufw.service 2>/dev/null; then echo ufw_enabled=1; fi`,
		`if systemctl is-enabled --quiet firewalld.service 2>/dev/null; then echo firewalld_enabled=1; fi`,
		// A hand-written ruleset loaded by nftables.service or
		// iptables.service: the number of rules (policy ACCEPT lines do not
		// count), or "unknown" when it cannot be read.
		`if systemctl is-active --quiet nftables.service 2>/dev/null; then`,
		`  if depper_rs=$(nft list ruleset 2>/dev/null); then printf 'ruleset_nftables=%s\n' "$(printf '%s\n' "$depper_rs" | grep -c '[^[:space:]]' || true)"; else echo ruleset_nftables=unknown; fi`,
		`fi`,
		`for t in iptables ip6tables; do`,
		`  if systemctl is-active --quiet "$t.service" 2>/dev/null; then`,
		`    if depper_rs=$("$t" -S 2>/dev/null); then printf 'ruleset_%s=%s\n' "$t" "$(printf '%s\n' "$depper_rs" | grep -Evc '^-P [A-Z]+ ACCEPT$|^[[:space:]]*$' || true)"; else printf 'ruleset_%s=unknown\n' "$t"; fi`,
		`  fi`,
		`done`,
		`if command -v firewall-cmd >/dev/null 2>&1 || command -v firewall-offline-cmd >/dev/null 2>&1; then`,
		`  echo firewalld_present=1`,
		`  if systemctl is-active --quiet firewalld.service 2>/dev/null; then echo firewalld_active=1; fc=firewall-cmd; else fc=firewall-offline-cmd; fi`,
		`  zone=""`,
		`  if test -n "$depper_iface"; then zone=$($fc --get-zone-of-interface="$depper_iface" 2>/dev/null || true); fi`,
		`  if test -z "$zone"; then zone=$($fc --get-default-zone 2>/dev/null || true); echo firewalld_zone_default=1; fi`,
		`  case "$zone" in ""|*[!A-Za-z0-9_-]*) printf 'firewalld_zone_bad=%s\n' "$(printf '%s' "$zone" | tr -c 'A-Za-z0-9_-' '?' | head -c 64)" ;; *)`,
		`    printf 'firewalld_zone=%s\n' "$zone"`,
		`    if test "$fc" = firewall-cmd; then`,
		`      printf 'firewalld_ports=%s\n' "$(firewall-cmd --zone="$zone" --list-ports 2>/dev/null | tr '\n' ' ')"`,
		`      printf 'firewalld_pports=%s\n' "$(firewall-cmd --permanent --zone="$zone" --list-ports 2>/dev/null | tr '\n' ' ')"`,
		`      printf 'firewalld_services=%s\n' "$(firewall-cmd --zone="$zone" --list-services 2>/dev/null | tr '\n' ' ')"`,
		`      printf 'firewalld_pservices=%s\n' "$(firewall-cmd --permanent --zone="$zone" --list-services 2>/dev/null | tr '\n' ' ')"`,
		`    else`,
		`      printf 'firewalld_pports=%s\n' "$(firewall-offline-cmd --zone="$zone" --list-ports 2>/dev/null | tr '\n' ' ')"`,
		`      printf 'firewalld_pservices=%s\n' "$(firewall-offline-cmd --zone="$zone" --list-services 2>/dev/null | tr '\n' ' ')"`,
		`    fi ;;`,
		`  esac`,
		`fi`,
		`f=` + r.p("/etc/apt/apt.conf.d/20auto-upgrades"),
		`if test -f "$f" && grep -q 'APT::Periodic::Unattended-Upgrade "1"' "$f"; then echo autoupd_apt=1; fi`,
		`f=` + r.p("/etc/dnf/automatic.conf"),
		`if test -f "$f" && grep -Eq '^[[:space:]]*upgrade_type[[:space:]]*=[[:space:]]*security' "$f"; then echo autoupd_security=1; fi`,
		`if test -f "$f" && grep -Eq '^[[:space:]]*apply_updates[[:space:]]*=[[:space:]]*(yes|true|1)' "$f"; then echo autoupd_apply=1; fi`,
		`for t in dnf-automatic-install.timer dnf5-automatic.timer; do if systemctl is-enabled --quiet "$t" 2>/dev/null; then printf 'autoupd_timer=%s\n' "$t"; break; fi; done`,
		`if command -v tailscale >/dev/null 2>&1 && tailscale status >/dev/null 2>&1; then echo tailscale_up=1; fi`,
		`if test -f ` + r.p(RecordPath) + `; then printf 'record=%s\n' "$(base64 < ` + r.p(RecordPath) + ` | tr -d '\n')"; fi`,
	}
	if o.Compose {
		// Installed and candidate versions of each compose provider.
		root = append(root,
			`for p in `+shell.Join(composePackages...)+`; do`,
			`  if command -v apt-cache >/dev/null 2>&1; then`,
			`    v=$(apt-cache policy "$p" 2>/dev/null | awk '$1 == "Installed:" { i = $2 } $1 == "Candidate:" { c = $2 } END { print i "|" c }')`,
			`  elif command -v dnf >/dev/null 2>&1; then`,
			`    i=$(rpm -q --qf '%{VERSION}\n' "$p" 2>/dev/null | head -n 1) || i=""`,
			`    c=$(dnf -q info --available "$p" 2>/dev/null | awk -F': *' '$1 ~ /^Version/ { print $2; exit }')`,
			`    v="$i|$c"`,
			`  else v="|"; fi`,
			`  printf 'compose_%s=%s\n' "$p" "$v"`,
			`done`,
			`if command -v apt-cache >/dev/null 2>&1 && ! ls `+r.p("/var/lib/apt/lists")+`/*_Packages* >/dev/null 2>&1; then echo apt_lists_empty=1; fi`,
			// The docker-compose CLI plugin (or binary) podman can run as its
			// provider, and the deploy user's podman.socket it then needs.
			`depper_plugin=""`,
			`for f in `+r.p("/usr/libexec/docker/cli-plugins/docker-compose")+` `+r.p("/usr/lib/docker/cli-plugins/docker-compose")+` `+r.p("/usr/local/libexec/docker/cli-plugins/docker-compose")+` `+r.p("/usr/local/lib/docker/cli-plugins/docker-compose")+`; do if test -x "$f"; then depper_plugin=$f; break; fi; done`,
			`if test -z "$depper_plugin"; then depper_plugin=$(command -v docker-compose 2>/dev/null || true); fi`,
			`printf 'compose_plugin=%s\n' "$depper_plugin"`,
			`if test -n "$pw" && runuser -u "$u" -- env XDG_RUNTIME_DIR="$rt" DBUS_SESSION_BUS_ADDRESS="unix:path=$rt/bus" systemctl --user show-environment >/dev/null 2>&1; then`,
			`  printf 'podman_socket_enabled=%s\n' "$(runuser -u "$u" -- env XDG_RUNTIME_DIR="$rt" DBUS_SESSION_BUS_ADDRESS="unix:path=$rt/bus" systemctl --user is-enabled podman.socket 2>/dev/null | head -n 1 | tr -cd 'a-z-')"`,
			`  if runuser -u "$u" -- env XDG_RUNTIME_DIR="$rt" DBUS_SESSION_BUS_ADDRESS="unix:path=$rt/bus" systemctl --user is-active --quiet podman.socket 2>/dev/null; then echo podman_socket_active=1; fi`,
			`fi`,
		)
	}
	root = append(root, `echo root_facts=1`)
	rootLines, err := r.asRootArgs(root, `"$depper_iface"`)
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
	"sshd_dropin": true, "sshd_conflicts": true, "sshd_unit": true, "sshd_match": true, "sshd_root_base": true,
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
		AdminUser:            clean(m["admin_user"]),
		Privilege:            m["priv"],
		SudoMessage:          clean(m["sudo_msg"]),
		OSID:                 osValue(m["os_ID"]),
		OSLike:               osValue(m["os_ID_LIKE"]),
		VersionID:            osValue(m["os_VERSION_ID"]),
		Codename:             osValue(m["os_VERSION_CODENAME"]),
		UbuntuCodename:       osValue(m["os_UBUNTU_CODENAME"]),
		PrettyName:           osValue(m["os_PRETTY_NAME"]),
		Systemd:              m["systemd"] == "1",
		Arch:                 clean(m["arch"]),
		UserExists:           m["user_exists"] == "1",
		UserHome:             clean(m["user_home"]),
		UserShell:            clean(m["user_shell"]),
		UserGroups:           strings.Fields(clean(m["user_groups"])),
		UserPassword:         clean(m["user_pw"]),
		KeyPresent:           m["ak_key"] == "plain",
		KeyRestricted:        m["ak_key"] == "restricted",
		SSHLocalAddr:         clean(m["ssh_laddr"]),
		SSHInterface:         clean(m["ssh_iface"]),
		Linger:               m["linger"] == "1",
		UserManager:          m["usermgr"] == "1",
		SubUID:               m["subuid"] == "1",
		SubGID:               m["subgid"] == "1",
		Packages:             map[string]bool{},
		Commands:             map[string]string{},
		DockerActive:         m["docker_active"] == "1",
		DockerEnabled:        m["docker_enabled"] == "1",
		DockerSocketActive:   m["docker_socket_active"] == "1",
		DockerSocketEnabled:  m["docker_socket_enabled"] == "1",
		ManagerGroups:        strings.Fields(m["usermgr_groups"]),
		ManagerGroupsKnown:   m["usermgr_groups_read"] == "1",
		PodmanSocketEnabled:  clean(m["podman_socket_enabled"]),
		PodmanSocketActive:   m["podman_socket_active"] == "1",
		SSHDTest:             m["sshd_t"] == "1",
		SSHDTestOutput:       clean(m["sshd_t_msg"]),
		SSHD:                 map[string]string{},
		SSHDInclude:          m["sshd_include"] == "1",
		SSHDConflicts:        strings.Fields(clean(m["sshd_conflicts"])),
		SSHDUnit:             m["sshd_unit"],
		UFWActive:            m["ufw_active"] == "1",
		UFWPorts:             ports(m["ufw_ports"]),
		FirewalldPresent:     m["firewalld_present"] == "1",
		FirewalldActive:      m["firewalld_active"] == "1",
		FirewalldZone:        clean(m["firewalld_zone"]),
		FirewalldZoneDefault: m["firewalld_zone_default"] == "1",
		AutoUpdatesConf:      m["autoupd_apt"] == "1" || m["autoupd_security"] == "1",
		AutoUpdatesApply:     m["autoupd_apply"] == "1",
		AutoUpdatesTimer:     clean(m["autoupd_timer"]),
		TailscaleUp:          m["tailscale_up"] == "1",
	}
	f.Family = classify(f.OSID, f.OSLike)
	if uid, err := strconv.Atoi(m["user_uid"]); err == nil {
		f.UserUID = uid
	}
	if pid, err := strconv.Atoi(m["usermgr_pid"]); err == nil && pid > 0 {
		f.ManagerPID = pid
	}
	if gid := clean(m["docker_gid"]); gidRe.MatchString(gid) {
		f.DockerGID = gid
	}
	if p := clean(m["compose_plugin"]); p != "" {
		f.ComposePlugin = p
	}
	// A firewalld port or the ssh service counts when it is open both now
	// and after a reload (runtime and permanent); a stopped firewalld only
	// has the permanent configuration.
	permPorts, permSvcs := ports(m["firewalld_pports"]), strings.Fields(m["firewalld_pservices"])
	if f.FirewalldActive {
		for _, p := range ports(m["firewalld_ports"]) {
			if containsInt(permPorts, p) {
				f.FirewalldPorts = append(f.FirewalldPorts, p)
			}
		}
		f.FirewalldSSHSvc = contains(strings.Fields(m["firewalld_services"]), "ssh") && contains(permSvcs, "ssh")
	} else {
		f.FirewalldPorts, f.FirewalldSSHSvc = permPorts, contains(permSvcs, "ssh")
	}
	for _, mm := range strings.Split(m["sshd_match"], ";") {
		if mm = clean(mm); mm != "" {
			f.SSHDMatch = append(f.SSHDMatch, mm)
		}
	}
	f.AptListsEmpty = m["apt_lists_empty"] == "1"
	f.UFWEnabled = m["ufw_enabled"] == "1"
	f.FirewalldEnabled = m["firewalld_enabled"] == "1"
	for _, unit := range []string{"nftables", "iptables", "ip6tables"} {
		n, ok := m["ruleset_"+unit]
		if !ok {
			continue
		}
		switch n = clean(n); n {
		case "0":
		case "unknown", "":
			f.RulesetServices = append(f.RulesetServices, unit+".service (ruleset unreadable)")
		default:
			f.RulesetServices = append(f.RulesetServices, unit+".service ("+n+" rules)")
		}
	}
	for _, p := range composePackages {
		if v, ok := m["compose_"+p]; ok {
			if f.Compose == nil {
				f.Compose = map[string]PkgVersions{}
			}
			i, c, _ := strings.Cut(v, "|")
			f.Compose[p] = PkgVersions{Installed: pkgVersion(i), Candidate: pkgVersion(c)}
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
	// On Arch, pacman alone says what is installed and available.
	f.Pacman, f.PacmanSyncEmpty = m["pacman"] == "1", m["pacman_sync_empty"] == "1"
	if f.Family == FamilyArch {
		installed := pacmanPairs(m["pac_q"])
		f.PacmanSync = pacmanPairs(m["pac_si"])
		f.Packages = map[string]bool{}
		for _, p := range archPackages {
			_, ok := installed[p]
			f.Packages[p] = ok
		}
		if f.Compose != nil {
			for _, p := range composePackages {
				f.Compose[p] = PkgVersions{Installed: pkgVersion(installed[p]), Candidate: pkgVersion(f.PacmanSync[p])}
			}
		}
	}
	f.SSHDPorts = ports(m["sshd_ports"])
	f.SSHSocketPorts = listenPorts(m["ssh_socket_listen"])
	f.SystemdVersion, _ = strconv.Atoi(strings.TrimSpace(m["systemd_version"]))
	f.SSHDRootBase = clean(m["sshd_root_base"])
	for _, g := range guardUnits {
		v, ok := m["guard_"+g]
		if !ok {
			continue
		}
		parts := strings.SplitN(v, "|", 3)
		for len(parts) < 3 {
			parts = append(parts, "")
		}
		if f.Guards == nil {
			f.Guards = map[string]GuardState{}
		}
		f.Guards[g] = GuardState{Timer: clean(parts[0]), Service: clean(parts[1]), Markers: parts[2] == "1"}
	}
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

// listenPorts parses the Listen values of a socket unit ("[::]:22 (Stream)
// 0.0.0.0:2222 (Stream) 22 (Stream)") into sorted TCP ports; datagram and
// other non-stream listeners are skipped.
func listenPorts(s string) []int {
	f := strings.Fields(s)
	var words []string
	for i := 0; i < len(f); i++ {
		if strings.HasPrefix(f[i], "(") {
			continue
		}
		if i+1 < len(f) && strings.HasPrefix(f[i+1], "(") && f[i+1] != "(Stream)" {
			continue
		}
		w := f[i]
		if j := strings.LastIndex(w, ":"); j >= 0 {
			w = w[j+1:]
		}
		words = append(words, w)
	}
	return ports(strings.Join(words, " "))
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
	if r.fsRoot != "" {
		for i, mm := range f.SSHDMatch {
			f.SSHDMatch[i] = strings.TrimPrefix(mm, r.fsRoot)
		}
		f.ComposePlugin = strings.TrimPrefix(f.ComposePlugin, r.fsRoot)
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
		return nil, fmt.Errorf("%s needs a password for sudo%s; depper connects with BatchMode and cannot type one. Either connect as root with an SSH key (root@%s; root key login is allowed by sshd's default PermitRootLogin prohibit-password), or temporarily allow passwordless sudo for %s (for example the line `%s ALL=(ALL) NOPASSWD: ALL` in a file made with `sudo visudo -f /etc/sudoers.d/depper-init`, removed again after host init)", o.Admin, msg, o.Host, o.Admin, o.Admin)
	default:
		return nil, fmt.Errorf("%s is not root and sudo is not installed; connect as root with an SSH key, or as a user with passwordless sudo", o.Admin)
	}
	return f, nil
}

// pkgVersion cleans a version reported by apt-cache or rpm/dnf; "(none)"
// and anything that is not a version become "".
func pkgVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "(none)" || !pkgVersionRe.MatchString(v) {
		return ""
	}
	return v
}

var pkgVersionRe = regexp.MustCompile(`^[0-9][A-Za-z0-9.+~:_-]{0,80}$`)

// gidRe matches a numeric group id.
var gidRe = regexp.MustCompile(`^[0-9]{1,10}$`)

// versionMajor returns the major version of a Debian or RPM version string
// (an epoch "N:" is skipped), or -1.
func versionMajor(v string) int {
	if _, rest, ok := strings.Cut(v, ":"); ok {
		v = rest
	}
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(v[:end])
	if err != nil {
		return -1
	}
	return n
}
