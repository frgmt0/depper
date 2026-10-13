package host

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/frgmt0/depper/internal/shell"
)

// Step IDs, in plan order.
const (
	StepKnownHosts    = "known-hosts"
	StepPackages      = "packages"
	StepCloudflared   = "cloudflared"
	StepTailscalePkg  = "tailscale-install"
	StepUser          = "user"
	StepAuthorizedKey = "authorized-key"
	StepLinger        = "linger"
	StepUserManager   = "user-manager"
	StepSubIDs        = "subids"
	StepDockerGroup   = "docker-group"
	StepDockerService = "docker-service"
	StepFirewall      = "firewall"
	StepAutoUpdates   = "auto-updates"
	StepTailscaleUp   = "tailscale-up"
	StepHardenSSH     = "harden-ssh"
	StepRecord        = "record"
	StepConfig        = "config"
)

// Step is one plan entry. Change is false for steps already satisfied.
type Step struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Change bool   `json:"change"`
	Detail string `json:"detail,omitempty"`

	lines    []string
	packages []string
	// prepare and revert belong to a guarded step (the firewall): prepare
	// writes the markers of what to undo into the guard's state directory
	// ($d) before the step runs; revert undoes it, acting only on those
	// markers and removing each one it handled. revert runs from a transient
	// systemd timer armed before the step and disarmed once new logins
	// succeed, or at once when the step fails.
	prepare []string
	revert  []string
}

// Plan is what host init will do.
type Plan struct {
	Steps    []*Step  `json:"steps"`
	Warnings []string `json:"warnings"`
	// Packages are the packages depper installs (for the record).
	Packages []string `json:"packages"`

	// rootBefore is the PermitRootLogin sshd uses without depper's drop-in
	// and hardenRoot whether the drop-in sets PermitRootLogin (only when
	// rootBefore is yes).
	rootBefore string
	hardenRoot bool
}

// Changes reports whether any step changes something.
func (p *Plan) Changes() bool {
	for _, s := range p.Steps {
		if s.Change {
			return true
		}
	}
	return false
}

func (p *Plan) step(id string) *Step {
	for _, s := range p.Steps {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func (p *Plan) add(s *Step) *Step {
	p.Steps = append(p.Steps, s)
	return s
}

func (p *Plan) warn(format string, args ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, args...))
}

// ErrUnsupported reports a host depper cannot prepare.
type ErrUnsupported struct{ Reason string }

func (e *ErrUnsupported) Error() string { return e.Reason }

var codenameRe = regexp.MustCompile(`^[a-z][a-z0-9]{1,31}$`)

// DockerGroupWarning is printed whenever the deploy user joins the docker
// group.
const DockerGroupWarning = "membership in the docker group is equivalent to root on this host: anyone who can log in as the deploy user (or run code through a deployed service) can take over the machine. Prefer rootless podman (engine = \"podman\" in [container]) unless you need docker."

// firewallPorts returns the TCP ports to allow in the firewall: the
// requested ones, the port host init connects to, and every port sshd
// listens on (they differ behind a port forward or NAT).
func firewallPorts(o *Options, f *Facts) []int {
	ports := unionInts(o.Firewall, []int{o.Port})
	if f != nil {
		ports = unionInts(ports, f.SSHDPorts)
		ports = unionInts(ports, f.SSHSocketPorts)
	}
	return ports
}

func (r *Runner) aptLines() []string {
	return []string{
		"export DEBIAN_FRONTEND=noninteractive",
		`apt_opts="-o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"`,
	}
}

func (r *Runner) installLines(family string, pkgs []string, update bool) []string {
	if family == FamilyDebian {
		lines := r.aptLines()
		if update {
			lines = append(lines, "apt-get $apt_opts -q update")
		}
		return append(lines, "apt-get $apt_opts -q -y install "+shell.Join(pkgs...))
	}
	return []string{"dnf -y -q install " + shell.Join(pkgs...)}
}

