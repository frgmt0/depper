package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/config"
)

// tsKey is a fake Tailscale auth key, assembled at runtime so no literal in
// the repository looks like a real token to secret scanners.
var tsKey = strings.Join([]string{"tskey", "auth", "kTESTSECRET123", "ZYXWVUTSRQ"}, "-")

func mustInit(t *testing.T, h *fakeHost, o *Options) *InitResult {
	t.Helper()
	res, err := h.runner().Init(context.Background(), o)
	if err != nil {
		t.Fatalf("Init: %v\noutput:\n%s", err, h.Out.String())
	}
	return res
}

func readRecord(t *testing.T, h *fakeHost) Record {
	t.Helper()
	var rec Record
	if err := json.Unmarshal([]byte(h.read("fs/etc/depper/host.json")), &rec); err != nil {
		t.Fatalf("host.json: %v\n%s", err, h.read("fs/etc/depper/host.json"))
	}
	return rec
}

func planChanges(res *InitResult) []string {
	var ids []string
	for _, s := range res.Plan {
		if s.Change {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

func TestInitDebianFullSetup(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.Container = ContainerPodman
	o.Cloudflared = true
	o.HardenSSH = true
	o.Firewall = []int{80, 443}
	o.AutoUpdates = true
	o.TailscaleKey = NewSecret(tsKey)
	res := mustInit(t, h, o)

	// Host key pinned for 127.0.0.1, mode 600, in .depper/.
	kh := filepath.Join(h.Repo, ".depper", "known_hosts")
	info, err := os.Stat(kh)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("known_hosts mode = %v", info.Mode().Perm())
	}
	khData, _ := os.ReadFile(kh)
	if !strings.HasPrefix(string(khData), "127.0.0.1 ssh-ed25519 ") || strings.Count(string(khData), "\n") != 2 {
		t.Errorf("known_hosts = %q", khData)
	}
	if res.HostKeyStatus != HostKeyAdded || len(res.HostKeys) != 2 || !strings.HasPrefix(res.HostKeys[0].Fingerprint, "SHA256:") {
		t.Errorf("host keys = %+v %s", res.HostKeys, res.HostKeyStatus)
	}
	// Every ssh connection used the pinned file strictly.
	for _, argv := range h.Exec.Argv() {
		if strings.Contains(argv, "/bin/ssh ") && (!strings.Contains(argv, "UserKnownHostsFile="+kh) || !strings.Contains(argv, "StrictHostKeyChecking=yes") || !strings.Contains(argv, "BatchMode=yes")) {
			t.Errorf("ssh without the pinned known_hosts: %s", argv)
		}
	}

	changes := h.changes()
	for _, want := range []string{
		"apt-get -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -q update",
		"-q -y install rsync curl podman uidmap slirp4netns ufw unattended-upgrades",
		"curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o",
		"install cloudflared",
		"curl -fsSL https://pkgs.tailscale.com/stable/debian/bookworm.noarmor.gpg -o",
		"curl -fsSL https://pkgs.tailscale.com/stable/debian/bookworm.tailscale-keyring.list -o",
		"install tailscale",
		"useradd --create-home --home-dir " + filepath.Join(h.FS, "home/deploy") + " --shell /bin/bash --user-group deploy",
		"usermod -p * deploy",
		"loginctl enable-linger deploy",
		"systemctl start user@1000.service",
		"usermod --add-subuids 100000-165535 deploy",
		"usermod --add-subgids 100000-165535 deploy",
		"systemctl enable --now tailscaled.service",
	} {
		if !strings.Contains(changes, want) {
			t.Errorf("missing change %q in:\n%s", want, changes)
		}
	}
	if list := h.read("fs/etc/apt/sources.list.d/cloudflared.list"); list != "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main\n" {
		t.Errorf("cloudflared.list = %q", list)
	}
	if !h.has("pkg-bin/cloudflared") || !h.has("pkg-bin/podman") || !h.has("pkg-bin/rsync") {
		t.Error("packages were not installed")
	}

	// Deploy user: no password, key authorized with the right modes.
	if !strings.Contains(h.read("fs/etc/passwd"), "deploy:x:1000:1000::"+filepath.Join(h.FS, "home/deploy")+":/bin/bash") {
		t.Errorf("passwd = %s", h.read("fs/etc/passwd"))
	}
	if strings.Contains(h.read("fs/etc/group"), "sudo:x:27:deploy") || strings.Contains(h.read("fs/etc/group"), "wheel:x:10:deploy") {
		t.Error("deploy user was given sudo/wheel")
	}
	if strings.TrimSpace(h.read("passwords")) != "deploy *" {
		t.Errorf("passwords = %q", h.read("passwords"))
	}
	ak := filepath.Join(h.FS, "home/deploy/.ssh/authorized_keys")
	akData, _ := os.ReadFile(ak)
	if string(akData) != "ssh-ed25519 "+strings.TrimSpace(h.read("client-key-blob"))+" me@laptop\n" {
		t.Errorf("authorized_keys = %q", akData)
	}
	for path, mode := range map[string]os.FileMode{ak: 0o600, filepath.Dir(ak): 0o700} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s mode = %v (%v)", path, info.Mode().Perm(), err)
		}
	}
	if !strings.Contains(h.read("runuser.log"), "runuser -u deploy -- sh -c") {
		t.Errorf("authorized key not written as the deploy user:\n%s", h.read("runuser.log"))
	}
	if !h.has("fs/var/lib/systemd/linger/deploy") || !h.has("fs/run/user/1000/manager") {
		t.Error("linger or user manager missing")
	}
	if h.read("fs/etc/subuid") != "deploy:100000:65536\n" || h.read("fs/etc/subgid") != "deploy:100000:65536\n" {
		t.Errorf("subuid %q subgid %q", h.read("fs/etc/subuid"), h.read("fs/etc/subgid"))
	}

	// Firewall: SSH port included, enabled only after the rules.
	if got := h.read("ufw.log"); !strings.Contains(got, "ufw allow 22/tcp\nufw allow 80/tcp\nufw allow 443/tcp\nufw --force enable\n") {
		t.Errorf("ufw.log:\n%s", got)
	}
	if h.read("fs/etc/apt/apt.conf.d/20auto-upgrades") != "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n" {
		t.Errorf("20auto-upgrades = %q", h.read("fs/etc/apt/apt.conf.d/20auto-upgrades"))
	}

	// Tailscale: the key arrived through a 600 file and never in argv,
	// output, logs, the record, or JSON.
	if h.read("tailscale-key-received") != tsKey {
		t.Errorf("tailscale did not receive the key")
	}
	if mode := strings.TrimSpace(h.read("tailscale-key-mode")); mode != "-rw-------" {
		t.Errorf("auth key file mode = %q", mode)
	}
	entries, _ := os.ReadDir(filepath.Join(h.Root, "tmp"))
	if len(entries) != 0 {
		t.Errorf("leftover temp files on the host: %v", entries)
	}
	js, _ := json.Marshal(res)
	leaks := map[string]string{"output": h.Out.String(), "json": string(js), "argv": strings.Join(h.Exec.Argv(), "\n"), "host.json": h.read("fs/etc/depper/host.json")}
	for _, log := range []string{"changes.log", "ssh.log", "tailscale.log", "systemctl.log", "runuser.log", "sudo.log", "keyscan.log"} {
		leaks[log] = h.read(log)
	}
	for where, text := range leaks {
		if strings.Contains(text, tsKey) || strings.Contains(text, "kTESTSECRET") {
			t.Errorf("the Tailscale key leaked into %s", where)
		}
	}

	// sshd hardening: validated, then reloaded once; root key login kept.
	if got := h.read("fs/etc/ssh/sshd_config.d/10-depper.conf"); got != dropInContent(true) || !strings.Contains(got, "PermitRootLogin prohibit-password") {
		t.Errorf("drop-in = %q", got)
	}
	if got := h.read("events"); got != "sshd-t\nreload\n" {
		t.Errorf("events = %q, want sshd -t before one reload", got)
	}
	if !strings.Contains(h.read("systemctl.log"), "systemctl reload ssh.service") || strings.Contains(h.read("systemctl.log"), "restart ssh") {
		t.Errorf("sshd not reloaded (or restarted):\n%s", h.read("systemctl.log"))
	}

	// Record.
	rec := readRecord(t, h)
	if rec.DeployUser != "deploy" || rec.Container != "podman" || !rec.Cloudflared || rec.DepperVersion != "v0.2.0-test" || rec.CreatedAt != "2026-10-13T12:00:00Z" {
		t.Errorf("record = %+v", rec)
	}
	if !rec.Hardening.SSH || !rec.Hardening.AutoUpdates || !rec.Hardening.Tailscale || intsString(rec.Hardening.Firewall) != "22,80,443" {
		t.Errorf("hardening = %+v", rec.Hardening)
	}
	for _, step := range []string{StepPackages, StepUser, StepAuthorizedKey, StepLinger, StepUserManager, StepSubIDs, StepCloudflared, StepTailscalePkg, StepTailscaleUp, StepFirewall, StepAutoUpdates, StepHardenSSH} {
		if !contains(rec.Steps, step) {
			t.Errorf("record steps %v lack %s", rec.Steps, step)
		}
	}
	// The local steps are recorded too.
	for _, step := range []string{StepKnownHosts, StepConfig, StepRecord} {
		if !contains(rec.Steps, step) {
			t.Errorf("record steps %v lack %s", rec.Steps, step)
		}
	}
	if strings.Join(rec.Packages, " ") != "cloudflared curl podman rsync slirp4netns tailscale ufw uidmap unattended-upgrades" {
		t.Errorf("record packages = %v", rec.Packages)
	}
	if !strings.Contains(h.read("chown.log"), "chown root:root") {
		t.Error("host.json is not chowned to root")
	}
	if info, err := os.Stat(filepath.Join(h.FS, "etc/depper/host.json")); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("host.json mode: %v %v", info, err)
	}

	// Doctor passed as the deploy user and the config was edited in place.
	if res.Doctor == nil || res.Doctor.Failed() {
		t.Fatalf("doctor = %+v\n%s", res.Doctor, h.Out.String())
	}
	wantConfig := strings.Replace(sampleConfig, `host = "deploy@203.0.113.10"   # replace me`, `host = "127.0.0.1" # replace me`, 1)
	wantConfig = strings.Replace(wantConfig, "[deploy]\n", "[deploy]\nuser = \"deploy\"\nknown_hosts = \".depper/known_hosts\"\n", 1)
	if got, _ := os.ReadFile(filepath.Join(h.Repo, "depper.toml")); string(got) != wantConfig {
		t.Errorf("depper.toml =\n%s\nwant\n%s", got, wantConfig)
	}
	cfg, err := config.LoadTarget(h.Repo, "", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Deploy.Host != "127.0.0.1" || cfg.Deploy.User != "deploy" || cfg.Deploy.KnownHosts != kh || cfg.Deploy.Port != 0 {
		t.Errorf("resolved deploy = %+v", cfg.Deploy)
	}
	if !res.ConfigWritten || res.Next != "depper deploy" {
		t.Errorf("config written %v next %q", res.ConfigWritten, res.Next)
	}
	if !strings.Contains(h.Out.String(), "Commit .depper/known_hosts") || !strings.Contains(h.Out.String(), "Next: depper deploy") {
		t.Errorf("summary missing:\n%s", h.Out.String())
	}
	checkScriptSyntax(t, h.Exec.Scripts())

	// host status, as the deploy user, sees the same setup.
	st, err := h.runner().Status(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Failed() || st.Record == nil || st.User != "deploy" {
		t.Fatalf("status = %+v", st)
	}
	byName := map[string]StatusCheck{}
	for _, c := range st.Checks {
		byName[c.Name] = c
	}
	for _, name := range []string{"record", "user", "linger", "systemd", "ssh hardening", "firewall", "auto updates", "tailscale"} {
		if byName[name].Status != StatusPass {
			t.Errorf("status %s = %+v", name, byName[name])
		}
	}
}

func TestInitIsIdempotent(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, ubuntu2404)
	o := h.options()
	o.Container = ContainerPodman
	o.HardenSSH = true
	o.Firewall = []int{443}
	o.AutoUpdates = true
	mustInit(t, h, o)
	firstChanges, firstRecord := h.changes(), h.read("fs/etc/depper/host.json")
	firstConfig, _ := os.ReadFile(filepath.Join(h.Repo, "depper.toml"))
	firstKH, _ := os.ReadFile(o.KnownHosts)
	if !strings.Contains(firstChanges, "install rsync podman uidmap slirp4netns ufw unattended-upgrades") {
		t.Errorf("ubuntu install: %s", firstChanges)
	}

	o2 := h.options()
	o2.Container = ContainerPodman
	o2.HardenSSH = true
	o2.Firewall = []int{443}
	o2.AutoUpdates = true
	o2.Yes = false
	asked := 0
	r := h.runner()
	r.Confirm = func(string) (bool, error) { asked++; return true, nil }
	h.Out = &syncBuffer{}
	r.Out = h.Out
	res, err := r.Init(context.Background(), o2)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, h.Out.String())
	}
	if got := planChanges(res); len(got) != 0 {
		t.Errorf("second run plans changes: %v\n%s", got, h.Out.String())
	}
	if len(res.Applied) != 0 || asked != 0 || res.HostKeyStatus != HostKeyPinned {
		t.Errorf("applied %v, asked %d, host key %s", res.Applied, asked, res.HostKeyStatus)
	}
	if !strings.Contains(h.Out.String(), "Nothing to change") {
		t.Errorf("output:\n%s", h.Out.String())
	}
	if h.changes() != firstChanges || h.read("fs/etc/depper/host.json") != firstRecord {
		t.Errorf("second run changed the host:\n%s", strings.TrimPrefix(h.changes(), firstChanges))
	}
	if got, _ := os.ReadFile(filepath.Join(h.Repo, "depper.toml")); string(got) != string(firstConfig) {
		t.Errorf("second run changed depper.toml")
	}
	if got, _ := os.ReadFile(o.KnownHosts); string(got) != string(firstKH) {
		t.Errorf("second run changed known_hosts")
	}
	if strings.Count(h.read("sshd-reloads"), "reload") != 1 {
		t.Errorf("sshd reloaded again")
	}
	if res.Doctor == nil || res.Doctor.Failed() {
		t.Errorf("doctor on the second run: %+v", res.Doctor)
	}
}

