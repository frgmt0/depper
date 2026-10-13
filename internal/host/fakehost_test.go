package host

// Test harness: a fake fresh VPS in a temporary directory.
//
// The test binary doubles as the fake ssh and ssh-keyscan programs
// (re-executed through wrapper scripts with DEPPER_FAKE set). Fake ssh
// enforces the pinned host key (UserKnownHostsFile + StrictHostKeyChecking=yes),
// authenticates the admin and, for the deploy user, the authorized key, then
// runs the script it receives with dash -s and a PATH of only fake tools
// (apt-get, dnf, useradd, loginctl, systemctl, sshd, ufw, ...) plus an
// allowlist of basic utilities. The fakes record their calls and mutate a
// fake root filesystem under FAKE_ROOT/fs, which the Runner's fsRoot prefixes
// to every absolute remote path. Nothing contacts a real host.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/transport"
)

func TestMain(m *testing.M) {
	switch os.Getenv("DEPPER_FAKE") {
	case "ssh":
		os.Exit(fakeSSHMain(os.Args[1:]))
	case "keyscan":
		os.Exit(fakeKeyscanMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func appendFile(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

func readTrim(path string) string {
	data, _ := os.ReadFile(path)
	return strings.TrimSpace(string(data))
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func fakeKeyscanMain(args []string) int {
	root := os.Getenv("FAKE_ROOT")
	appendFile(filepath.Join(root, "keyscan.log"), strings.Join(args, " "))
	port, addr := 22, ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-p":
			port, _ = strconv.Atoi(args[i+1])
			i++
		case "-T":
			i++
		default:
			addr = args[i]
		}
	}
	if exists(filepath.Join(root, "keyscan-unreachable")) {
		return 1
	}
	fmt.Fprintf(os.Stderr, "# %s:%d SSH-2.0-OpenSSH_9.6\n", addr, port)
	name := KnownHostsName(addr, port)
	for _, line := range strings.Split(readTrim(filepath.Join(root, "hostkeys")), "\n") {
		if line != "" {
			fmt.Printf("%s %s\n", name, line)
		}
	}
	return 0
}

func fakeSSHMain(args []string) int {
	root := os.Getenv("FAKE_ROOT")
	fs := filepath.Join(root, "fs")
	appendFile(filepath.Join(root, "ssh.log"), strings.Join(args, " "))
	opts := map[string]string{}
	port, user, host := 22, "", ""
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-p":
			port, _ = strconv.Atoi(args[i+1])
			i += 2
		case "-l":
			user = args[i+1]
			i += 2
		case "-i":
			i += 2
		case "-o":
			k, v, _ := strings.Cut(args[i+1], "=")
			opts[k] = v
			i += 2
		case "--":
			host = args[i+1]
			i += 2
			goto rest
		default:
			i++
		}
	}
rest:
	if host == "" {
		fmt.Fprintln(os.Stderr, "fake ssh: missing host")
		return 255
	}
	if strings.Join(args[i:], " ") != transport.RemoteShell {
		fmt.Fprintf(os.Stderr, "fake ssh: unexpected remote command %q\n", args[i:])
		return 255
	}
	if opts["BatchMode"] != "yes" {
		fmt.Fprintln(os.Stderr, "fake ssh: BatchMode=yes is required")
		return 255
	}
	addr := host
	if j := strings.LastIndexByte(host, '@'); j >= 0 {
		user, addr = host[:j], host[j+1:]
	}
	// Host key: only the pinned file is trusted.
	if opts["StrictHostKeyChecking"] != "yes" || opts["UserKnownHostsFile"] == "" {
		fmt.Fprintln(os.Stderr, "fake ssh: refusing a connection without a pinned known_hosts file")
		return 255
	}
	content, _ := os.ReadFile(opts["UserKnownHostsFile"])
	keys, _ := knownKeysFor(string(content), KnownHostsName(addr, port))
	trusted := false
	for _, line := range strings.Split(readTrim(filepath.Join(root, "hostkeys")), "\n") {
		f := strings.Fields(line)
		for _, k := range keys {
			if len(f) == 2 && k.Type == f[0] && k.Blob == f[1] {
				trusted = true
			}
		}
	}
	if !trusted {
		fmt.Fprintln(os.Stderr, "Host key verification failed.")
		return 255
	}
	env := []string{
		"PATH=" + fakeRemotePATH(root),
		"FAKE_ROOT=" + root,
		"TMPDIR=" + filepath.Join(root, "tmp"),
	}
	if user == readTrim(filepath.Join(root, "admin-user")) {
		if exists(filepath.Join(root, "deny-admin-after-reload")) && exists(filepath.Join(root, "sshd-reloads")) {
			fmt.Fprintln(os.Stderr, "Permission denied (publickey).")
			return 255
		}
		env = append(env, "HOME="+filepath.Join(fs, "root"), "FAKE_USER="+user, "FAKE_UID="+readTrim(filepath.Join(root, "admin-uid")))
	} else {
		var home, uid string
		passwd, _ := os.ReadFile(filepath.Join(fs, "etc", "passwd"))
		for _, line := range strings.Split(string(passwd), "\n") {
			f := strings.Split(line, ":")
			if len(f) >= 7 && f[0] == user {
				uid, home = f[2], f[5]
			}
		}
		denied := home == ""
		if !denied {
			ak, _ := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
			denied = !strings.Contains(string(ak), readTrim(filepath.Join(root, "client-key-blob")))
		}
		if exists(filepath.Join(root, "deny-deploy-login")) {
			denied = true
		}
		if exists(filepath.Join(root, "deny-deploy-after-reload")) && exists(filepath.Join(root, "sshd-reloads")) && exists(filepath.Join(fs, "etc/ssh/sshd_config.d/50-depper.conf")) {
			denied = true
		}
		if denied {
			fmt.Fprintln(os.Stderr, user+"@"+addr+": Permission denied (publickey).")
			return 255
		}
		env = append(env, "HOME="+home, "FAKE_USER="+user, "FAKE_UID="+uid, "XDG_RUNTIME_DIR="+filepath.Join(fs, "run", "user", uid))
	}
	shellProgram := "/bin/sh"
	if p, err := exec.LookPath("dash"); err == nil {
		shellProgram = p
	}
	cmd := exec.Command(shellProgram, "-s")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Dir = filepath.Join(root, "tmp")
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "fake ssh:", err)
		return 255
	}
	return 0
}