// buildPlan decides every remote step from the facts. It refuses
// unsupported hosts and unsafe hardening before anything changes.
func (r *Runner) buildPlan(o *Options, f *Facts, key PublicKey) (*Plan, error) {
	if f.Family == "" {
		id := f.OSID
		if id == "" {
			id = "unknown (no /etc/os-release)"
		}
		return nil, &ErrUnsupported{Reason: fmt.Sprintf("unsupported operating system %q (ID_LIKE %q): depper host init supports Debian/Ubuntu (apt) and Fedora/RHEL/CentOS/Rocky/AlmaLinux (dnf)", id, f.OSLike)}
	}
	if f.OSID == "amzn" {
		major, _, _ := strings.Cut(f.VersionID, ".")
		if n, err := strconv.Atoi(major); err != nil || n < 2023 {
			return nil, &ErrUnsupported{Reason: fmt.Sprintf("Amazon Linux %s is not supported: depper host init supports Amazon Linux 2023 and later (Amazon Linux 2 has no dnf and an older systemd)", shellName(f.VersionID))}
		}
	}
	if !f.Systemd {
		return nil, &ErrUnsupported{Reason: fmt.Sprintf("%s is not running systemd (no /run/systemd/system); depper needs a systemd user manager", o.Host)}
	}
	if f.Commands["runuser"] == "" {
		return nil, &ErrUnsupported{Reason: "runuser (util-linux) is not installed on the host; depper needs it to act as the deploy user"}
	}
	p := &Plan{Warnings: []string{}, Packages: []string{}}
	u := shell.Quote(o.User)
	fedoraProper := f.OSID == "fedora"

	// Packages from the distribution.
	reqs := []pkgReq{{Pkg: "rsync", Cmd: "rsync"}, {Pkg: "curl", Cmd: "curl"}, {Pkg: "ca-certificates"}, {Pkg: "coreutils", Cmd: "sha256sum"}}
	amazon := f.OSID == "amzn"
	switch o.Container {
	case ContainerPodman:
		if amazon {
			return nil, &ErrUnsupported{Reason: fmt.Sprintf("Amazon Linux (%s) has no supported rootless podman packages; use docker (--container docker, or engine = \"docker\" in [container])", f.VersionID)}
		}
		reqs = append(reqs, pkgReq{Pkg: "podman", Cmd: "podman"})
		if f.Family == FamilyDebian {
			reqs = append(reqs, pkgReq{Pkg: "uidmap", Cmd: "newuidmap"}, pkgReq{Pkg: "slirp4netns", Cmd: ""})
		} else if fedoraProper {
			reqs = append(reqs, pkgReq{Pkg: "shadow-utils", Cmd: "newuidmap"}, pkgReq{Pkg: "passt", Cmd: ""})
		} else {
			reqs = append(reqs, pkgReq{Pkg: "shadow-utils", Cmd: "newuidmap"}, pkgReq{Pkg: "slirp4netns", Cmd: ""})
		}
	case ContainerDocker:
		switch {
		case f.Family == FamilyDebian:
			reqs = append(reqs, pkgReq{Pkg: "docker.io", Cmd: "docker"})
		case fedoraProper:
			reqs = append(reqs, pkgReq{Pkg: "moby-engine", Cmd: "docker"})
		case amazon:
			reqs = append(reqs, pkgReq{Pkg: "docker", Cmd: "docker"})
		default:
			return nil, &ErrUnsupported{Reason: fmt.Sprintf("%s has no distribution docker package; install docker-ce from Docker's official repository (https://docs.docker.com/engine/install/) yourself, or use podman (engine = \"podman\" in [container])", f.OSID)}
		}
	}
	if o.Firewall != nil {
		if f.Family == FamilyDebian {
			reqs = append(reqs, pkgReq{Pkg: "ufw", Cmd: "ufw"})
		} else {
			reqs = append(reqs, pkgReq{Pkg: "firewalld", Cmd: "firewall-cmd"})
		}
	}
	if o.AutoUpdates {
		if f.Family == FamilyDebian {
			reqs = append(reqs, pkgReq{Pkg: "unattended-upgrades", Cmd: ""})
		} else {
			reqs = append(reqs, pkgReq{Pkg: "dnf-automatic", Cmd: ""})
		}
	}
	if o.Compose && o.Container != ContainerNone {
		req, err := composeProvider(o.Container, f)
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, req)
	}
	var missing, present []string
	for _, req := range reqs {
		if f.has(req) {
			present = append(present, req.Pkg)
		} else {
			missing = append(missing, req.Pkg)
		}
	}
	pkgStep := p.add(&Step{ID: StepPackages, Title: "Install packages"})
	if len(missing) > 0 {
		pkgStep.Change = true
		pkgStep.Detail = "install " + strings.Join(missing, " ") + " with " + map[string]string{FamilyDebian: "apt-get", FamilyFedora: "dnf"}[f.Family]
		pkgStep.lines = r.installLines(f.Family, missing, true)
		pkgStep.packages = missing
	} else {
		pkgStep.Detail = "already installed: " + strings.Join(present, " ")
	}

	// cloudflared from Cloudflare's signed package repository.
	if o.Cloudflared {
		s := p.add(&Step{ID: StepCloudflared, Title: "Install cloudflared from pkg.cloudflare.com"})
		if f.has(pkgReq{Pkg: "cloudflared", Cmd: "cloudflared"}) {
			s.Detail = "already installed"
		} else {
			s.Change, s.packages = true, []string{"cloudflared"}
			if f.Family == FamilyDebian {
				keyring := r.raw("/usr/share/keyrings/cloudflare-main.gpg")
				list := r.raw("/etc/apt/sources.list.d/cloudflared.list")
				s.Detail = "add the Cloudflare apt repository (signed-by /usr/share/keyrings/cloudflare-main.gpg) and install cloudflared"
				s.lines = append([]string{
					"mkdir -p " + r.p("/usr/share/keyrings") + " " + r.p("/etc/apt/sources.list.d"),
					"chmod 755 " + r.p("/usr/share/keyrings"),
					"curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o " + shell.Quote(keyring+".depper-tmp"),
					"chmod 644 " + shell.Quote(keyring+".depper-tmp"),
					"mv -f " + shell.Quote(keyring+".depper-tmp") + " " + shell.Quote(keyring),
					"printf '%s\\n' " + shell.Quote("deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main") + " > " + shell.Quote(list),
				}, r.installLines(f.Family, []string{"cloudflared"}, true)...)
			} else {
				repo := r.raw("/etc/yum.repos.d/cloudflared.repo")
				s.Detail = "add https://pkg.cloudflare.com/cloudflared.repo and install cloudflared"
				s.lines = append([]string{
					"mkdir -p " + r.p("/etc/yum.repos.d"),
					"curl -fsSL https://pkg.cloudflare.com/cloudflared.repo -o " + shell.Quote(repo+".depper-tmp"),
					"chmod 644 " + shell.Quote(repo+".depper-tmp"),
					"mv -f " + shell.Quote(repo+".depper-tmp") + " " + shell.Quote(repo),
				}, r.installLines(f.Family, []string{"cloudflared"}, false)...)
			}
		}
	}

	// Tailscale from its official package repository.
	if o.TailscaleKey.Set() {
		s := p.add(&Step{ID: StepTailscalePkg, Title: "Install Tailscale from pkgs.tailscale.com"})
		if f.has(pkgReq{Pkg: "tailscale", Cmd: "tailscale"}) {
			s.Detail = "already installed"
		} else {
			lines, detail, err := r.tailscaleInstall(f)
			if err != nil {
				return nil, err
			}
			s.Change, s.Detail, s.lines, s.packages = true, detail, lines, []string{"tailscale"}
		}
	}

	// The deploy account.
	s := p.add(&Step{ID: StepUser, Title: "Create the deploy user " + o.User})
	if f.UserExists {
		if f.UserUID == 0 {
			return nil, fmt.Errorf("the account %q exists with uid 0; choose another --user", o.User)
		}
		s.Detail = fmt.Sprintf("exists (uid %d, home %s)", f.UserUID, f.UserHome)
		var repairs []string
		// A locked password ("!" or "!!", what useradd leaves) makes sshd
		// refuse even key logins when UsePAM is off; "*" only disables
		// password login.
		if f.UserPassword == "locked" && f.SSHD["usepam"] == "no" {
			repairs = append(repairs, "unlock key login (usermod -p '*': the password field is locked and sshd has UsePAM no)")
			s.lines = append(s.lines, "usermod -p '*' "+u)
		}
		if sh := path.Base(f.UserShell); f.UserShell == "" || sh == "nologin" || sh == "false" {
			if f.Record != nil && f.Record.DeployUser == o.User && contains(f.Record.Steps, StepUser) {
				repairs = append(repairs, "set the login shell to /bin/sh (it is "+shellName(f.UserShell)+")")
				s.lines = append(s.lines, "usermod -s /bin/sh "+u)
			} else {
				p.warn("the existing account %s has the login shell %s, so depper cannot run commands as it over SSH; depper did not create it and leaves it alone: give it a shell (for example `sudo usermod -s /bin/sh %s`)", o.User, shellName(f.UserShell), o.User)
			}
		}
		if len(repairs) > 0 {
			s.Change = true
			s.Detail = "exists; " + strings.Join(repairs, "; ")
		}
		for _, g := range f.UserGroups {
			switch g {
			case "sudo", "wheel", "admin", "root":
				p.warn("the existing account %s is in the %s group, so it can become root; depper does not remove it, but a deploy account should not have administrator rights", o.User, g)
			case "docker":
				if o.Container != ContainerDocker {
					p.warn("the existing account %s is in the docker group; %s", o.User, DockerGroupWarning)
				}
			}
		}
	} else {
		s.Change = true
		s.Detail = "no password, home " + "/home/" + o.User + ", shell /bin/bash (or /bin/sh), no sudo"
		home := shell.Quote(r.raw("/home/" + o.User))
		s.lines = []string{
			"depper_shell=/bin/sh",
			"if test -x " + r.p("/bin/bash") + "; then depper_shell=/bin/bash; fi",
			"if getent group " + u + " >/dev/null 2>&1; then depper_grp=\"-g " + o.User + "\"; else depper_grp=--user-group; fi",
			"useradd --create-home --home-dir " + home + " --shell \"$depper_shell\" $depper_grp " + u,
			// "*" disables password login without locking the account
			// ("!"), which sshd treats as locked when UsePAM is off.
			"usermod -p '*' " + u,
		}
	}

	// Its authorized key, appended and deduplicated, written as the user.
	s = p.add(&Step{ID: StepAuthorizedKey, Title: "Authorize the public key for " + o.User})
	restricted := f.UserExists && !f.KeyPresent && f.KeyRestricted
	switch {
	case f.UserExists && f.KeyPresent:
		s.Detail = key.Type + " " + key.Fingerprint() + " already authorized"
	case restricted && !o.KeyExplicit:
		s.Detail = key.Type + " " + key.Fingerprint() + " is authorized only with options (command=, from=, restrict, ...); left as is"
		p.warn("~%s/.ssh/authorized_keys has the key %s only on a line with options (command=, from=, restrict, no-pty, ...), which may stop depper from running commands or forwarding the health check port; depper leaves it alone. Pass --key %s explicitly to add an unrestricted line as well", o.User, key.Fingerprint(), displayPath(o.KeyFile, ""))
	default:
		s.Change = true
		s.Detail = "append " + key.Type + " " + key.Fingerprint() + " to ~" + o.User + "/.ssh/authorized_keys (700/600, owned by " + o.User + ")"
		if restricted {
			s.Detail = "append an unrestricted line for " + key.Type + " " + key.Fingerprint() + " (it is authorized only with options) to ~" + o.User + "/.ssh/authorized_keys"
			p.warn("~%s/.ssh/authorized_keys has the key %s only on a line with options; --key was given, so depper appends an unrestricted line for it (the restricted line stays)", o.User, key.Fingerprint())
		}
		install := `set -eu
umask 077
home=$1; line=$2; match=$3
d="$home/.ssh"; ak="$d/authorized_keys"
if test -L "$d" || test -L "$ak"; then echo "refusing: $d or $ak is a symbolic link" >&2; exit 1; fi
mkdir -p "$d"
chmod 700 "$d"
touch "$ak"
chmod 600 "$ak"
if test "$(awk -v k="$match" '` + authorizedKeyAwk + `' "$ak")" != plain; then
  if test -s "$ak" && test -n "$(tail -c 1 "$ak")"; then echo >> "$ak"; fi
  printf '%s\n' "$line" >> "$ak"
fi`
		s.lines = []string{
			"depper_home=$(getent passwd " + u + " | cut -d: -f6)",
			`test -n "$depper_home" || { echo "cannot find the home directory of ` + o.User + `" >&2; exit 1; }`,
			"runuser -u " + u + " -- sh -c " + shell.Quote(install) + " depper-authorize \"$depper_home\" " + shell.Quote(key.Line()) + " " + shell.Quote(key.Type+" "+key.Blob),
		}
	}

	// Linger and the user manager.
	s = p.add(&Step{ID: StepLinger, Title: "Enable lingering for " + o.User})
	if f.UserExists && f.Linger {
		s.Detail = "already enabled"
	} else {
		s.Change, s.Detail = true, "loginctl enable-linger "+o.User
		s.lines = []string{"loginctl enable-linger " + u}
	}
	s = p.add(&Step{ID: StepUserManager, Title: "Start the systemd user manager for " + o.User})
	if f.UserExists && f.UserManager {
		s.Detail = "`systemctl --user` works as " + o.User
	} else {
		s.Change, s.Detail = true, "systemctl start user@UID.service, then check `systemctl --user` as "+o.User
		wait := r.managerWait
		if wait == 0 {
			wait = 30
		}
		s.lines = []string{
			"depper_uid=$(id -u " + u + ")",
			`systemctl start "user@$depper_uid.service"`,
			`depper_rt=` + r.p("/run/user") + `/"$depper_uid"`,
			"depper_i=0",
			"until runuser -u " + u + ` -- env XDG_RUNTIME_DIR="$depper_rt" DBUS_SESSION_BUS_ADDRESS="unix:path=$depper_rt/bus" systemctl --user show-environment >/dev/null 2>&1; do`,
			`  depper_i=$((depper_i + 1))`,
			"  if test \"$depper_i\" -ge " + strconv.Itoa(wait) + "; then echo \"the systemd user manager for " + o.User + " did not start (systemctl --user fails)\" >&2; exit 1; fi",
			"  sleep 1",
			"done",
		}
	}

	// Rootless podman needs subordinate id ranges; docker needs its group
	// and service.
	switch o.Container {
	case ContainerPodman:
		s = p.add(&Step{ID: StepSubIDs, Title: "Subordinate uid/gid ranges for rootless podman"})
		if f.UserExists && f.SubUID && f.SubGID {
			s.Detail = "/etc/subuid and /etc/subgid already have ranges for " + o.User
		} else {
			s.Change, s.Detail = true, "add a 65536-id range for "+o.User+" to /etc/subuid and /etc/subgid"
			s.lines = []string{
				"touch " + r.p("/etc/subuid") + " " + r.p("/etc/subgid"),
				"depper_start=$(cat " + r.p("/etc/subuid") + " " + r.p("/etc/subgid") + ` | awk -F: 'BEGIN { m = 100000 } NF >= 3 && $2 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/ { e = $2 + $3; if (e > m) m = e } END { print m }')`,
				`depper_range="$depper_start-$((depper_start + 65535))"`,
				"if ! grep -q " + shell.Quote("^"+o.User+":") + " " + r.p("/etc/subuid") + "; then usermod --add-subuids \"$depper_range\" " + u + "; fi",
				"if ! grep -q " + shell.Quote("^"+o.User+":") + " " + r.p("/etc/subgid") + "; then usermod --add-subgids \"$depper_range\" " + u + "; fi",
			}
		}
	case ContainerDocker:
		p.warn("%s", DockerGroupWarning)
		s = p.add(&Step{ID: StepDockerGroup, Title: "Add " + o.User + " to the docker group"})
		if f.UserExists && contains(f.UserGroups, "docker") {
			s.Detail = "already a member"
		} else {
			s.Change, s.Detail = true, "usermod -aG docker "+o.User+" (root-equivalent)"
			s.lines = []string{"usermod -aG docker " + u}
		}
		s = p.add(&Step{ID: StepDockerService, Title: "Enable the docker service"})
		if f.DockerActive && f.DockerEnabled {
			s.Detail = "docker.service is enabled and running"
		} else {
			s.Change, s.Detail = true, "systemctl enable --now docker.service"
			s.lines = []string{"systemctl enable --now docker.service"}
		}
	}

	// Firewall (opt-in). It runs in its own guarded phase (see
	// Runner.guarded): a timed revert is armed first and disarmed only after
	// new admin and deploy logins succeed.
	if o.Firewall != nil {
		want := firewallPorts(o, f)
		portList := intsString(want)
		if listen := unionInts(f.SSHDPorts, f.SSHSocketPorts); len(listen) > 0 && !containsInt(listen, o.Port) {
			p.warn("host init connects to port %d, but sshd listens on %s: assuming a port forward or NAT in between, so the firewall allows %s", o.Port, intsString(listen), portList)
		}
		if f.Family == FamilyDebian {
			s = p.add(&Step{ID: StepFirewall, Title: "Firewall (ufw): allow TCP " + portList})
			var add []int
			for _, port := range want {
				if !containsInt(f.UFWPorts, port) {
					add = append(add, port)
				}
			}
			if f.UFWActive && len(add) == 0 {
				s.Detail = "ufw is active and allows " + portList
			} else {
				s.Change = true
				for _, port := range add {
					s.lines = append(s.lines, "ufw allow "+strconv.Itoa(port)+"/tcp")
				}
				var parts []string
				if len(add) > 0 {
					parts = append(parts, "allow "+intsString(add))
				}
				if !f.UFWActive {
					s.lines = append(s.lines, "ufw --force enable")
					parts = append(parts, "enable ufw")
				}
				s.prepare, s.revert = firewallMarkers(!f.UFWActive, add,
					[]string{"ufw --force disable"},
					nil,
					func(pt string) []string { return []string{"ufw delete allow " + pt + " || true"} })
				s.Detail = strings.Join(parts, ", then ") + "; reverted automatically unless new admin and " + o.User + " logins succeed"
			}
		} else {
			if f.FirewalldPresent && f.FirewalldZone == "" {
				return nil, fmt.Errorf("--firewall: cannot determine the firewalld zone to open the ports in (firewall-cmd --get-zone-of-interface / --get-default-zone gave no usable zone name)")
			}
			zoneDesc := "the zone of " + f.SSHInterface
			if f.SSHInterface == "" {
				zoneDesc = "the default zone"
			}
			if f.FirewalldZone != "" {
				zoneDesc += " (" + f.FirewalldZone + ")"
			}
			if f.FirewalldPresent && f.FirewalldZoneDefault {
				iface := f.SSHInterface
				if iface == "" {
					iface = "(unknown: SSH_CONNECTION or `ip -o addr` gave no interface)"
				}
				p.warn("the interface of the admin connection, %s, has no firewalld zone, so the ports are opened in the default zone %s", iface, f.FirewalldZone)
			}
			s = p.add(&Step{ID: StepFirewall, Title: "Firewall (firewalld): allow TCP " + portList + " in " + zoneDesc})
			var add []int
			for _, port := range want {
				if containsInt(f.FirewalldPorts, port) || (port == 22 && f.FirewalldSSHSvc) {
					continue
				}
				add = append(add, port)
			}
			if f.FirewalldActive && len(add) == 0 {
				s.Detail = "firewalld is active and allows " + portList
			} else {
				s.Change = true
				fc := "firewall-offline-cmd"
				if f.FirewalldActive {
					fc = "firewall-cmd"
				}
				zone := firewalldZoneLines(fc, f.SSHInterface)
				s.lines = append(s.lines, zone...)
				if f.FirewalldActive {
					// Runtime and permanent, so nothing depends on a reload.
					for _, port := range add {
						pt := strconv.Itoa(port) + "/tcp"
						s.lines = append(s.lines,
							`firewall-cmd --zone="$depper_zone" --add-port=`+pt,
							`firewall-cmd --permanent --zone="$depper_zone" --add-port=`+pt)
					}
					s.prepare, s.revert = firewallMarkers(false, add, nil, zone,
						func(pt string) []string {
							return []string{
								`firewall-cmd --zone="$depper_zone" --remove-port=` + pt + ` || true`,
								`firewall-cmd --permanent --zone="$depper_zone" --remove-port=` + pt + ` || true`,
							}
						})
					s.Detail = "allow " + intsString(add) + " (runtime and permanent)"
				} else {
					for _, port := range add {
						s.lines = append(s.lines, `firewall-offline-cmd --zone="$depper_zone" --add-port=`+strconv.Itoa(port)+"/tcp")
					}
					s.lines = append(s.lines, "systemctl enable --now firewalld.service")
					s.prepare, s.revert = firewallMarkers(true, nil, []string{"systemctl disable --now firewalld.service"}, nil, nil)
					s.Detail = "allow " + intsString(add) + ", then enable firewalld"
					if len(add) == 0 {
						s.Detail = "enable firewalld"
					}
				}
				s.Detail += "; reverted automatically unless new admin and " + o.User + " logins succeed"
			}
		}
		if s.Change {
			if err := guardSupported(f, "--firewall"); err != nil {
				return nil, err
			}
		}
	}

	// Automatic security updates (opt-in).
	if o.AutoUpdates {
		s = p.add(&Step{ID: StepAutoUpdates, Title: "Automatic security updates"})
		if f.Family == FamilyDebian {
			if f.has(pkgReq{Pkg: "unattended-upgrades", Cmd: ""}) && f.AutoUpdatesConf {
				s.Detail = "unattended-upgrades is enabled"
			} else {
				conf := r.raw("/etc/apt/apt.conf.d/20auto-upgrades")
				s.Change, s.Detail = true, "enable unattended-upgrades (/etc/apt/apt.conf.d/20auto-upgrades)"
				s.lines = []string{
					"mkdir -p " + r.p("/etc/apt/apt.conf.d"),
					"printf '%s\\n' 'APT::Periodic::Update-Package-Lists \"1\";' 'APT::Periodic::Unattended-Upgrade \"1\";' > " + shell.Quote(conf+".depper-tmp"),
					"chmod 644 " + shell.Quote(conf+".depper-tmp"),
					"mv -f " + shell.Quote(conf+".depper-tmp") + " " + shell.Quote(conf),
				}
			}
		} else {
			ok := f.has(pkgReq{Pkg: "dnf-automatic", Cmd: ""}) && f.AutoUpdatesConf &&
				(f.AutoUpdatesTimer == "dnf-automatic-install.timer" || (f.AutoUpdatesTimer == "dnf5-automatic.timer" && f.AutoUpdatesApply))
			if ok {
				s.Detail = "dnf-automatic applies security updates (" + f.AutoUpdatesTimer + ")"
			} else {
				conf := shell.Quote(r.raw("/etc/dnf/automatic.conf"))
				tmp := shell.Quote(r.raw("/etc/dnf/automatic.conf") + ".depper-tmp")
				s.Change, s.Detail = true, "set upgrade_type = security in /etc/dnf/automatic.conf and enable the dnf-automatic timer"
				s.lines = []string{
					"if test -f " + conf + "; then",
					"  sed -e 's/^[[:space:]]*upgrade_type[[:space:]]*=.*/upgrade_type = security/' -e 's/^[[:space:]]*apply_updates[[:space:]]*=.*/apply_updates = yes/' " + conf + " > " + tmp,
					"  cat " + tmp + " > " + conf,
					"  rm -f " + tmp,
					"else",
					"  mkdir -p " + r.p("/etc/dnf"),
					"  printf '%s\\n' '[commands]' 'upgrade_type = security' 'apply_updates = yes' > " + conf,
					"fi",
					"grep -Eq '^upgrade_type = security' " + conf + ` || { echo "could not set upgrade_type = security in /etc/dnf/automatic.conf" >&2; exit 1; }`,
					"if systemctl list-unit-files dnf-automatic-install.timer 2>/dev/null | grep -q dnf-automatic-install.timer; then",
					"  systemctl enable --now dnf-automatic-install.timer",
					"elif systemctl list-unit-files dnf5-automatic.timer 2>/dev/null | grep -q dnf5-automatic.timer; then",
					"  grep -Eq '^apply_updates = yes' " + conf + ` || { echo "/etc/dnf/automatic.conf has no apply_updates line; add apply_updates = yes under [commands]" >&2; exit 1; }`,
					"  systemctl enable --now dnf5-automatic.timer",
					"else",
					`  echo "no dnf-automatic timer is installed" >&2; exit 1`,
					"fi",
				}
			}
		}
	}

	// Tailscale login (opt-in). The key reaches the host inside the script
	// on stdin and is handed to tailscale as a 600 file, never in argv; a
	// trap removes the file however the script ends.
	if o.TailscaleKey.Set() {
		s = p.add(&Step{ID: StepTailscaleUp, Title: "Join the tailnet"})
		if f.TailscaleUp {
			s.Detail = "tailscale is already up"
		} else {
			s.Change, s.Detail = true, "tailscale up with the auth key from the environment (never in argv or output; 120 s timeout)"
			s.lines = []string{
				"systemctl enable --now tailscaled.service",
				"depper_kf=$(umask 077 && mktemp)",
				`trap 'rm -f "$depper_kf"' EXIT HUP INT TERM`,
				"printf '%s' " + shell.Quote(o.TailscaleKey.value) + " > \"$depper_kf\"",
				`if timeout 120 tailscale up --authkey="file:$depper_kf" >/dev/null; then rm -f "$depper_kf"; else rm -f "$depper_kf"; echo "tailscale up failed or timed out after 120 s" >&2; exit 1; fi`,
			}
		}
	}

	// SSH hardening (opt-in) runs in its own guarded phase.
	if o.HardenSSH {
		s = p.add(&Step{ID: StepHardenSSH, Title: "Harden sshd (" + SSHDropIn + ")"})
		switch {
		case f.Commands["sshd"] == "":
			return nil, fmt.Errorf("--harden-ssh: sshd was not found on the host")
		case !f.SSHDInclude:
			return nil, fmt.Errorf("--harden-ssh: %s does not Include sshd_config.d/*.conf, so a drop-in would be ignored. depper does not edit the main sshd_config; add `Include /etc/ssh/sshd_config.d/*.conf` at its top yourself, or drop --harden-ssh", sshdMain)
		case len(f.SSHDConflicts) > 0:
			return nil, fmt.Errorf("--harden-ssh: %s (in /etc/ssh) set(s) password, keyboard-interactive or root login to yes and sshd reads it before %s (it keeps the first value it reads), so sshd would ignore depper's settings; change or remove those lines first", strings.Join(f.SSHDConflicts, ", "), sshDropInName)
		case !f.SSHDTest:
			return nil, fmt.Errorf("--harden-ssh: the current sshd configuration does not validate (sshd -T: %s); fix it first", f.SSHDTestOutput)
		}
		// PermitRootLogin is only ever tightened: the drop-in sets it only
		// when sshd would otherwise use yes. The value is read without
		// depper's own drop-in, so a line an earlier build wrote is dropped
		// again when the rest of the configuration is stricter.
		p.rootBefore = f.SSHD["permitrootlogin"]
		if f.SSHDDropInSet {
			if f.SSHDRootBase == "" {
				return nil, fmt.Errorf("--harden-ssh: cannot determine PermitRootLogin without depper's drop-in (sshd -T on a copy of the configuration without %s failed); check `sshd -T` on the host", sshDropInName)
			}
			p.rootBefore = f.SSHDRootBase
		}
		p.hardenRoot = p.rootBefore == "yes"
		want := dropInContent(p.hardenRoot)
		if f.SSHDDropInSet && f.SSHDDropIn == want && sshdHardened(f.SSHD) {
			s.Detail = "drop-in present and in effect"
		} else {
			s.Change = true
			root := "PermitRootLogin unchanged (" + shellName(p.rootBefore) + ")"
			if p.hardenRoot {
				root = "PermitRootLogin prohibit-password"
			}
			s.Detail = "PasswordAuthentication no, KbdInteractiveAuthentication no, " + root + "; verified with sshd -t and new " + o.User + " and " + o.Admin + " logins; reverted automatically after " + RevertDelay.String() + " unless they succeed"
			if err := guardSupported(f, "--harden-ssh"); err != nil {
				return nil, err
			}
		}
		if len(f.SSHDMatch) > 0 {
			p.warn("sshd has Match blocks that set password, keyboard-interactive or root login (%s); --harden-ssh does not cover the connections they match", strings.Join(f.SSHDMatch, ", "))
		}
	}

	for _, st := range p.Steps {
		p.Packages = append(p.Packages, st.packages...)
	}
	sort.Strings(p.Packages)
	return p, nil
}

