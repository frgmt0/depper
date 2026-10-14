package host

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/config"
)

// pacmanRefreshRe matches a pacman command that refreshes the sync
// database (-Sy, -Syu, -Syy, ...).
// (-Sy, -Syu, -Syy, ...) as a command, not inside a message.
var pacmanRefreshRe = regexp.MustCompile(`(?m)^\s*(if\s+!\s*)?pacman\s+(-[a-zA-Z]*\s+)*-S[a-zA-Z]*y`)

// noPacmanRefresh fails when any script sent to the host, or any pacman
// call on it, refreshes the sync database.
func noPacmanRefresh(t *testing.T, h *fakeHost) {
	t.Helper()
	if h.has("pacman-refresh") {
		t.Errorf("pacman refreshed the sync database:\n%s", h.read("pacman-refresh"))
	}
	for i, s := range h.Exec.Scripts() {
		if pacmanRefreshRe.MatchString(s) {
			t.Errorf("script %d runs pacman -Sy:\n%s", i, s)
		}
	}
}

func statusChecks(t *testing.T, h *fakeHost) map[string]StatusCheck {
	t.Helper()
	cfg, err := config.LoadTarget(h.Repo, "", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	st, err := h.runner().Status(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]StatusCheck{}
	for _, c := range st.Checks {
		byName[c.Name] = c
	}
	return byName
}

func TestInitArchFullSetup(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, archLinux)
	o := h.options()
	o.Container = ContainerPodman
	o.Cloudflared = true
	o.HardenSSH = true
	o.Firewall = []int{80, 443}
	o.TailscaleKey = NewSecret(tsKey)
	res := mustInit(t, h, o)

	f := res.Facts
	if f.Family != FamilyArch || f.OSID != "arch" || !f.Pacman || f.PacmanSyncEmpty {
		t.Errorf("facts: family %q id %q pacman %v empty %v", f.Family, f.OSID, f.Pacman, f.PacmanSyncEmpty)
	}
	if f.PacmanSync["docker"] != "1:27.3.1-1" || !f.Packages["shadow"] || f.Packages["rsync"] {
		t.Errorf("pacman facts: sync %v packages %v", f.PacmanSync, f.Packages)
	}
	// Arch's own drop-ins (99-archlinux.conf, 20-systemd-userdb.conf with
	// AuthorizedKeysCommand) are read after 10-depper.conf and set nothing
	// depper hardens: no conflict.
	if len(f.SSHDConflicts) != 0 || f.SSHDUnit != "sshd" {
		t.Errorf("sshd conflicts %v unit %q", f.SSHDConflicts, f.SSHDUnit)
	}
	if !strings.Contains(h.Out.String(), "os-release ID arch, handled as the Arch Linux (pacman) family") {
		t.Errorf("output does not name the detected ID:\n%s", h.Out.String())
	}

	changes := h.changes()
	for _, want := range []string{
		"pacman -S --needed --noconfirm --noprogressbar rsync podman passt ufw\n",
		"pacman -S --needed --noconfirm --noprogressbar cloudflared\n",
		"pacman -S --needed --noconfirm --noprogressbar tailscale\n",
		"useradd --create-home --home-dir " + h.FS + "/home/deploy --shell /bin/bash --user-group deploy",
		"usermod -p * deploy",
		"loginctl enable-linger deploy",
		"usermod --add-subuids 100000-165535 deploy",
		"usermod --add-subgids 100000-165535 deploy",
		"systemctl enable --now tailscaled.service",
		"systemctl enable ufw.service",
		"systemctl reload sshd.service",
	} {
		if !strings.Contains(changes, want) {
			t.Errorf("missing change %q in:\n%s", want, changes)
		}
	}
	for _, bad := range []string{"apt-get", "dnf ", "pkg.cloudflare.com", "pkgs.tailscale.com", "unattended", "reload ssh.service"} {
		if strings.Contains(changes, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, changes)
		}
	}
	noPacmanRefresh(t, h)
	if h.read("fs/etc/subuid") != "deploy:100000:65536\n" || h.read("fs/etc/subgid") != "deploy:100000:65536\n" {
		t.Errorf("subuid %q subgid %q", h.read("fs/etc/subuid"), h.read("fs/etc/subgid"))
	}
	if strings.Contains(h.read("fs/etc/group"), "wheel:x:10:deploy") {
		t.Error("deploy user was put in wheel")
	}

	// ufw: rules, then enable, then the boot unit (Arch does not enable it).
	if got := h.read("ufw.log"); !strings.Contains(got, "ufw allow 22/tcp\nufw allow 80/tcp\nufw allow 443/tcp\nufw --force enable\n") {
		t.Errorf("ufw.log:\n%s", got)
	}
	if !h.has("units/ufw.service.enabled") || !h.has("ufw-active") {
		t.Error("ufw is not active and enabled at boot")
	}
	h.noGuardMarkers(firewallRevertUnit)

	// sshd: root login is already prohibit-password by default, so the
	// drop-in leaves it alone.
	if got := h.read("fs/etc/ssh/sshd_config.d/10-depper.conf"); got != dropInContent(false) {
		t.Errorf("drop-in = %q", got)
	}
	if got := h.read("events"); got != "sshd-t\nreload\n" {
		t.Errorf("events = %q", got)
	}

	rec := readRecord(t, h)
	if strings.Join(rec.Packages, " ") != "cloudflared passt podman rsync tailscale ufw" {
		t.Errorf("record packages = %v", rec.Packages)
	}
	if rec.Container != ContainerPodman || !rec.Cloudflared || !rec.Hardening.SSH || !rec.Hardening.Tailscale || intsString(rec.Hardening.Firewall) != "22,80,443" || rec.Hardening.AutoUpdates {
		t.Errorf("record = %+v", rec)
	}
	if res.Doctor == nil || res.Doctor.Failed() {
		t.Fatalf("doctor = %+v", res.Doctor)
	}
	checkScriptSyntax(t, h.Exec.Scripts())

	checks := statusChecks(t, h)
	for _, name := range []string{"record", "user", "linger", "systemd", "container engine", "cloudflared", "ssh hardening", "firewall", "tailscale"} {
		if checks[name].Status != StatusPass {
			t.Errorf("status %s = %+v", name, checks[name])
		}
	}
	if checks["auto updates"].Status != StatusInfo {
		t.Errorf("status auto updates = %+v", checks["auto updates"])
	}

	// A second run changes nothing.
	before := h.changes()
	h.Out = &syncBuffer{}
	r := h.runner()
	r.Out = h.Out
	res, err := r.Init(context.Background(), o)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, h.Out.String())
	}
	if ids := planChanges(res); len(ids) != 0 || h.changes() != before || len(res.Applied) != 0 {
		t.Errorf("second Arch run changes %v applied %v:\n%s", ids, res.Applied, strings.TrimPrefix(h.changes(), before))
	}
	if strings.Count(h.read("sshd-reloads"), "reload") != 1 {
		t.Error("sshd reloaded again")
	}
	noPacmanRefresh(t, h)
}