func fakeRemotePATH(root string) string {
	sep := string(os.PathListSeparator)
	return filepath.Join(root, "remote-bin") + sep + filepath.Join(root, "pkg-bin") + sep + filepath.Join(root, "sysbin")
}

// remoteToolAllowlist is every real utility the scripts may use.
var remoteToolAllowlist = []string{
	"sh", "dash", "[", "test", "true", "false", "echo", "printf", "pwd", "expr", "env",
	"cat", "mkdir", "rmdir", "mv", "ln", "rm", "cp", "chmod", "readlink", "ls",
	"date", "stat", "touch", "find", "tee", "sleep",
	"grep", "sed", "awk", "head", "tail", "tr", "cut", "sort", "uniq", "wc",
	"basename", "dirname", "base64", "mktemp", "od", "uname", "hostname", "kill", "perl", "df",
}

// guardedTools must only ever resolve to a fake.
var guardedTools = []string{
	"apt-get", "dpkg-query", "dnf", "rpm", "useradd", "usermod", "loginctl", "systemctl", "runuser",
	"sudo", "sshd", "ufw", "firewall-cmd", "firewall-offline-cmd", "tailscale", "curl", "podman", "docker",
	"cloudflared", "rsync", "getent", "id", "chown",
}

const fakeID = `#!/bin/sh
FS="$FAKE_ROOT/fs"
me=${FAKE_USER:-root}; uid=${FAKE_UID:-0}
lookup() { awk -F: -v u="$1" '$1 == u { print; f = 1 } END { if (!f) exit 1 }' "$FS/etc/passwd"; }
case "${1:-}" in
  -u)
    if test $# -ge 2; then l=$(lookup "$2") || { echo "id: '$2': no such user" >&2; exit 1; }; printf '%s\n' "$l" | cut -d: -f3; else echo "$uid"; fi ;;
  -un) echo "$me" ;;
  -nG)
    u=${2:-$me}
    l=$(lookup "$u") || { echo "id: '$u': no such user" >&2; exit 1; }
    gid=$(printf '%s\n' "$l" | cut -d: -f4)
    awk -F: -v u="$u" -v g="$gid" '{ n = split($4, m, ","); hit = ($3 == g); for (i = 1; i <= n; i++) if (m[i] == u) hit = 1; if (hit) { printf "%s%s", sep, $1; sep = " " } } END { print "" }' "$FS/etc/group" ;;
  *) echo "uid=$uid($me)" ;;
esac
`