func TestInitFedora(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, fedora40)
	o := h.options()
	o.Container = ContainerPodman
	o.HardenSSH = true
	o.Firewall = []int{8080}
	o.AutoUpdates = true
	o.Cloudflared = true
	o.TailscaleKey = NewSecret(tsKey)
	mustInit(t, h, o)
	changes := h.changes()
	for _, want := range []string{
		"dnf -y -q install rsync podman passt firewalld dnf-automatic",
		"curl -fsSL https://pkg.cloudflare.com/cloudflared.repo -o",
		"dnf -y -q install cloudflared",
		"curl -fsSL https://pkgs.tailscale.com/stable/fedora/tailscale.repo -o",
		"dnf -y -q install tailscale",
		"systemctl enable --now firewalld.service",
		"systemctl enable --now dnf-automatic-install.timer",
	} {
		if !strings.Contains(changes, want) {
			t.Errorf("missing %q in:\n%s", want, changes)
		}
	}
	if strings.Contains(changes, "apt-get") || strings.Contains(changes, "shadow-utils") {
		t.Errorf("unexpected change:\n%s", changes)
	}
	// Ports, including the SSH port, are added offline before firewalld
	// starts.
	if got := h.read("firewalld.log"); !strings.Contains(got, "firewall-offline-cmd --zone=public --add-port=22/tcp\nfirewall-offline-cmd --zone=public --add-port=8080/tcp") {
		t.Errorf("firewalld.log:\n%s", got)
	}
	if strings.Index(changes, "firewalld.service") < 0 {
		t.Error("firewalld not enabled")
	}
	if got := h.read("fs/etc/dnf/automatic.conf"); got != "[commands]\nupgrade_type = security\napply_updates = yes\n" {
		t.Errorf("automatic.conf = %q", got)
	}
	if !strings.Contains(h.read("systemctl.log"), "reload sshd.service") {
		t.Errorf("sshd.service not reloaded:\n%s", h.read("systemctl.log"))
	}
	rec := readRecord(t, h)
	if strings.Join(rec.Packages, " ") != "cloudflared dnf-automatic firewalld passt podman rsync tailscale" {
		t.Errorf("packages = %v", rec.Packages)
	}
	checkScriptSyntax(t, h.Exec.Scripts())

	// A second run is a no-op on Fedora too.
	h.Out = &syncBuffer{}
	before := h.changes()
	r := h.runner()
	r.Out = h.Out
	res, err := r.Init(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if got := planChanges(res); len(got) != 0 || h.changes() != before {
		t.Errorf("second Fedora run changes %v:\n%s", got, h.Out.String())
	}
}