// A derivative is recognised by ID_LIKE, and docker comes from the docker
// package.
func TestInitArchDerivativeDocker(t *testing.T) {
	t.Parallel()
	d := archLinux
	d.osRelease = "NAME=\"Omarchy\"\nPRETTY_NAME=\"Omarchy\"\nID=omarchy\nID_LIKE=arch\n"
	h := newFakeHost(t, d)
	o := h.options()
	o.Container = ContainerDocker
	res := mustInit(t, h, o)
	if res.Facts.Family != FamilyArch || res.Facts.OSID != "omarchy" {
		t.Errorf("family %q id %q", res.Facts.Family, res.Facts.OSID)
	}
	if !strings.Contains(h.Out.String(), "os-release ID omarchy, handled as the Arch Linux (pacman) family") {
		t.Errorf("output:\n%s", h.Out.String())
	}
	for _, want := range []string{"pacman -S --needed --noconfirm --noprogressbar rsync docker\n", "usermod -aG docker deploy", "systemctl enable --now docker.service"} {
		if !strings.Contains(h.changes(), want) {
			t.Errorf("missing %q in:\n%s", want, h.changes())
		}
	}
	if !strings.Contains(h.read("fs/etc/group"), "docker:x:998:deploy") {
		t.Errorf("group = %s", h.read("fs/etc/group"))
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "equivalent to root") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	before := h.changes()
	res = mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
	if ids := planChanges(res); len(ids) != 0 || h.changes() != before {
		t.Errorf("second run changes %v", ids)
	}
	noPacmanRefresh(t, h)
}