const fakeGetent = `#!/bin/sh
awk -F: -v k="$2" '$1 == k { print; f = 1 } END { if (!f) exit 2 }' "$FAKE_ROOT/fs/etc/$1"
`

const fakeSudo = `#!/bin/sh
printf '%s\n' "sudo $*" >> "$FAKE_ROOT/sudo.log"
mode=$(cat "$FAKE_ROOT/sudo-mode" 2>/dev/null || echo password)
test "$1" = -n && shift
case "$mode" in
  nopasswd) FAKE_UID=0; FAKE_USER=root; export FAKE_UID FAKE_USER; exec "$@" ;;
  *) echo "sudo: a password is required" >&2; exit 1 ;;
esac
`

const fakeUseradd = `#!/bin/sh
printf '%s\n' "useradd $*" >> "$FAKE_ROOT/changes.log"
FS="$FAKE_ROOT/fs"
home=""; shell=/bin/sh; group=""; usergroup=no
while test $# -gt 1; do
  case "$1" in
    --create-home|-m) ;;
    --home-dir|-d) home=$2; shift ;;
    --shell|-s) shell=$2; shift ;;
    --user-group|-U) usergroup=yes ;;
    -g) group=$2; shift ;;
  esac
  shift
done
name=$1
if grep -q "^$name:" "$FS/etc/passwd"; then echo "useradd: user '$name' already exists" >&2; exit 9; fi
uid=$(awk -F: 'BEGIN { m = 999 } $3 > m && $3 < 60000 { m = $3 } END { print m + 1 }' "$FS/etc/passwd")
if test "$usergroup" = yes; then printf '%s:x:%s:\n' "$name" "$uid" >> "$FS/etc/group"; gid=$uid
else gid=$(awk -F: -v g="$group" '$1 == g { print $3 }' "$FS/etc/group"); fi
printf '%s:x:%s:%s::%s:%s\n' "$name" "$uid" "$gid" "$home" "$shell" >> "$FS/etc/passwd"
mkdir -p "$home"
`

const fakeUsermod = `#!/bin/sh
printf '%s\n' "usermod $*" >> "$FAKE_ROOT/changes.log"
FS="$FAKE_ROOT/fs"
for u; do :; done
while test $# -gt 1; do
  case "$1" in
    -p) printf '%s %s\n' "$u" "$2" >> "$FAKE_ROOT/passwords"; shift ;;
    -aG) awk -F: -v OFS=: -v g="$2" -v u="$u" '$1 == g { if ($4 == "") $4 = u; else $4 = $4 "," u } { print }' "$FS/etc/group" > "$FS/etc/group.new" && mv "$FS/etc/group.new" "$FS/etc/group"; shift ;;
    --add-subuids|--add-subgids)
      f=subuid; test "$1" = --add-subgids && f=subgid
      s=${2%-*}; e=${2#*-}
      printf '%s:%s:%s\n' "$u" "$s" $((e - s + 1)) >> "$FS/etc/$f"; shift ;;
  esac
  shift
done
`

const fakeLoginctl = `#!/bin/sh
FS="$FAKE_ROOT/fs"
case "$1" in
  enable-linger) printf '%s\n' "loginctl $*" >> "$FAKE_ROOT/changes.log"; mkdir -p "$FS/var/lib/systemd/linger"; : > "$FS/var/lib/systemd/linger/$2" ;;
  show-user) if test -e "$FS/var/lib/systemd/linger/$2"; then echo yes; else echo no; fi ;;
  *) echo "fake loginctl: unsupported $1" >&2; exit 1 ;;
esac
`

