package host

import (
	"fmt"
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
}

// Plan is what host init will do.
type Plan struct {
	Steps    []*Step  `json:"steps"`
	Warnings []string `json:"warnings"`
	// Packages are the packages depper installs (for the record).
	Packages []string `json:"packages"`
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
const DockerGroupWarning = "membership in the docker group is equivalent to root on this host: anyone who can log in as the deploy user (or run code through a deployed service) can take over the machine. Prefer --container podman (rootless) unless you need docker."

// sshPorts returns the TCP ports to allow in the firewall: the requested
// ones plus the SSH port in use.
func firewallPorts(o *Options) []int {
	return unionInts(o.Firewall, []int{o.Port})
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
	reqs := []pkgReq{{"rsync", "rsync"}, {"curl", "curl"}, {"ca-certificates", ""}, {"coreutils", "sha256sum"}}
	switch o.Container {
	case ContainerPodman:
		reqs = append(reqs, pkgReq{"podman", "podman"})
		if f.Family == FamilyDebian {
			reqs = append(reqs, pkgReq{"uidmap", "newuidmap"}, pkgReq{"slirp4netns", ""})
		} else if fedoraProper {
			reqs = append(reqs, pkgReq{"shadow-utils", "newuidmap"}, pkgReq{"passt", ""})
		} else {
			reqs = append(reqs, pkgReq{"shadow-utils", "newuidmap"}, pkgReq{"slirp4netns", ""})
		}
	case ContainerDocker:
		switch {
		case f.Family == FamilyDebian:
			reqs = append(reqs, pkgReq{"docker.io", "docker"})
		case fedoraProper:
			reqs = append(reqs, pkgReq{"moby-engine", "docker"})
		default:
			return nil, &ErrUnsupported{Reason: fmt.Sprintf("%s has no distribution docker package; install docker-ce from Docker's official repository (https://docs.docker.com/engine/install/) yourself, or use --container podman", f.OSID)}
		}
	}
	if o.Firewall != nil {
		if f.Family == FamilyDebian {
			reqs = append(reqs, pkgReq{"ufw", "ufw"})
		} else {
			reqs = append(reqs, pkgReq{"firewalld", "firewall-cmd"})
		}
	}
	if o.AutoUpdates {
		if f.Family == FamilyDebian {
			reqs = append(reqs, pkgReq{"unattended-upgrades", ""})
		} else {
			reqs = append(reqs, pkgReq{"dnf-automatic", ""})
		}
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
		if f.has(pkgReq{"cloudflared", "cloudflared"}) {
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
		if f.has(pkgReq{"tailscale", "tailscale"}) {
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
	if f.UserExists && f.KeyPresent {
		s.Detail = key.Type + " " + key.Fingerprint() + " already authorized"
	} else {
		s.Change = true
		s.Detail = "append " + key.Type + " " + key.Fingerprint() + " to ~" + o.User + "/.ssh/authorized_keys (700/600, owned by " + o.User + ")"
		const install = `set -eu
umask 077
home=$1; line=$2; match=$3
d="$home/.ssh"; ak="$d/authorized_keys"
if test -L "$d" || test -L "$ak"; then echo "refusing: $d or $ak is a symbolic link" >&2; exit 1; fi
mkdir -p "$d"
chmod 700 "$d"
touch "$ak"
chmod 600 "$ak"
if ! awk -v k="$match" '{ for (i = 1; i < NF; i++) if ($i " " $(i+1) == k) f = 1 } END { exit !f }' "$ak"; then
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

	// Firewall (opt-in).
	if o.Firewall != nil {
		want := firewallPorts(o)
		portList := intsString(want)
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
				s.Detail = strings.Join(parts, ", then ")
			}
		} else {
			s = p.add(&Step{ID: StepFirewall, Title: "Firewall (firewalld): allow TCP " + portList})
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
				if f.FirewalldActive {
					for _, port := range add {
						s.lines = append(s.lines, "firewall-cmd --permanent --add-port="+strconv.Itoa(port)+"/tcp")
					}
					s.lines = append(s.lines, "firewall-cmd --reload")
					s.Detail = "allow " + intsString(add) + " and reload firewalld"
				} else {
					for _, port := range add {
						s.lines = append(s.lines, "firewall-offline-cmd --add-port="+strconv.Itoa(port)+"/tcp")
					}
					s.lines = append(s.lines, "systemctl enable --now firewalld.service")
					s.Detail = "allow " + intsString(add) + ", then enable firewalld"
					if len(add) == 0 {
						s.Detail = "enable firewalld"
					}
				}
			}
		}
	}

	// Automatic security updates (opt-in).
	if o.AutoUpdates {
		s = p.add(&Step{ID: StepAutoUpdates, Title: "Automatic security updates"})
		if f.Family == FamilyDebian {
			if f.has(pkgReq{"unattended-upgrades", ""}) && f.AutoUpdatesConf {
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
			ok := f.has(pkgReq{"dnf-automatic", ""}) && f.AutoUpdatesConf &&
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
	// on stdin and is handed to tailscale as a 600 file, never in argv.
	if o.TailscaleKey.Set() {
		s = p.add(&Step{ID: StepTailscaleUp, Title: "Join the tailnet"})
		if f.TailscaleUp {
			s.Detail = "tailscale is already up"
		} else {
			s.Change, s.Detail = true, "tailscale up with the auth key from the environment (never in argv or output)"
			s.lines = []string{
				"systemctl enable --now tailscaled.service",
				"depper_kf=$(umask 077 && mktemp)",
				"printf '%s' " + shell.Quote(o.TailscaleKey.value) + " > \"$depper_kf\"",
				`if tailscale up --authkey="file:$depper_kf" >/dev/null; then rm -f "$depper_kf"; else rm -f "$depper_kf"; echo "tailscale up failed" >&2; exit 1; fi`,
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
			return nil, fmt.Errorf("--harden-ssh: %s in %s set(s) password, keyboard-interactive or root login to yes and sort(s) before 50-depper.conf, so sshd would ignore depper's settings; change or remove those lines first", strings.Join(f.SSHDConflicts, ", "), sshdDropInD)
		case !f.SSHDTest:
			return nil, fmt.Errorf("--harden-ssh: the current sshd configuration does not validate (sshd -T: %s); fix it first", f.SSHDTestOutput)
		}
		if f.SSHDDropInSet && f.SSHDDropIn == dropInContent && sshdHardened(f.SSHD, o.Admin) {
			s.Detail = "drop-in present and in effect"
		} else {
			s.Change = true
			s.Detail = "PasswordAuthentication no, KbdInteractiveAuthentication no, PermitRootLogin prohibit-password; verified with sshd -t and a new " + o.User + " login before and after reload"
		}
	}

	// The host record.
	steps := []string{}
	for _, st := range p.Steps {
		if st.Change {
			steps = append(steps, st.ID)
		}
		p.Packages = append(p.Packages, st.packages...)
	}
	rec := mergeRecord(f.Record, o, steps, p.Packages)
	s = p.add(&Step{ID: StepRecord, Title: "Record the setup in " + RecordPath})
	if f.Record != nil && sameContent(f.Record, &rec) {
		s.Detail = "up to date"
	} else {
		s.Change, s.Detail = true, "root-owned, mode 644, no secrets"
	}
	sort.Strings(p.Packages)
	return p, nil
}

// dropInContent is the sshd drop-in --harden-ssh writes. Root key login
// stays allowed because the admin may be root.
const dropInContent = "# Managed by depper (depper host init --harden-ssh).\n# Remove this file and reload sshd to undo.\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin prohibit-password\n"

// sshdHardened reports whether `sshd -T` shows the hardened settings.
func sshdHardened(eff map[string]string, admin string) bool {
	if eff["passwordauthentication"] != "no" {
		return false
	}
	if v, ok := eff["kbdinteractiveauthentication"]; ok && v != "no" {
		return false
	}
	switch eff["permitrootlogin"] {
	case "prohibit-password", "without-password":
		return true
	case "no":
		return admin != "root"
	}
	return false
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