// addRecord appends the host record step. extra are steps that change
// something but are not (or no longer) marked Change in the plan, such as a
// host key pinned after its own confirmation.
func (p *Plan) addRecord(f *Facts, o *Options, extra []string) {
	steps := append([]string{}, extra...)
	for _, st := range p.Steps {
		if st.Change {
			steps = append(steps, st.ID)
		}
	}
	s := &Step{ID: StepRecord, Title: "Record the setup in " + RecordPath}
	p.add(s)
	if len(steps) > 0 {
		steps = append(steps, StepRecord)
	}
	rec := mergeRecord(f.Record, o, firewallPorts(o, f), steps, p.Packages)
	if f.Record != nil && sameContent(f.Record, &rec) {
		s.Detail = "up to date"
	} else {
		s.Change, s.Detail = true, "root-owned, mode 644, no secrets"
	}
}

// rootLoginLine is the PermitRootLogin directive --harden-ssh writes when
// root password login is allowed (PermitRootLogin yes).
const rootLoginLine = "PermitRootLogin prohibit-password"

// dropInContent is the sshd drop-in --harden-ssh writes. PermitRootLogin is
// included only to tighten "yes"; a stricter setting is never weakened.
func dropInContent(rootLogin bool) string {
	s := "# Managed by depper (depper host init --harden-ssh).\n# Remove this file and reload sshd to undo.\nPasswordAuthentication no\nKbdInteractiveAuthentication no\n"
	if rootLogin {
		s += rootLoginLine + "\n"
	}
	return s
}