// Compose providers come from the sync database: podman-compose, and
// docker-compose (version 2, which ships the docker CLI plugin).
func TestInitArchComposeProvider(t *testing.T) {
	t.Parallel()
	compose := strings.Replace(sampleConfig, "command = \"./run\" # start command\n", "", 1) + "\n[container]\nengine = \"%s\"\ncompose = \"compose.yaml\"\n"
	for _, tt := range []struct {
		name, engine, sync, want, wantErr string
	}{
		{"podman", "podman", archSync, "podman-compose", ""},
		{"docker", "docker", archSync, "docker-compose", ""},
		{"docker-compose missing from the sync database", "docker", strings.Replace(archSync, "docker-compose 2.29.7-1\n", "", 1), "", "sudo pacman -Syu"},
		{"only compose v1", "docker", strings.Replace(archSync, "docker-compose 2.29.7-1", "docker-compose 1.29.2-1", 1), "", "docker-compose must be version 2 or later"},
		{"podman-compose missing", "podman", strings.Replace(archSync, "podman-compose 1.2.0-1\n", "", 1), "", "never runs pacman -Sy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newFakeHost(t, archLinux)
			h.write("repo/depper.toml", strings.Replace(compose, "%s", tt.engine, 1), 0o644)
			h.write("compose.yaml", "", 0o644)
			h.write("pacman-sync", tt.sync, 0o644)
			o := h.options()
			o.Container = ContainerAuto
			res, err := h.runner().Init(context.Background(), o)
			if tt.wantErr != "" {
				var unsupported *ErrUnsupported
				if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v", err)
				}
				if h.changes() != "" {
					t.Errorf("refused host was changed:\n%s", h.changes())
				}
				noPacmanRefresh(t, h)
				return
			}
			if err != nil {
				t.Fatalf("%v\n%s", err, h.Out.String())
			}
			if !strings.Contains(h.changes(), "--noprogressbar rsync "+tt.engine) || !strings.Contains(h.changes(), " "+tt.want+"\n") {
				t.Errorf("changes:\n%s", h.changes())
			}
			if !contains(readRecord(t, h).Packages, tt.want) || res.Facts.Compose[tt.want].Candidate == "" {
				t.Errorf("record/facts lack %s: %+v", tt.want, res.Facts.Compose)
			}
			res = mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerAuto }))
			if ids := planChanges(res); len(ids) != 0 {
				t.Errorf("second run changes %v", ids)
			}
			noPacmanRefresh(t, h)
		})
	}
}