const fakeSystemctl = `#!/bin/sh
printf '%s\n' "systemctl $*" >> "$FAKE_ROOT/systemctl.log"
FS="$FAKE_ROOT/fs"
S="$FAKE_ROOT/units"
mkdir -p "$S"
if test "$1" = --user; then
  shift
  case "$1" in
    show-environment)
      if test -e "$FS/run/user/${FAKE_UID:-0}/manager"; then echo "HOME=$HOME"; exit 0; fi
      echo "Failed to connect to bus: No such file or directory" >&2; exit 1 ;;
  esac
  exit 0
fi
cmd=""; units=""; now=no
for a; do
  case "$a" in
    --now) now=yes ;;
    -*) ;;
    *) if test -z "$cmd"; then cmd=$a; else units="$units $a"; fi ;;
  esac
done
case "$cmd" in
  start|enable|reload) printf '%s\n' "systemctl $*" >> "$FAKE_ROOT/changes.log" ;;
esac
case "$cmd" in
  start)
    for u in $units; do
      case "$u" in user@*.service) id=${u#user@}; id=${id%.service}; mkdir -p "$FS/run/user/$id"; : > "$FS/run/user/$id/manager" ;; esac
      : > "$S/$u.active"
    done ;;
  enable) for u in $units; do : > "$S/$u.enabled"; if test "$now" = yes; then : > "$S/$u.active"; fi; done ;;
  is-active) for u in $units; do test -e "$S/$u.active" || exit 3; done ;;
  is-enabled) for u in $units; do test -e "$S/$u.enabled" || exit 1; done ;;
  reload)
    for u in $units; do test -e "$S/$u.active" || { echo "Unit $u is not active." >&2; exit 1; }; done
    echo reload >> "$FAKE_ROOT/sshd-reloads"
    echo reload >> "$FAKE_ROOT/events" ;;
  list-unit-files) for u in $units; do if test -e "$S/$u.file"; then echo "$u disabled"; fi; done ;;
  daemon-reload|restart) ;;
  *) echo "fake systemctl: unsupported $cmd" >&2; exit 1 ;;
esac
`

const fakeRunuser = `#!/bin/sh
printf '%s\n' "runuser $*" >> "$FAKE_ROOT/runuser.log"
test "$1" = -u || { echo "fake runuser: expected -u" >&2; exit 1; }
u=$2; shift 2
test "$1" = -- && shift
l=$(awk -F: -v u="$u" '$1 == u { print }' "$FAKE_ROOT/fs/etc/passwd")
test -n "$l" || { echo "runuser: user $u does not exist" >&2; exit 1; }
FAKE_UID=$(printf '%s\n' "$l" | cut -d: -f3)
HOME=$(printf '%s\n' "$l" | cut -d: -f6)
FAKE_USER=$u
export FAKE_UID FAKE_USER HOME
exec "$@"
`

const fakeChown = `#!/bin/sh
printf '%s\n' "chown $*" >> "$FAKE_ROOT/chown.log"
`

const fakeSSHD = `#!/bin/sh
printf '%s\n' "sshd $*" >> "$FAKE_ROOT/sshd.log"
FS="$FAKE_ROOT/fs"
main="$FS/etc/ssh/sshd_config"
case "$1" in
  -t)
    echo sshd-t >> "$FAKE_ROOT/events"
    if test -f "$FAKE_ROOT/sshd-t-fail"; then echo "50-depper.conf line 3: Bad configuration option" >&2; exit 255; fi ;;
  -T)
    files=""
    if grep -qi '^Include' "$main"; then for f in "$FS"/etc/ssh/sshd_config.d/*.conf; do test -f "$f" && files="$files $f"; done; fi
    cat $files "$main" | awk '
      { k = tolower($1) }
      k == "" || k ~ /^#/ || k == "include" { next }
      !(k in v) { v[k] = tolower($2) }
      END {
        if (!("port" in v)) v["port"] = "22"
        if (!("passwordauthentication" in v)) v["passwordauthentication"] = "yes"
        if (!("kbdinteractiveauthentication" in v)) v["kbdinteractiveauthentication"] = "yes"
        if (!("permitrootlogin" in v)) v["permitrootlogin"] = "prohibit-password"
        if (!("pubkeyauthentication" in v)) v["pubkeyauthentication"] = "yes"
        for (k in v) print k, v[k]
      }' ;;
esac
`

const fakeDpkgQuery = `#!/bin/sh
for p; do :; done
if grep -qx "$p" "$FAKE_ROOT/packages"; then printf 'install ok installed'; exit 0; fi
echo "dpkg-query: no packages found matching $p" >&2; exit 1
`

const fakeRpm = `#!/bin/sh
for p; do :; done
if grep -qx "$p" "$FAKE_ROOT/packages"; then echo "$p-1.0"; exit 0; fi
echo "package $p is not installed"; exit 1
`