func TestInitRockyRefusesDockerButUsesRHELRepos(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, rocky9)
	o := h.options()
	o.Container = ContainerDocker
	_, err := h.runner().Init(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "docker-ce") {
		t.Fatalf("err = %v", err)
	}
	if h.changes() != "" {
		t.Errorf("changes after refusal:\n%s", h.changes())
	}
	o = h.options()
	o.Container = ContainerPodman
	o.TailscaleKey = NewSecret(tsKey)
	mustInit(t, h, o)
	for _, want := range []string{"install rsync podman slirp4netns", "https://pkgs.tailscale.com/stable/rhel/9/tailscale.repo"} {
		if !strings.Contains(h.changes(), want) {
			t.Errorf("missing %q in\n%s", want, h.changes())
		}
	}
}

func TestInitDockerOnDebianWarns(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.Container = ContainerDocker
	res := mustInit(t, h, o)
	if !strings.Contains(h.changes(), "install rsync curl docker.io") || !strings.Contains(h.changes(), "usermod -aG docker deploy") || !strings.Contains(h.changes(), "systemctl enable --now docker.service") {
		t.Errorf("changes:\n%s", h.changes())
	}
	if !strings.Contains(h.read("fs/etc/group"), "docker:x:998:deploy") {
		t.Errorf("group = %s", h.read("fs/etc/group"))
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "equivalent to root") || !strings.Contains(h.Out.String(), "WARNING: membership in the docker group is equivalent to root") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

// Without --container, host init installs the engine the resolved
// configuration uses: [container].engine, docker when unset, nothing
// without [container].
func TestInitContainerDefaultsToConfiguredEngine(t *testing.T) {
	t.Parallel()
	noCommand := strings.Replace(sampleConfig, "command = \"./run\" # start command\n", "", 1)
	for _, tt := range []struct {
		name, config, want, pkg string
	}{
		{"no container table", sampleConfig, ContainerNone, ""},
		{"engine unset", noCommand + "\n[container]\nimage = \"nginx:1.27\"\n", ContainerDocker, "docker.io"},
		{"engine podman", noCommand + "\n[container]\nengine = \"podman\"\nimage = \"nginx:1.27\"\n", ContainerPodman, "podman"},
		{"engine docker", noCommand + "\n[container]\nengine = \"docker\"\nimage = \"nginx:1.27\"\n", ContainerDocker, "docker.io"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newFakeHost(t, debian12)
			h.write("repo/depper.toml", tt.config, 0o644)
			o := h.options()
			o.Container = ContainerAuto
			res := mustInit(t, h, o)
			if rec := readRecord(t, h); rec.Container != tt.want {
				t.Errorf("recorded container = %q, want %q", rec.Container, tt.want)
			}
			installed := h.read("packages")
			for _, pkg := range []string{"podman", "docker.io"} {
				if has := strings.Contains("\n"+installed, "\n"+pkg+"\n"); has != (pkg == tt.pkg) {
					t.Errorf("package %s installed = %v (packages:\n%s)", pkg, has, installed)
				}
			}
			for _, w := range res.Warnings {
				if strings.Contains(w, "resolves [container].engine") || strings.Contains(w, "pass --container") {
					t.Errorf("obsolete warning: %s", w)
				}
			}
		})
	}
}