// rootLoginRank orders PermitRootLogin values from the loosest (yes) to the
// strictest (no); unknown values rank -1.
func rootLoginRank(v string) int {
	switch v {
	case "yes":
		return 0
	case "prohibit-password", "without-password":
		return 1
	case "forced-commands-only":
		return 2
	case "no":
		return 3
	}
	return -1
}

// sshdHardened reports whether `sshd -T` shows the hardened settings:
// no password or keyboard-interactive login, and root password login off.
func sshdHardened(eff map[string]string) bool {
	if eff["passwordauthentication"] != "no" {
		return false
	}
	if v, ok := eff["kbdinteractiveauthentication"]; ok && v != "no" {
		return false
	}
	return rootLoginRank(eff["permitrootlogin"]) >= 1
}

// tailscaleInstall returns the official repository setup for the host
// (https://pkgs.tailscale.com/stable/).
func (r *Runner) tailscaleInstall(f *Facts) ([]string, string, error) {
	if f.Family == FamilyDebian {
		dist, codename := "debian", f.Codename
		if f.OSID == "ubuntu" || f.UbuntuCodename != "" {
			dist = "ubuntu"
			if f.UbuntuCodename != "" {
				codename = f.UbuntuCodename
			}
		}
		if !codenameRe.MatchString(codename) {
			return nil, "", fmt.Errorf("cannot determine the %s release codename (VERSION_CODENAME) needed for the Tailscale repository", dist)
		}
		base := "https://pkgs.tailscale.com/stable/" + dist + "/" + codename
		keyring := r.raw("/usr/share/keyrings/tailscale-archive-keyring.gpg")
		list := r.raw("/etc/apt/sources.list.d/tailscale.list")
		lines := append([]string{
			"mkdir -p " + r.p("/usr/share/keyrings") + " " + r.p("/etc/apt/sources.list.d"),
			"chmod 755 " + r.p("/usr/share/keyrings"),
			"curl -fsSL " + shell.Quote(base+".noarmor.gpg") + " -o " + shell.Quote(keyring+".depper-tmp"),
			"chmod 644 " + shell.Quote(keyring+".depper-tmp"),
			"mv -f " + shell.Quote(keyring+".depper-tmp") + " " + shell.Quote(keyring),
			"curl -fsSL " + shell.Quote(base+".tailscale-keyring.list") + " -o " + shell.Quote(list+".depper-tmp"),
			"chmod 644 " + shell.Quote(list+".depper-tmp"),
			"mv -f " + shell.Quote(list+".depper-tmp") + " " + shell.Quote(list),
		}, r.installLines(f.Family, []string{"tailscale"}, true)...)
		return lines, "add the Tailscale apt repository for " + dist + "/" + codename + " and install tailscale", nil
	}
	var url string
	major, _, _ := strings.Cut(f.VersionID, ".")
	switch {
	case f.OSID == "fedora":
		url = "https://pkgs.tailscale.com/stable/fedora/tailscale.repo"
	case f.OSID == "amzn" && regexp.MustCompile(`^20[2-9][0-9]$`).MatchString(major):
		url = "https://pkgs.tailscale.com/stable/amazon-linux/" + major + "/tailscale.repo"
	case regexp.MustCompile(`^[0-9]{1,3}$`).MatchString(major):
		dist := "rhel"
		if f.OSID == "centos" {
			dist = "centos"
		}
		url = "https://pkgs.tailscale.com/stable/" + dist + "/" + major + "/tailscale.repo"
	default:
		return nil, "", fmt.Errorf("cannot determine the release (VERSION_ID) needed for the Tailscale repository")
	}
	repo := r.raw("/etc/yum.repos.d/tailscale.repo")
	lines := append([]string{
		"mkdir -p " + r.p("/etc/yum.repos.d"),
		"curl -fsSL " + shell.Quote(url) + " -o " + shell.Quote(repo+".depper-tmp"),
		"chmod 644 " + shell.Quote(repo+".depper-tmp"),
		"mv -f " + shell.Quote(repo+".depper-tmp") + " " + shell.Quote(repo),
	}, r.installLines(f.Family, []string{"tailscale"}, false)...)
	return lines, "add " + url + " and install tailscale", nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func containsInt(list []int, n int) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}