// fakePkgInstall installs packages into the fake host: it records them and
// installs fake programs (from pkgsrc) into pkg-bin.
const fakePkgInstall = `#!/bin/sh
FS="$FAKE_ROOT/fs"
inst() { cp "$FAKE_ROOT/pkgsrc/$1" "$FAKE_ROOT/pkg-bin/$1"; chmod 755 "$FAKE_ROOT/pkg-bin/$1"; }
for p; do
  if grep -qx "$p" "$FAKE_ROOT/unavailable" 2>/dev/null; then echo "E: Unable to locate package $p" >&2; exit 100; fi
  case "$p" in
    cloudflared) test -f "$FS/etc/apt/sources.list.d/cloudflared.list" || test -f "$FS/etc/yum.repos.d/cloudflared.repo" || { echo "E: Unable to locate package cloudflared" >&2; exit 100; } ;;
    tailscale) test -f "$FS/etc/apt/sources.list.d/tailscale.list" || test -f "$FS/etc/yum.repos.d/tailscale.repo" || { echo "E: Unable to locate package tailscale" >&2; exit 100; } ;;
  esac
done
for p; do
  grep -qx "$p" "$FAKE_ROOT/packages" || echo "$p" >> "$FAKE_ROOT/packages"
  case "$p" in
    rsync|curl|podman|cloudflared|ufw) inst "$p" ;;
    tailscale) inst tailscale; : > "$FAKE_ROOT/units/tailscaled.service.file" ;;
    docker.io|moby-engine) inst docker; grep -q '^docker:' "$FS/etc/group" || echo 'docker:x:998:' >> "$FS/etc/group"; : > "$FAKE_ROOT/units/docker.service.file" ;;
    firewalld) inst firewall-cmd; inst firewall-offline-cmd ;;
    uidmap|shadow-utils) inst newuidmap ;;
    dnf-automatic) : > "$FAKE_ROOT/units/dnf-automatic-install.timer.file" ;;
  esac
done
`

const fakeAptGet = `#!/bin/sh
printf '%s\n' "apt-get $*" >> "$FAKE_ROOT/changes.log"
test "${DEBIAN_FRONTEND:-}" = noninteractive || { echo "fake apt-get: DEBIAN_FRONTEND=noninteractive is required" >&2; exit 1; }
cmd=""; pkgs=""
while test $# -gt 0; do
  case "$1" in
    -o) shift ;;
    -*) ;;
    *) if test -z "$cmd"; then cmd=$1; else pkgs="$pkgs $1"; fi ;;
  esac
  shift
done
case "$cmd" in
  update) echo "Reading package lists..." ;;
  install) exec pkg-install $pkgs ;;
  *) echo "fake apt-get: unsupported $cmd" >&2; exit 1 ;;
esac
`

const fakeDnf = `#!/bin/sh
printf '%s\n' "dnf $*" >> "$FAKE_ROOT/changes.log"
cmd=""; pkgs=""
for a; do
  case "$a" in
    -*) ;;
    *) if test -z "$cmd"; then cmd=$a; else pkgs="$pkgs $a"; fi ;;
  esac
done
case "$cmd" in
  install) exec pkg-install $pkgs ;;
  *) echo "fake dnf: unsupported $cmd" >&2; exit 1 ;;
esac
`

const fakeCurl = `#!/bin/sh
printf '%s\n' "curl $*" >> "$FAKE_ROOT/changes.log"
out=""; url=""
while test $# -gt 0; do
  case "$1" in
    -o) out=$2; shift ;;
    -*) ;;
    *) url=$1 ;;
  esac
  shift
done
if test -n "$out"; then printf 'fetched %s\n' "$url" > "$out"; else printf 'fetched %s\n' "$url"; fi
`

const fakeUfw = `#!/bin/sh
printf '%s\n' "ufw $*" >> "$FAKE_ROOT/ufw.log"
R="$FAKE_ROOT/ufw-rules"
touch "$R"
case "$1" in
  status)
    if test -f "$FAKE_ROOT/ufw-active"; then
      echo "Status: active"; echo; echo "To                         Action      From"; echo "--                         ------      ----"
      while read -r p; do printf '%-27s%-12s%s\n' "$p" ALLOW Anywhere; done < "$R"
    else echo "Status: inactive"; fi ;;
  allow) grep -qx "$2" "$R" || echo "$2" >> "$R" ;;
  --force) if test "$2" = enable; then : > "$FAKE_ROOT/ufw-active"; mkdir -p "$FAKE_ROOT/fs/etc/ufw"; echo ENABLED=yes > "$FAKE_ROOT/fs/etc/ufw/ufw.conf"; fi ;;
  *) echo "fake ufw: unsupported $1" >&2; exit 1 ;;
esac
`