// An explicit --container none on a container project installs nothing and
// says so.
func TestInitContainerNoneWarnsForContainerProject(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	cfg := strings.Replace(sampleConfig, "command = \"./run\" # start command\n", "", 1) + "\n[container]\nengine = \"podman\"\nimage = \"nginx:1.27\"\n"
	h.write("repo/depper.toml", cfg, 0o644)
	o := h.options()
	o.Container = ContainerNone
	res, err := h.runner().Init(context.Background(), o)
	// The final doctor run rightly fails: the configured engine is missing.
	var doctorFailed *DoctorFailedError
	if !errors.As(err, &doctorFailed) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(h.read("packages"), "podman") {
		t.Error("podman installed with --container none")
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "--container none installs no container engine") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestInitRefusesUnsupportedOS(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.write("fs/etc/os-release", "NAME=\"Arch Linux\"\nID=arch\n", 0o644)
	_, err := h.runner().Init(context.Background(), h.options())
	var unsupported *ErrUnsupported
	if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), `"arch"`) {
		t.Fatalf("err = %v", err)
	}
	if h.changes() != "" || h.has("fs/etc/depper/host.json") {
		t.Errorf("unsupported host was changed:\n%s", h.changes())
	}
	if got, _ := os.ReadFile(filepath.Join(h.Repo, "depper.toml")); string(got) != sampleConfig {
		t.Error("depper.toml changed")
	}
}