// Everything Arch refuses is refused before the host changes.
func TestInitArchRefusals(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, h *fakeHost, o *Options, wants ...string) {
		t.Helper()
		_, err := h.runner().Init(context.Background(), o)
		if err == nil {
			t.Fatalf("accepted:\n%s", h.Out.String())
		}
		for _, want := range wants {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
		if h.changes() != "" {
			t.Errorf("refused host was changed:\n%s", h.changes())
		}
		noPacmanRefresh(t, h)
	}
	t.Run("auto updates", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, archLinux)
		refused(t, h, h.optionsWith(func(o *Options) { o.AutoUpdates = true }), "--auto-updates is not supported on Arch Linux", "sudo pacman -Syu", "regularly")
	})
	t.Run("package missing from the sync database", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, archLinux)
		h.write("pacman-sync", strings.Replace(archSync, "rsync 3.3.0-2\n", "", 1), 0o644)
		refused(t, h, h.options(), "does not have rsync", "never runs `pacman -Sy`", "partial upgrade", "Run `sudo pacman -Syu` on the host yourself")
	})
	t.Run("several packages missing", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, archLinux)
		h.write("pacman-sync", "rsync 3.3.0-2\n", 0o644)
		refused(t, h, h.optionsWith(func(o *Options) { o.Container, o.Cloudflared = ContainerPodman, true }), "does not have podman, passt, cloudflared")
	})
	t.Run("empty sync database", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, archLinux)
		h.remove("fs/var/lib/pacman/sync")
		h.write("pacman-sync", "", 0o644)
		refused(t, h, h.options(), "sync database is empty", "sudo pacman -Syu")
	})
	t.Run("no pacman", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, archLinux)
		h.remove("remote-bin/pacman")
		refused(t, h, h.options(), "pacman was not found")
	})
	t.Run("hand-written nftables ruleset", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, archLinux)
		h.write("units/nftables.service.active", "", 0o644)
		h.write("pkg-bin/nft", "#!/bin/sh\nprintf 'table inet filter {\\n\\tchain input {\\n\\t\\ttype filter hook input priority filter; policy drop;\\n\\t}\\n}\\n'\n", 0o755)
		refused(t, h, h.optionsWith(func(o *Options) { o.Firewall = []int{80} }), "nftables.service (5 rules) is active", "does not replace a hand-written ruleset")
	})
	t.Run("unreadable nftables ruleset", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, archLinux)
		h.write("units/nftables.service.active", "", 0o644)
		h.write("pkg-bin/nft", "#!/bin/sh\necho 'Operation not permitted' >&2\nexit 1\n", 0o755)
		refused(t, h, h.optionsWith(func(o *Options) { o.Firewall = []int{80} }), "nftables.service (ruleset unreadable)")
	})
	t.Run("iptables ruleset", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, archLinux)
		h.write("units/iptables.service.active", "", 0o644)
		h.write("pkg-bin/iptables", "#!/bin/sh\nprintf '%s\\n' '-P INPUT DROP' '-P FORWARD DROP' '-P OUTPUT ACCEPT' '-A INPUT -p tcp --dport 22 -j ACCEPT'\n", 0o755)
		refused(t, h, h.optionsWith(func(o *Options) { o.Firewall = []int{80} }), "iptables.service (3 rules) is active", "stop and disable iptables.service")
	})
	t.Run("firewalld enabled but stopped", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, archLinux)
		h.write("packages", "coreutils\nca-certificates\ncurl\nshadow\nfirewalld\n", 0o644)
		h.write("pkg-bin/firewall-cmd", fakeFirewallCmd, 0o755)
		h.write("pkg-bin/firewall-offline-cmd", fakeFirewallCmd, 0o755)
		h.write("units/firewalld.service.enabled", "", 0o644)
		refused(t, h, h.optionsWith(func(o *Options) { o.Firewall = []int{80} }), "firewalld.service is enabled but not running")
	})
	t.Run("sudo needs a password", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, archLinux)
		h.write("admin-user", "jason\n", 0o644)
		h.write("admin-uid", "1000\n", 0o644)
		h.write("sudo-mode", "password\n", 0o644)
		refused(t, h, h.optionsWith(func(o *Options) { o.Admin = "jason" }),
			"jason needs a password for sudo", "sudo said: sudo: a password is required", "connect as root with an SSH key (root@127.0.0.1", "temporarily allow passwordless sudo for jason", "jason ALL=(ALL) NOPASSWD: ALL", "removed again after host init")
	})
}