const fakeFirewallCmd = `#!/bin/sh
printf '%s\n' "${0##*/} $*" >> "$FAKE_ROOT/firewalld.log"
P="$FAKE_ROOT/firewalld-ports"
touch "$P"
for a; do
  case "$a" in
    --list-ports) tr '\n' ' ' < "$P"; echo ;;
    --list-services) echo "ssh dhcpv6-client" ;;
    --add-port=*) p=${a#--add-port=}; grep -qx "$p" "$P" || echo "$p" >> "$P" ;;
  esac
done
`

const fakeTailscale = `#!/bin/sh
printf '%s\n' "tailscale $*" >> "$FAKE_ROOT/tailscale.log"
case "$1" in
  status) test -f "$FAKE_ROOT/tailscale-up" ;;
  up)
    for a; do
      case "$a" in
        --authkey=file:*)
          f=${a#--authkey=file:}
          cat "$f" > "$FAKE_ROOT/tailscale-key-received"
          ls -l "$f" | cut -c1-10 > "$FAKE_ROOT/tailscale-key-mode"
          : > "$FAKE_ROOT/tailscale-up" ;;
      esac
    done
    test -f "$FAKE_ROOT/tailscale-up" ;;
esac
`

const stubTool = "#!/bin/sh\nexit 0\n"

const sha256Stub = "#!/bin/sh\nshasum -a 256 \"$@\" 2>/dev/null || echo 0\n"

// distro describes the fake host's operating system.
type distro struct {
	osRelease string
	sshUnit   string
	debian    bool
	// installed are the packages present before host init runs.
	installed []string
	// preBin are programs present before host init runs.
	preBin []string
}

var (
	debian12 = distro{
		osRelease: "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nNAME=\"Debian GNU/Linux\"\nVERSION_ID=\"12\"\nVERSION_CODENAME=bookworm\nID=debian\n",
		sshUnit:   "ssh.service", debian: true,
		installed: []string{"coreutils", "ca-certificates"},
	}
	ubuntu2404 = distro{
		osRelease: "PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nNAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\nVERSION_CODENAME=noble\nID=ubuntu\nID_LIKE=debian\nUBUNTU_CODENAME=noble\n",
		sshUnit:   "ssh.service", debian: true,
		installed: []string{"coreutils", "ca-certificates", "curl"},
		preBin:    []string{"curl"},
	}
	fedora40 = distro{
		osRelease: "NAME=\"Fedora Linux\"\nVERSION_ID=40\nID=fedora\nPRETTY_NAME=\"Fedora Linux 40 (Server Edition)\"\n",
		sshUnit:   "sshd.service",
		installed: []string{"coreutils", "ca-certificates", "shadow-utils", "curl"},
		preBin:    []string{"newuidmap", "curl"},
	}
	rocky9 = distro{
		osRelease: "NAME=\"Rocky Linux\"\nVERSION_ID=\"9.4\"\nID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\nPRETTY_NAME=\"Rocky Linux 9.4 (Blue Onyx)\"\n",
		sshUnit:   "sshd.service",
		installed: []string{"coreutils", "ca-certificates", "shadow-utils", "curl"},
		preBin:    []string{"newuidmap", "curl"},
	}
)

// fakeHost is a fake VPS rooted in a temporary directory.
type fakeHost struct {
	t       *testing.T
	Root    string
	FS      string
	Repo    string
	Home    string // the local (workstation) home with ~/.ssh
	KeyFile string
	Out     *syncBuffer
	Exec    *spyExec
	ssh     string
	keyscan string
}