func TestInitRefusesWithoutSystemd(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.remove("fs/run/systemd/system")
	_, err := h.runner().Init(context.Background(), h.options())
	if err == nil || !strings.Contains(err.Error(), "not running systemd") {
		t.Fatalf("err = %v", err)
	}
	if h.changes() != "" {
		t.Errorf("changes:\n%s", h.changes())
	}
}

func TestInitDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.DryRun, o.Yes = true, false
	o.Container = ContainerPodman
	o.HardenSSH = true
	o.Firewall = []int{80}
	o.TailscaleKey = NewSecret(tsKey)
	r := h.runner()
	r.Confirm = func(string) (bool, error) { t.Fatal("dry run asked for confirmation"); return false, nil }
	res, err := r.Init(context.Background(), o)
	if err != nil {
		t.Fatalf("%v\n%s", err, h.Out.String())
	}
	if h.changes() != "" || h.has("ufw.log") || h.has("fs/etc/depper/host.json") || h.has("sshd.log") && strings.Contains(h.read("sshd.log"), "-t") {
		t.Errorf("dry run changed the host:\n%s", h.changes())
	}
	if _, err := os.Stat(filepath.Join(h.Repo, ".depper")); !os.IsNotExist(err) {
		t.Errorf("dry run created .depper/: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(h.Repo, "depper.toml")); string(got) != sampleConfig {
		t.Error("dry run changed depper.toml")
	}
	out := h.Out.String()
	for _, want := range []string{res.HostKeys[0].Fingerprint, "1. Pin the host key", "Install packages: install rsync curl podman uidmap slirp4netns ufw", "[deploy] host: \"deploy@203.0.113.10\" -> \"127.0.0.1\"", "Dry run: nothing was changed."} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if res.HostKeyStatus != HostKeyUnconfirmed || res.Doctor != nil || len(res.Applied) != 0 {
		t.Errorf("result = %+v", res)
	}
	if strings.Contains(out, tsKey) {
		t.Error("dry run printed the Tailscale key")
	}
	checkScriptSyntax(t, h.Exec.Scripts())
}