func intsString(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// composeProvider picks the package that provides `<engine> compose`:
// podman-compose for podman; for docker the first of docker-compose-plugin,
// docker-compose-v2 and docker-compose (version 2 or later: the 1.x Python
// tool is not a docker CLI plugin) that is installed or available.
func composeProvider(engine string, f *Facts) (pkgReq, error) {
	candidates := []string{"podman-compose"}
	if engine == ContainerDocker {
		candidates = []string{"docker-compose-plugin", "docker-compose-v2", "docker-compose"}
	}
	usable := func(pkg, version string) bool {
		if version == "" {
			return false
		}
		return pkg != "docker-compose" || versionMajor(version) >= 2
	}
	for _, pkg := range candidates {
		if usable(pkg, f.Compose[pkg].Installed) {
			return pkgReq{Pkg: pkg, Installed: true}, nil
		}
	}
	for _, pkg := range candidates {
		if usable(pkg, f.Compose[pkg].Candidate) {
			return pkgReq{Pkg: pkg}, nil
		}
	}
	reason := fmt.Sprintf("depper.toml uses [container].compose, but no compose provider for %s is available from the package repositories of %s (looked for %s", engine, f.OSID, strings.Join(candidates, ", "))
	if engine == ContainerDocker {
		reason += "; docker-compose must be version 2 or later"
	}
	reason += ")"
	switch {
	case f.AptListsEmpty:
		reason += ". The apt package lists are empty: run `sudo apt-get update` on the host, then host init again"
	case engine == ContainerPodman && f.Family == FamilyFedora && f.OSID != "fedora":
		reason += ". On RHEL-compatible systems podman-compose is in EPEL: enable it (`dnf install epel-release`), then run host init again"
	case engine == ContainerDocker:
		reason += ". Install docker-ce and docker-compose-plugin from Docker's repository (https://docs.docker.com/engine/install/), or use podman"
	}
	return pkgReq{}, &ErrUnsupported{Reason: reason}
}

// shellName renders a login shell for messages.
func shellName(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}

// minSystemd is the oldest systemd whose systemd-run has --collect, which
// the automatic revert needs (a failed transient unit must not linger).
const minSystemd = 236

// guardSupported checks that the host can arm the automatic revert of a
// guarded change (flag names it in the message).
func guardSupported(f *Facts, flag string) error {
	switch {
	case f.Commands["systemd-run"] == "":
		return fmt.Errorf("%s: systemd-run was not found on the host; depper needs it to arm the automatic revert", flag)
	case f.SystemdVersion == 0:
		return fmt.Errorf("%s: cannot read the systemd version (systemctl --version); depper needs systemd %d or later to arm the automatic revert", flag, minSystemd)
	case f.SystemdVersion < minSystemd:
		return fmt.Errorf("%s: the host runs systemd %d, whose systemd-run has no --collect; depper needs systemd %d or later to arm the automatic revert safely", flag, f.SystemdVersion, minSystemd)
	}
	return nil
}

// firewallMarkers returns the prepare and revert lines of a firewall step.
// enabling: the step turns the firewall on (marker was-inactive; the
// revert runs disable). added: ports the step opens (marker added-ports,
// one per line; the revert runs remove for each, after the setup lines).
func firewallMarkers(enabling bool, added []int, disable, setup []string, remove func(pt string) []string) (prepare, revert []string) {
	if enabling {
		prepare = append(prepare, `: > "$d/`+markWasInactive+`"`)
		revert = append(revert, `if test -f "$d/`+markWasInactive+`"; then`)
		for _, l := range disable {
			revert = append(revert, "  "+l)
		}
		revert = append(revert, `  rm -f "$d/`+markWasInactive+`"`, "fi")
	}
	if len(added) > 0 && remove != nil {
		var ps []string
		for _, p := range added {
			ps = append(ps, strconv.Itoa(p))
		}
		prepare = append(prepare, "printf '%s\\n' "+strings.Join(ps, " ")+` > "$d/`+markAddedPorts+`"`)
		revert = append(revert, `if test -f "$d/`+markAddedPorts+`"; then`)
		for _, l := range setup {
			revert = append(revert, "  "+l)
		}
		revert = append(revert, `  while read -r depper_port; do`, `    case "$depper_port" in ""|*[!0-9]*) continue ;; esac`)
		for _, l := range remove(`"$depper_port"/tcp`) {
			revert = append(revert, "    "+l)
		}
		revert = append(revert, `  done < "$d/`+markAddedPorts+`"`, `  rm -f "$d/`+markAddedPorts+`"`, "fi")
	}
	return prepare, revert
}

// firewalldZoneLines set $depper_zone to the zone of iface (the interface
// the admin connection arrives on), falling back to the default zone with a
// warning. fc is firewall-cmd or firewall-offline-cmd. iface was validated
// by the facts script ([A-Za-z0-9_.:-]).
func firewalldZoneLines(fc, iface string) []string {
	q := shell.Quote(iface)
	return []string{
		`depper_zone=""`,
		`if test -n ` + q + `; then depper_zone=$(` + fc + ` --get-zone-of-interface=` + q + ` 2>/dev/null || true); fi`,
		`if test -z "$depper_zone"; then depper_zone=$(` + fc + ` --get-default-zone 2>/dev/null || true); echo "firewalld: the interface of the admin connection (` + iface + `) has no zone; using the default zone $depper_zone" >&2; fi`,
		`case "$depper_zone" in ""|*[!A-Za-z0-9_-]*) echo "cannot determine the firewalld zone" >&2; exit 1 ;; esac`,
	}
}