// An empty ruleset of an active nftables.service is no reason to refuse:
// ufw is installed and used.
func TestInitArchEmptyNftablesRulesetUsesUFW(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, archLinux)
	h.write("units/nftables.service.active", "", 0o644)
	h.write("pkg-bin/nft", "#!/bin/sh\nexit 0\n", 0o755)
	res := mustInit(t, h, h.optionsWith(func(o *Options) { o.Firewall = []int{80} }))
	if len(res.Facts.RulesetServices) != 0 || !strings.Contains(h.changes(), "--noprogressbar rsync ufw\n") || !h.has("ufw-active") {
		t.Errorf("rulesets %v changes:\n%s", res.Facts.RulesetServices, h.changes())
	}
}

// A running firewalld is used as it is; ufw is not installed.
func TestInitArchUsesRunningFirewalld(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, archLinux)
	h.write("packages", "coreutils\nca-certificates\ncurl\nshadow\nfirewalld\n", 0o644)
	h.write("pkg-bin/firewall-cmd", fakeFirewallCmd, 0o755)
	h.write("pkg-bin/firewall-offline-cmd", fakeFirewallCmd, 0o755)
	h.write("units/firewalld.service.active", "", 0o644)
	h.write("units/firewalld.service.enabled", "", 0o644)
	o := h.optionsWith(func(o *Options) { o.Firewall = []int{443} })
	res := mustInit(t, h, o)
	s := res.Plan[0]
	for _, st := range res.Plan {
		if st.ID == StepFirewall {
			s = st
		}
	}
	if !strings.Contains(s.Title, "Firewall (firewalld)") || strings.Contains(h.changes(), "ufw") || h.has("pkg-bin/ufw") {
		t.Errorf("firewall step %+v, changes:\n%s", s, h.changes())
	}
	for _, f := range []string{"fwd/public.runtime", "fwd/public.perm"} {
		if got := h.read(f); got != "443/tcp\n" {
			t.Errorf("%s = %q", f, got)
		}
	}
	if ids := planChanges(mustInit(t, h, o)); len(ids) != 0 {
		t.Errorf("second run changes %v", ids)
	}
}

// ufw that is active but not enabled at boot (Arch does not enable
// ufw.service): host init enables the unit and adds nothing else.
func TestInitArchEnablesUFWAtBoot(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, archLinux)
	h.write("packages", "coreutils\nca-certificates\ncurl\nshadow\nufw\n", 0o644)
	h.write("pkg-bin/ufw", fakeUfw, 0o755)
	h.write("ufw-active", "", 0o644)
	h.write("ufw-rules", "22/tcp\n80/tcp\n", 0o644)
	h.write("fs/etc/ufw/ufw.conf", "ENABLED=yes\n", 0o644)
	o := h.optionsWith(func(o *Options) { o.Firewall = []int{80} })

	// Before: host status warns that ufw is off after a reboot.
	res := mustInit(t, h, h.options())
	if checks := statusChecks(t, h); checks["firewall"].Status != StatusWarn || !strings.Contains(checks["firewall"].Detail, "ufw.service is not enabled") {
		t.Errorf("status firewall = %+v", checks["firewall"])
	}

	res = mustInit(t, h, o)
	var fw *Step
	for _, s := range res.Plan {
		if s.ID == StepFirewall {
			fw = s
		}
	}
	if fw == nil || !fw.Change || !strings.HasPrefix(fw.Detail, "enable ufw.service at boot;") {
		t.Fatalf("firewall step = %+v", fw)
	}
	if strings.Contains(h.read("ufw.log"), "--force enable") || strings.Contains(h.read("ufw.log"), "ufw allow") || !h.has("units/ufw.service.enabled") {
		t.Errorf("ufw.log:\n%s", h.read("ufw.log"))
	}
	h.noGuardMarkers(firewallRevertUnit)
	if checks := statusChecks(t, h); checks["firewall"].Status != StatusPass {
		t.Errorf("status firewall = %+v", checks["firewall"])
	}
	if ids := planChanges(mustInit(t, h, o)); len(ids) != 0 {
		t.Errorf("second run changes %v", ids)
	}
}