func newFakeHost(t *testing.T, d distro) *fakeHost {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{t: t, Root: root, FS: filepath.Join(root, "fs"), Out: &syncBuffer{}}
	for _, dir := range []string{"bin", "remote-bin", "pkg-bin", "pkgsrc", "sysbin", "tmp", "units",
		"fs/etc/ssh/sshd_config.d", "fs/run/systemd/system", "fs/root", "fs/home", "fs/bin", "fs/var/lib/systemd"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range remoteToolAllowlist {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if err := os.Symlink(p, filepath.Join(root, "sysbin", name)); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
	quoted := quote(self)
	h.ssh = h.write("bin/ssh", "#!/bin/sh\nFAKE_ROOT="+quote(root)+" DEPPER_FAKE=ssh exec "+quoted+" \"$@\"\n", 0o755)
	h.keyscan = h.write("bin/ssh-keyscan", "#!/bin/sh\nFAKE_ROOT="+quote(root)+" DEPPER_FAKE=keyscan exec "+quoted+" \"$@\"\n", 0o755)
	for name, body := range map[string]string{
		"id": fakeID, "getent": fakeGetent, "sudo": fakeSudo, "useradd": fakeUseradd, "usermod": fakeUsermod,
		"loginctl": fakeLoginctl, "systemctl": fakeSystemctl, "runuser": fakeRunuser, "chown": fakeChown,
		"sshd": fakeSSHD, "pkg-install": fakePkgInstall, "sha256sum": sha256Stub,
	} {
		h.write("remote-bin/"+name, body, 0o755)
	}
	if d.debian {
		h.write("remote-bin/dpkg-query", fakeDpkgQuery, 0o755)
		h.write("remote-bin/apt-get", fakeAptGet, 0o755)
	} else {
		h.write("remote-bin/rpm", fakeRpm, 0o755)
		h.write("remote-bin/dnf", fakeDnf, 0o755)
	}
	for name, body := range map[string]string{
		"rsync": stubTool, "podman": stubTool, "docker": stubTool, "cloudflared": stubTool, "newuidmap": stubTool,
		"curl": fakeCurl, "ufw": fakeUfw, "firewall-cmd": fakeFirewallCmd, "firewall-offline-cmd": fakeFirewallCmd,
		"tailscale": fakeTailscale,
	} {
		h.write("pkgsrc/"+name, body, 0o755)
	}
	for _, name := range d.preBin {
		data, err := os.ReadFile(filepath.Join(root, "pkgsrc", name))
		if err != nil {
			t.Fatal(err)
		}
		h.write("pkg-bin/"+name, string(data), 0o755)
	}
	h.write("packages", strings.Join(d.installed, "\n")+"\n", 0o644)
	h.write("fs/etc/os-release", d.osRelease, 0o644)
	h.write("fs/etc/passwd", "root:x:0:0:root:"+filepath.Join(h.FS, "root")+":/bin/bash\n", 0o644)
	h.write("fs/etc/group", "root:x:0:\nsudo:x:27:\nwheel:x:10:\n", 0o644)
	h.write("fs/etc/ssh/sshd_config", "Include /etc/ssh/sshd_config.d/*.conf\nPermitRootLogin yes\nPasswordAuthentication yes\n", 0o644)
	h.write("fs/bin/bash", stubTool, 0o755)
	h.write("units/"+d.sshUnit+".active", "", 0o644)
	h.write("admin-user", "root\n", 0o644)
	h.write("admin-uid", "0\n", 0o644)
	h.write("hostkeys", "ssh-ed25519 "+fakeKeyBlob("ssh-ed25519", 32)+"\necdsa-sha2-nistp256 "+fakeKeyBlob("ecdsa-sha2-nistp256", 65)+"\n", 0o644)

	// The workstation: a home with an ed25519 public key (no private key:
	// it lives in the agent) and a repository with depper.toml.
	h.Home = filepath.Join(root, "local-home")
	blob := fakeKeyBlob("ssh-ed25519", 32)
	h.write("client-key-blob", blob+"\n", 0o644)
	h.KeyFile = h.write("local-home/.ssh/id_ed25519.pub", "ssh-ed25519 "+blob+" me@laptop\n", 0o644)
	h.Repo = filepath.Join(root, "repo")
	h.write("repo/depper.toml", sampleConfig, 0o644)

	h.Exec = &spyExec{inner: transport.Local{}}
	return h
}

const sampleConfig = `# depper configuration for app
[service]
name = "app"
command = "./run" # start command

[deploy]
# The deploy host (placeholder from depper init)
host = "deploy@203.0.113.10"   # replace me
`

// fakeKeyBlob returns a base64 SSH public key blob of the given type with
// random key material.
func fakeKeyBlob(keyType string, n int) string {
	var b bytes.Buffer
	for _, part := range [][]byte{[]byte(keyType), randomBytes(n)} {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(part)))
		b.Write(part)
	}
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func (h *fakeHost) write(rel, body string, mode os.FileMode) string {
	h.t.Helper()
	path := filepath.Join(h.Root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		h.t.Fatal(err)
	}
	return path
}

func (h *fakeHost) read(rel string) string {
	data, _ := os.ReadFile(filepath.Join(h.Root, rel))
	return string(data)
}

func (h *fakeHost) has(rel string) bool { return exists(filepath.Join(h.Root, rel)) }

func (h *fakeHost) remove(rel string) {
	h.t.Helper()
	if err := os.RemoveAll(filepath.Join(h.Root, rel)); err != nil {
		h.t.Fatal(err)
	}
}

// runner returns a Runner wired to the fake host.
func (h *fakeHost) runner() *Runner {
	return &Runner{
		Exec: h.Exec, SSHProgram: h.ssh, KeyscanProgram: h.keyscan, Out: h.Out,
		LookupEnv: func(string) (string, bool) { return "", false },
		Now:       func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) },
		Version:   "v0.2.0-test",
		fsRoot:    h.FS, pathSet: true, sysPath: "", managerWait: 2, retryDelay: 10 * time.Millisecond,
	}
}