func TestInitHostKeyConfirmation(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.Yes = false
	var questions []string
	r := h.runner()
	r.Confirm = func(q string) (bool, error) { questions = append(questions, q); return false, nil }
	_, err := r.Init(context.Background(), o)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("err = %v", err)
	}
	if len(questions) != 1 || !strings.Contains(questions[0], "Trust these host keys") {
		t.Errorf("questions = %q", questions)
	}
	if _, err := os.Stat(o.KnownHosts); !os.IsNotExist(err) {
		t.Error("declined host key was written")
	}
	if h.has("ssh.log") {
		t.Errorf("connected before the host key was confirmed:\n%s", h.read("ssh.log"))
	}
	if !strings.Contains(h.Out.String(), "SHA256:") {
		t.Errorf("fingerprints not shown:\n%s", h.Out.String())
	}

	// Accept the key, decline the plan: the key is pinned, the host untouched.
	questions = nil
	r.Confirm = func(q string) (bool, error) {
		questions = append(questions, q)
		return strings.Contains(q, "Trust"), nil
	}
	_, err = r.Init(context.Background(), o)
	if !errors.Is(err, ErrDeclined) || len(questions) != 2 || !strings.Contains(questions[1], "Apply these changes") {
		t.Fatalf("err = %v questions %q", err, questions)
	}
	if _, err := os.Stat(o.KnownHosts); err != nil {
		t.Error("accepted host key was not written")
	}
	if h.changes() != "" {
		t.Errorf("declined plan changed the host:\n%s", h.changes())
	}

	// Now pinned: no host key question, only the plan question.
	questions = nil
	r.Confirm = func(q string) (bool, error) { questions = append(questions, q); return true, nil }
	if _, err := r.Init(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 || !strings.Contains(questions[0], "Apply") {
		t.Errorf("questions = %q", questions)
	}

	// Without --yes and without a way to ask, refuse.
	r.Confirm = nil
	h2 := newFakeHost(t, debian12)
	o2 := h2.options()
	o2.Yes = false
	r2 := h2.runner()
	if _, err := r2.Init(context.Background(), o2); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("err = %v", err)
	}
}

func TestInitRefusesChangedHostKey(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	other := "127.0.0.1 ssh-ed25519 " + fakeKeyBlob("ssh-ed25519", 32) + "\n"
	h.write("repo/.depper/known_hosts", other, 0o600)
	_, err := h.runner().Init(context.Background(), o)
	var mismatch *HostKeyMismatchError
	if !errors.As(err, &mismatch) || !strings.Contains(err.Error(), "HOST KEY MISMATCH") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(o.KnownHosts); string(got) != other {
		t.Error("known_hosts was modified")
	}
	if h.has("ssh.log") {
		t.Error("connected despite a host key mismatch")
	}
}

func TestInitNonStandardPortKnownHosts(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.Port = 2222
	o.Firewall = []int{80}
	mustInit(t, h, o)
	kh, _ := os.ReadFile(o.KnownHosts)
	if !strings.HasPrefix(string(kh), "[127.0.0.1]:2222 ssh-ed25519 ") {
		t.Errorf("known_hosts = %q", kh)
	}
	if !strings.Contains(h.read("keyscan.log"), "-p 2222 127.0.0.1") {
		t.Errorf("keyscan = %q", h.read("keyscan.log"))
	}
	if !strings.Contains(h.read("ufw.log"), "ufw allow 2222/tcp") {
		t.Errorf("SSH port not allowed:\n%s", h.read("ufw.log"))
	}
	cfg, err := config.LoadTarget(h.Repo, "", func(string) (string, bool) { return "", false })
	if err != nil || cfg.Deploy.Port != 2222 {
		t.Fatalf("cfg %+v err %v", cfg, err)
	}
}

func TestInitSudoAdmin(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.write("admin-user", "admin\n", 0o644)
	h.write("admin-uid", "1001\n", 0o644)
	o := h.options()
	o.Admin = "admin"

	h.write("sudo-mode", "password\n", 0o644)
	_, err := h.runner().Init(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "needs a password for sudo") || !strings.Contains(err.Error(), "NOPASSWD") {
		t.Fatalf("err = %v", err)
	}
	if h.changes() != "" {
		t.Errorf("changes:\n%s", h.changes())
	}

	h.write("sudo-mode", "nopasswd\n", 0o644)
	res := mustInit(t, h, o)
	if res.Facts.Privilege != PrivSudo || !strings.Contains(h.read("sudo.log"), "sudo -n sh -s") {
		t.Errorf("privilege %s sudo.log %q", res.Facts.Privilege, h.read("sudo.log"))
	}
	if !strings.Contains(h.changes(), "useradd") {
		t.Error("nothing applied through sudo")
	}
}