// The revert timer undoes everything the Arch firewall step did, the boot
// unit included, when new logins fail.
func TestArchFirewallRevertDisablesTheBootUnit(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, archLinux)
	h.write("firewall-blocks-ssh", "", 0o644)
	r := h.runner()
	r.revertDelay, r.revertGrace = 15*time.Second, 5*time.Second
	_, err := r.Init(context.Background(), h.optionsWith(func(o *Options) { o.Firewall = []int{80} }))
	if err == nil || !strings.Contains(err.Error(), "depper-firewall-revert.timer on the host undid the change") {
		t.Fatalf("err = %v", err)
	}
	if h.has("ufw-active") || h.has("units/ufw.service.enabled") {
		t.Error("ufw is still active or enabled at boot after the revert")
	}
	if !strings.Contains(h.read("systemctl.log"), "systemctl disable --quiet ufw.service") {
		t.Errorf("systemctl.log:\n%s", h.read("systemctl.log"))
	}
	h.noGuardMarkers(firewallRevertUnit)
	noPacmanRefresh(t, h)
}

// The hardening guard on Arch: a drop-in read before 10-depper.conf that
// enables password login is a conflict; Arch's own drop-ins are not.
func TestHardenSSHOnArchConflicts(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, archLinux)
	h.write("fs/etc/ssh/sshd_config.d/05-password.conf", "PasswordAuthentication yes\n", 0o644)
	_, err := h.runner().Init(context.Background(), h.optionsWith(func(o *Options) { o.HardenSSH = true }))
	if err == nil || !strings.Contains(err.Error(), "--harden-ssh: sshd_config.d/05-password.conf (in /etc/ssh) set(s)") || strings.Contains(err.Error(), "archlinux") || strings.Contains(err.Error(), "userdb") {
		t.Fatalf("err = %v", err)
	}
	if h.changes() != "" {
		t.Errorf("changed:\n%s", h.changes())
	}
}

// With Arch's 99-archlinux.conf already turning keyboard-interactive login
// off, root login prohibit-password by default, and password login on,
// hardening only needs PasswordAuthentication; useradd that adds subids
// itself is handled.
func TestHardenSSHOnArchAndUseraddSubIDs(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, archLinux)
	h.write("useradd-subids", "", 0o644)
	res := mustInit(t, h, h.optionsWith(func(o *Options) { o.HardenSSH, o.Container = true, ContainerPodman }))
	if res.Facts.SSHD["kbdinteractiveauthentication"] != "no" || res.Facts.SSHD["passwordauthentication"] != "yes" || res.Facts.SSHD["permitrootlogin"] != "prohibit-password" {
		t.Errorf("sshd before = %v", res.Facts.SSHD)
	}
	if strings.Contains(h.changes(), "--add-subuids") || h.read("fs/etc/subuid") != "deploy:100000:65536\n" || h.read("fs/etc/subgid") != "deploy:100000:65536\n" {
		t.Errorf("subids: changes\n%s\nsubuid %q subgid %q", h.changes(), h.read("fs/etc/subuid"), h.read("fs/etc/subgid"))
	}
	if checks := statusChecks(t, h); checks["ssh hardening"].Status != StatusPass {
		t.Errorf("status = %+v", checks["ssh hardening"])
	}
}