// options returns minimal options for root@127.0.0.1.
func (h *fakeHost) options() *Options {
	return &Options{
		Repo: h.Repo, Admin: "root", Host: "127.0.0.1", Port: 22, User: "deploy",
		KeyFile: h.KeyFile, Container: ContainerNone, KnownHosts: filepath.Join(h.Repo, ".depper", "known_hosts"),
		WriteConfig: true, Yes: true,
	}
}

// changes returns the log of state-changing fake commands.
func (h *fakeHost) changes() string { return h.read("changes.log") }

// spyExec records every command's argv and stdin, then delegates.
type spyExec struct {
	inner transport.Executor
	mu    sync.Mutex
	calls [][]string
	stdin []string
}

func (s *spyExec) Run(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	input := ""
	if c.Stdin != nil {
		data, err := io.ReadAll(c.Stdin)
		if err != nil {
			return transport.Result{}, err
		}
		input = string(data)
		c.Stdin = strings.NewReader(input)
	}
	s.mu.Lock()
	s.calls = append(s.calls, append([]string(nil), c.Args...))
	s.stdin = append(s.stdin, input)
	s.mu.Unlock()
	return s.inner.Run(ctx, c)
}

// Scripts returns the stdin of every ssh script call.
func (s *spyExec) Scripts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for i, c := range s.calls {
		if len(c) > 0 && c[len(c)-1] == transport.RemoteShell {
			out = append(out, s.stdin[i])
		}
	}
	return out
}

// Argv returns every recorded argv joined by spaces.
func (s *spyExec) Argv() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

// checkScriptSyntax runs `sh -n` and, when available, `dash -n` on every
// script.
func checkScriptSyntax(t *testing.T, scripts []string) {
	t.Helper()
	checkers := [][]string{{"sh", "-n"}}
	if p, err := exec.LookPath("dash"); err == nil {
		checkers = append(checkers, []string{p, "-n"})
	}
	if p, err := exec.LookPath("checkbashisms"); err == nil {
		checkers = append(checkers, []string{p, "-"})
	}
	for i, script := range scripts {
		for _, c := range checkers {
			cmd := exec.Command(c[0], c[1:]...)
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%v rejected script %d: %v\n%s\n%s", c, i, err, out, script)
			}
		}
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The fake host must never resolve a real package manager, init system, or
// any other guarded tool, whatever the machine running the tests has.
func TestFakeHostPathIsolation(t *testing.T) {
	scratch := t.TempDir()
	for _, name := range guardedTools {
		if err := os.WriteFile(filepath.Join(scratch, name), []byte(stubTool), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", scratch+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := newFakeHost(t, debian12)
	path := fakeRemotePATH(h.Root)
	script := "for t in " + strings.Join(guardedTools, " ") + "; do command -v $t || true; done"
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + path}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Fields(string(out)) {
		dir := filepath.Dir(line)
		if dir != filepath.Join(h.Root, "remote-bin") && dir != filepath.Join(h.Root, "pkg-bin") {
			t.Errorf("guarded tool resolves outside the fakes: %s", line)
		}
	}
}