func TestInitWithTargetAndExplicitKey(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	priv := h.write("local-home/.ssh/deploy_key", "PRIVATE", 0o600)
	pub := h.write("local-home/.ssh/deploy_key.pub", h.read("local-home/.ssh/id_ed25519.pub"), 0o644)
	h.write("repo/depper.toml", sampleConfig+"\n[targets.staging]\nhost = \"old.example.com\" # staging box\n", 0o644)
	o := h.options()
	o.Target = "staging"
	o.KeyFile, o.KeyExplicit = pub, true
	res := mustInit(t, h, o)
	got, _ := os.ReadFile(filepath.Join(h.Repo, "depper.toml"))
	want := sampleConfig + "\n[targets.staging]\nuser = \"deploy\"\nidentity_file = " + tomlString(displayPath(priv, "")) + "\nknown_hosts = \".depper/known_hosts\"\nhost = \"127.0.0.1\" # staging box\n"
	if string(got) != want {
		t.Errorf("depper.toml =\n%s\nwant\n%s", got, want)
	}
	if res.Next != "depper deploy --target staging" {
		t.Errorf("next = %q", res.Next)
	}
	// The deploy user verification and doctor used the explicit key.
	found := false
	for _, argv := range h.Exec.Argv() {
		if strings.Contains(argv, "-i "+priv) && strings.Contains(argv, "-l deploy") {
			found = true
		}
	}
	if !found {
		t.Errorf("deploy connection did not use %s", priv)
	}
}

func TestInitNoWriteConfig(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.WriteConfig = false
	res := mustInit(t, h, o)
	if got, _ := os.ReadFile(filepath.Join(h.Repo, "depper.toml")); string(got) != sampleConfig || res.ConfigWritten {
		t.Error("--no-write-config wrote the config")
	}
	if res.Doctor == nil || res.Doctor.Failed() {
		t.Errorf("doctor = %+v", res.Doctor)
	}
}

func TestInitRequiresConfigFile(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.remove("repo/depper.toml")
	_, err := h.runner().Init(context.Background(), h.options())
	if err == nil || !strings.Contains(err.Error(), "depper init") {
		t.Fatalf("err = %v", err)
	}
	if h.has("keyscan.log") {
		t.Error("contacted the host without a config file")
	}
}

func TestHardenSSHRefusesWithoutInclude(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.write("fs/etc/ssh/sshd_config", "PermitRootLogin yes\nPasswordAuthentication yes\n", 0o644)
	o := h.options()
	o.HardenSSH = true
	_, err := h.runner().Init(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "does not Include sshd_config.d") {
		t.Fatalf("err = %v", err)
	}
	if h.changes() != "" || h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") {
		t.Error("refused hardening changed the host")
	}
	if h.read("fs/etc/ssh/sshd_config") != "PermitRootLogin yes\nPasswordAuthentication yes\n" {
		t.Error("main sshd_config edited")
	}
}

func TestHardenSSHRefusesEarlierConflictingDropIn(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, ubuntu2404)
	h.write("fs/etc/ssh/sshd_config.d/10-cloud.conf", "PasswordAuthentication yes\n", 0o644)
	h.write("fs/etc/ssh/sshd_config.d/90-late.conf", "PasswordAuthentication yes\n", 0o644)
	o := h.options()
	o.HardenSSH = true
	_, err := h.runner().Init(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "10-cloud.conf") || strings.Contains(err.Error(), "90-late.conf") {
		t.Fatalf("err = %v", err)
	}
	if h.changes() != "" {
		t.Errorf("changes:\n%s", h.changes())
	}
}

// Ubuntu cloud images ship 50-cloud-init.conf with PasswordAuthentication
// yes. depper's 10-depper.conf is read first, so it wins and is no conflict.
func TestHardenSSHOverridesCloudInitDropIn(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, ubuntu2404)
	h.write("fs/etc/ssh/sshd_config.d/50-cloud-init.conf", "PasswordAuthentication yes\n", 0o644)
	o := h.options()
	o.HardenSSH = true
	res := mustInit(t, h, o)
	if !contains(res.Applied, StepHardenSSH) {
		t.Fatalf("applied = %v", res.Applied)
	}
	if got := h.read("fs/etc/ssh/sshd_config.d/10-depper.conf"); got != dropInContent(true) {
		t.Errorf("drop-in = %q", got)
	}
	if !h.has("fs/etc/ssh/sshd_config.d/50-cloud-init.conf") {
		t.Error("the cloud-init drop-in was removed")
	}
	// A second run sees the hardening in effect.
	o2 := h.options()
	o2.HardenSSH = true
	res = mustInit(t, h, o2)
	if ids := planChanges(res); len(ids) != 0 {
		t.Errorf("second run changes %v", ids)
	}
}