// sshd started per connection by sshd.socket: there is no service to
// reload, new connections read the drop-in, and the revert (here of an
// interrupted run, by its timer) needs no reload either.
func TestHardenSSHWithSocketActivatedSSHD(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, archLinux)
	h.remove("units/sshd.service.active")
	h.write("units/sshd.socket.active", "", 0o644)
	h.write("sshd.socket.listen", "[::]:22 (Stream)\n", 0o644)
	r := h.runner()
	r.interruptAt = StepHardenSSH
	if _, err := r.Init(context.Background(), h.optionsWith(func(o *Options) { o.HardenSSH = true })); !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v\n%s", err, h.Out.String())
	}
	res := mustInit(t, h, h.optionsWith(func(o *Options) { o.HardenSSH, o.Firewall = true, []int{80} }))
	if !strings.Contains(h.Out.String(), "depper-ssh-revert reverts it now") || !strings.Contains(h.Out.String(), "sshd is started per connection by sshd.socket") {
		t.Errorf("output:\n%s", h.Out.String())
	}
	if h.has("sshd-reloads") || strings.Contains(h.read("systemctl.log"), "systemctl reload") {
		t.Errorf("sshd was reloaded:\n%s", h.read("systemctl.log"))
	}
	if !contains(res.Applied, StepHardenSSH) || h.read("fs/etc/ssh/sshd_config.d/10-depper.conf") != dropInContent(false) || res.Facts.SSHDUnit != "" || intsString(res.Facts.SSHSocketPorts) != "22" {
		t.Errorf("applied %v unit %q socket ports %v", res.Applied, res.Facts.SSHDUnit, res.Facts.SSHSocketPorts)
	}
	h.noGuardMarkers(sshRevertUnit)

	// With neither a service nor a socket running sshd, the revert cannot
	// confirm sshd sees the restored file: it keeps its marker and fails.
	h.remove("units/sshd.socket.active")
	h.installRevert(h.runner(), h.runner().sshGuard(""))
	h.write(sshGuardDir+"/"+markHadNone, "", 0o644)
	h.remove(sshGuardDir + "/" + markDisarmed)
	out, err := h.runRevert(sshRevertUnit, "timer")
	if err == nil || !strings.Contains(out, "cannot find the running sshd service") || !strings.Contains(out, "could not reload sshd") || h.has(sshGuardDir+"/"+markReverted) {
		t.Errorf("revert: %v\n%s", err, out)
	}
}

func TestPacmanFacts(t *testing.T) {
	t.Parallel()
	out := factsMarker + "\nos_ID=arch\npacman=1\npkg_rsync=1\npkg_podman=1\n" +
		"pac_q=coreutils=9.5-2 docker=1:27.3.1-1 bogus=1 \n" +
		"pac_si=rsync=3.3.0-2 docker-compose=2.29.7-1 podman-compose=1.2.0-1 \n" +
		"compose_podman-compose=1.0|1.0\n"
	f, _, err := parseFacts(out)
	if err != nil {
		t.Fatal(err)
	}
	// dpkg/rpm answers (pkg_) do not count on Arch.
	if f.Family != FamilyArch || f.Packages["rsync"] || f.Packages["podman"] || !f.Packages["coreutils"] || !f.Packages["docker"] || f.Packages["bogus"] {
		t.Errorf("packages = %v", f.Packages)
	}
	if f.PacmanSync["rsync"] != "3.3.0-2" || len(f.PacmanSync) != 3 {
		t.Errorf("sync = %v", f.PacmanSync)
	}
	if f.Compose["podman-compose"] != (PkgVersions{Candidate: "1.2.0-1"}) || f.Compose["docker-compose"] != (PkgVersions{Candidate: "2.29.7-1"}) {
		t.Errorf("compose = %+v", f.Compose)
	}
	// The awk programs on real pacman output: the first repository wins,
	// epochs are kept.
	si := "Repository      : core-testing\nName            : docker\nVersion         : 1:28.0.0-1\n\nRepository      : extra\nName            : docker\nVersion         : 1:27.3.1-1\n\n"
	si = "Repository      : extra\nName            : rsync\nVersion         : 3.3.0-2\nDescription     : A fast and versatile file copying tool\n\n" + si
	cmd := exec.Command("awk", pacmanInfoAwk)
	cmd.Stdin = strings.NewReader(si)
	got, err := cmd.Output()
	if err != nil || string(got) != "rsync=3.3.0-2 docker=1:28.0.0-1 " {
		t.Errorf("pacman -Si awk = %q (%v)", got, err)
	}
	cmd = exec.Command("awk", pacmanListAwk)
	cmd.Stdin = strings.NewReader("coreutils 9.5-2\ndocker 1:27.3.1-1\n")
	if got, err = cmd.Output(); err != nil || string(got) != "coreutils=9.5-2 docker=1:27.3.1-1 " {
		t.Errorf("pacman -Q awk = %q (%v)", got, err)
	}
}