// Lines of the main sshd_config before its Include are read before every
// drop-in; lines after it are not.
func TestHardenSSHRefusesMainConfigBeforeInclude(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, ubuntu2404)
	h.write("fs/etc/ssh/sshd_config", "KbdInteractiveAuthentication yes\nInclude /etc/ssh/sshd_config.d/*.conf\nPasswordAuthentication yes\n", 0o644)
	o := h.options()
	o.HardenSSH = true
	_, err := h.runner().Init(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "sshd_config (in /etc/ssh)") || !strings.Contains(err.Error(), "10-depper.conf") {
		t.Fatalf("err = %v", err)
	}
	if h.changes() != "" || h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") {
		t.Errorf("refused hardening changed the host:\n%s", h.changes())
	}
}

func TestHardenSSHRemovesDropInWhenSSHDTestFails(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.write("sshd-t-fail", "", 0o644)
	o := h.options()
	o.HardenSSH = true
	_, err := h.runner().Init(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "sshd -t rejected") {
		t.Fatalf("err = %v", err)
	}
	if h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") || h.has("sshd-reloads") {
		t.Error("drop-in left in place or sshd reloaded")
	}
}

func TestHardenSSHVerifiesDeployLoginBeforeReload(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.HardenSSH = true
	// The user and key steps run, but the deploy login fails.
	h.write("deny-deploy-login", "", 0o644)
	_, err := h.runner().Init(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "cannot log in with its key") || !strings.Contains(err.Error(), "did not reload") {
		t.Fatalf("err = %v", err)
	}
	if h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") || h.has("sshd-reloads") {
		t.Error("drop-in kept or sshd reloaded although the deploy login failed")
	}
	// The steps that did complete are recorded, without the hardening.
	rec := readRecord(t, h)
	if rec.Hardening.SSH || contains(rec.Steps, StepHardenSSH) || !contains(rec.Steps, StepUser) || !contains(rec.Packages, "rsync") {
		t.Errorf("record after failed hardening = %+v", rec)
	}
}

func TestHardenSSHRollsBackWhenLoginFailsAfterReload(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.HardenSSH = true
	h.write("deny-deploy-after-reload", "", 0o644)
	_, err := h.runner().Init(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "a new login as deploy failed") || !strings.Contains(err.Error(), "after reloading sshd") || !strings.Contains(err.Error(), "rolled back the change and disarmed depper-ssh-revert.timer") {
		t.Fatalf("err = %v", err)
	}
	if h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") {
		t.Error("drop-in not removed after the failed verification")
	}
	if got := strings.Count(h.read("sshd-reloads"), "reload"); got != 2 {
		t.Errorf("reloads = %d, want 2 (apply + rollback)", got)
	}
	if h.has("units/depper-ssh-revert.timer.active") {
		t.Error("the revert timer is still armed after the immediate rollback")
	}
}

// The admin cannot log in after the reload, so the immediate rollback over
// a new admin connection fails too: the timer armed on the host before the
// drop-in was written undoes it on its own, and host init says so.
func TestHardenSSHRevertTimerFiresWhenAdminLoginFails(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.HardenSSH = true
	h.write("deny-admin-after-reload", "", 0o644)
	r := h.runner()
	r.revertDelay, r.revertGrace = 5*time.Second, 3*time.Second
	_, err := r.Init(context.Background(), o)
	if err == nil {
		t.Fatal("hardening succeeded although the admin cannot log in")
	}
	for _, want := range []string{
		"--harden-ssh: a new login as root failed",
		"after reloading sshd",
		"the immediate rollback failed",
		"so depper-ssh-revert.timer on the host undid the change at",
		"a new root login works again",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") {
		t.Error("the revert timer did not remove the drop-in")
	}
	if got := h.read("timer-events"); !strings.Contains(got, "armed depper-ssh-revert\n") || !strings.Contains(got, "fired depper-ssh-revert\n") || !strings.Contains(got, "done depper-ssh-revert\n") {
		t.Errorf("timer events = %q", got)
	}
	// The timer reloaded sshd after removing the drop-in.
	if got := strings.Count(h.read("sshd-reloads"), "reload"); got != 2 {
		t.Errorf("reloads = %d, want 2 (apply + timer)", got)
	}
	if !strings.Contains(h.Out.String(), "waiting for depper-ssh-revert.timer") {
		t.Errorf("output does not explain the wait:\n%s", h.Out.String())
	}
	if rec := readRecord(t, h); rec.Hardening.SSH || contains(rec.Steps, StepHardenSSH) {
		t.Errorf("record claims hardening: %+v", rec)
	}
	// The timer is armed before the drop-in is written, so an interrupted
	// run is undone as well.
	logs := h.read("systemd-run.log")
	if !strings.Contains(logs, "--unit=depper-ssh-revert --on-active=5s /bin/sh -c") || !strings.Contains(logs, "10-depper.conf") {
		t.Errorf("systemd-run.log = %q", logs)
	}
}
