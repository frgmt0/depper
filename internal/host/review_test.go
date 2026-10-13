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

// addUser creates an existing account on the fake host with the given
// shadow password field and login shell, and authorizes lines in its
// authorized_keys.
func (h *fakeHost) addUser(name, password, shell string, akLines ...string) {
	h.t.Helper()
	home := filepath.Join(h.FS, "home", name)
	appendFile(filepath.Join(h.FS, "etc/passwd"), name+":x:1000:1000::"+home+":"+shell)
	appendFile(filepath.Join(h.FS, "etc/group"), name+":x:1000:")
	appendFile(filepath.Join(h.FS, "etc/shadow"), name+":"+password+":19000:0:99999:7:::")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if len(akLines) > 0 {
		if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(strings.Join(akLines, "\n")+"\n"), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *fakeHost) clientKey() string {
	return "ssh-ed25519 " + strings.TrimSpace(h.read("client-key-blob"))
}

func shadowField(h *fakeHost, user string) string {
	for _, line := range strings.Split(h.read("fs/etc/shadow"), "\n") {
		if f := strings.Split(line, ":"); len(f) > 1 && f[0] == user {
			return f[1]
		}
	}
	return ""
}

// Item 1 (firewall half): enabling the firewall locks every new login out;
// the immediate rollback cannot connect either, so the timer armed before
// the change disables the firewall again.
func TestFirewallRevertTimerFiresWhenLoginsFail(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.write("firewall-blocks-ssh", "", 0o644)
	o := h.options()
	o.Firewall = []int{80}
	r := h.runner()
	r.revertDelay, r.revertGrace = 5*time.Second, 3*time.Second
	_, err := r.Init(context.Background(), o)
	if err == nil {
		t.Fatal("firewall accepted although logins fail")
	}
	for _, want := range []string{"--firewall: a new login as deploy failed", "after enabling the firewall", "the immediate rollback failed", "depper-firewall-revert.timer on the host undid the change", "a new root login works again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if h.has("ufw-active") {
		t.Error("ufw is still active after the revert")
	}
	if got := h.read("ufw.log"); !strings.Contains(got, "ufw --force enable\n") || !strings.Contains(got, "ufw --force disable\n") {
		t.Errorf("ufw.log = %q", got)
	}
	if rec := readRecord(t, h); len(rec.Hardening.Firewall) != 0 || contains(rec.Steps, StepFirewall) {
		t.Errorf("record claims the firewall: %+v", rec)
	}
}

// A successful guarded change disarms its timer, and the timer never fires.
func TestGuardedChangesDisarmTheirTimers(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.Firewall = []int{80}
	o.HardenSSH = true
	mustInit(t, h, o)
	ev := h.read("timer-events")
	if !strings.Contains(ev, "armed depper-firewall-revert") || !strings.Contains(ev, "armed depper-ssh-revert") || strings.Contains(ev, "fired") {
		t.Errorf("timer events = %q", ev)
	}
	if h.has("units/depper-firewall-revert.timer.active") || h.has("units/depper-ssh-revert.timer.active") {
		t.Error("a revert timer is still armed")
	}
	if h.has("fs/etc/ssh/sshd_config.d/10-depper.conf.depper-prev") {
		t.Error("the drop-in backup was left behind")
	}
	if !strings.Contains(h.Out.String(), "verified; disarmed depper-ssh-revert.timer") {
		t.Errorf("output:\n%s", h.Out.String())
	}
}

// Item 2: the firewall allows every port sshd listens on, even when host
// init connects through another one (port forward / NAT), and says so.
func TestFirewallAllowsSSHDPorts(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.write("fs/etc/ssh/sshd_config", "Include /etc/ssh/sshd_config.d/*.conf\nPort 2222\nPermitRootLogin yes\nPasswordAuthentication yes\n", 0o644)
	o := h.options()
	o.Firewall = []int{80}
	res := mustInit(t, h, o)
	log := h.read("ufw.log")
	for _, p := range []string{"22", "80", "2222"} {
		if !strings.Contains(log, "ufw allow "+p+"/tcp\n") {
			t.Errorf("port %s not allowed:\n%s", p, log)
		}
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "sshd listens on 2222") && strings.Contains(w, "port forward or NAT") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v", res.Warnings)
	}
	if rec := readRecord(t, h); intsString(rec.Hardening.Firewall) != "22,80,2222" {
		t.Errorf("recorded firewall = %v", rec.Hardening.Firewall)
	}
}

// Item 3: --harden-ssh never weakens PermitRootLogin.
func TestHardenSSHKeepsStricterPermitRootLogin(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"no", "forced-commands-only", "prohibit-password"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			h := newFakeHost(t, debian12)
			h.write("fs/etc/ssh/sshd_config", "Include /etc/ssh/sshd_config.d/*.conf\nPermitRootLogin "+value+"\nPasswordAuthentication yes\n", 0o644)
			o := h.options()
			o.HardenSSH = true
			res := mustInit(t, h, o)
			got := h.read("fs/etc/ssh/sshd_config.d/10-depper.conf")
			if got != dropInContent(false) || strings.Contains(got, "PermitRootLogin") {
				t.Errorf("drop-in = %q", got)
			}
			if !contains(res.Applied, StepHardenSSH) {
				t.Errorf("applied = %v", res.Applied)
			}
			// sshd -T still shows the stricter value, and a re-run changes
			// nothing.
			res = mustInit(t, h, h.optionsWith(func(o *Options) { o.HardenSSH = true }))
			if ids := planChanges(res); len(ids) != 0 {
				t.Errorf("second run changes %v", ids)
			}
		})
	}
	// "yes" is tightened to prohibit-password.
	h := newFakeHost(t, debian12)
	o := h.options()
	o.HardenSSH = true
	mustInit(t, h, o)
	if got := h.read("fs/etc/ssh/sshd_config.d/10-depper.conf"); !strings.Contains(got, "\nPermitRootLogin prohibit-password\n") {
		t.Errorf("drop-in = %q", got)
	}
}

func TestRootLoginRank(t *testing.T) {
	order := []string{"yes", "prohibit-password", "forced-commands-only", "no"}
	for i := 1; i < len(order); i++ {
		if rootLoginRank(order[i]) <= rootLoginRank(order[i-1]) {
			t.Errorf("%s is not stricter than %s", order[i], order[i-1])
		}
	}
	if rootLoginRank("without-password") != rootLoginRank("prohibit-password") || rootLoginRank("bogus") != -1 {
		t.Error("aliases or unknown values ranked wrongly")
	}
	if sshdHardened(map[string]string{"passwordauthentication": "no", "kbdinteractiveauthentication": "no", "permitrootlogin": "yes"}) {
		t.Error("PermitRootLogin yes counted as hardened")
	}
	if !sshdHardened(map[string]string{"passwordauthentication": "no", "kbdinteractiveauthentication": "no", "permitrootlogin": "no"}) {
		t.Error("PermitRootLogin no not counted as hardened")
	}
}

func (h *fakeHost) optionsWith(f func(*Options)) *Options {
	o := h.options()
	f(o)
	return o
}

// Item 4: the edited table resolves to exactly the intended connection.
func TestPlanConfigWritesAFullyResolvedConnection(t *testing.T) {
	const head = "[service]\nname = \"app\"\ncommand = \"x\"\n\n"
	home, _ := filepath.EvalSymlinks(t.TempDir())
	pub := filepath.Join(home, "id_test.pub")
	priv := filepath.Join(home, "id_test")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAA x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("an existing port is reset to 22", func(t *testing.T) {
		repo := writeRepoConfig(t, head+"[deploy]\nhost = \"a.example.com\"\nport = 2200\n")
		o := &Options{Host: "1.2.3.4", User: "deploy", Port: 22, KeyFile: pub, KnownHosts: filepath.Join(repo, ".depper/known_hosts")}
		edit, cfg, err := planConfig(repo, "", o, "", noEnv)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(edit.Edited, "port = 22\n") || cfg.Deploy.Port != 22 {
			t.Errorf("edited:\n%s", edit.Edited)
		}
	})
	t.Run("port 22 is not added when absent", func(t *testing.T) {
		repo := writeRepoConfig(t, head+"[deploy]\nhost = \"a.example.com\"\n")
		o := &Options{Host: "1.2.3.4", User: "deploy", Port: 22, KeyFile: pub, KnownHosts: filepath.Join(repo, ".depper/known_hosts")}
		edit, _, err := planConfig(repo, "", o, "", noEnv)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(edit.Edited, "port") {
			t.Errorf("edited:\n%s", edit.Edited)
		}
	})
	t.Run("an inherited port is overridden in the target", func(t *testing.T) {
		repo := writeRepoConfig(t, head+"[deploy]\nhost = \"a.example.com\"\nport = 2200\n")
		o := &Options{Host: "1.2.3.4", User: "deploy", Port: 22, KeyFile: pub, KnownHosts: filepath.Join(repo, ".depper/known_hosts")}
		edit, cfg, err := planConfig(repo, "prod", o, "", noEnv)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(edit.Edited, "[targets.prod]\nhost = \"1.2.3.4\"\nuser = \"deploy\"\nport = 22\n") || cfg.Deploy.Port != 22 {
			t.Errorf("edited:\n%s", edit.Edited)
		}
	})
	t.Run("an existing identity_file is replaced by the authorized key's private key", func(t *testing.T) {
		repo := writeRepoConfig(t, head+"[deploy]\nhost = \"a.example.com\"\nidentity_file = \"/elsewhere/old_key\"\n")
		o := &Options{Host: "1.2.3.4", User: "deploy", Port: 22, KeyFile: pub, KnownHosts: filepath.Join(repo, ".depper/known_hosts")}
		_, cfg, err := planConfig(repo, "", o, priv, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Deploy.IdentityFile != priv {
			t.Errorf("identity = %q", cfg.Deploy.IdentityFile)
		}
	})
	t.Run("an existing identity_file without a local private key is refused", func(t *testing.T) {
		repo := writeRepoConfig(t, head+"[deploy]\nhost = \"a.example.com\"\nidentity_file = \"/elsewhere/old_key\"\n")
		o := &Options{Host: "1.2.3.4", User: "deploy", Port: 22, KeyFile: pub, KnownHosts: filepath.Join(repo, ".depper/known_hosts")}
		_, _, err := planConfig(repo, "", o, "", noEnv)
		if err == nil || !strings.Contains(err.Error(), "sets identity_file") || !strings.Contains(err.Error(), "--key") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an inherited identity_file is overridden in the target", func(t *testing.T) {
		repo := writeRepoConfig(t, head+"[deploy]\nhost = \"a.example.com\"\nidentity_file = \"/elsewhere/old_key\"\n")
		o := &Options{Host: "1.2.3.4", User: "deploy", Port: 22, KeyFile: pub, KnownHosts: filepath.Join(repo, ".depper/known_hosts")}
		_, cfg, err := planConfig(repo, "prod", o, priv, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Deploy.IdentityFile != priv {
			t.Errorf("identity = %q", cfg.Deploy.IdentityFile)
		}
		if _, _, err := planConfig(repo, "prod", o, "", noEnv); err == nil || !strings.Contains(err.Error(), "--key") {
			t.Errorf("without a private key: err = %v", err)
		}
	})
	t.Run("an environment override that changes the connection aborts", func(t *testing.T) {
		repo := writeRepoConfig(t, head+"[deploy]\nhost = \"a.example.com\"\n")
		o := &Options{Host: "1.2.3.4", User: "deploy", Port: 22, KeyFile: pub, KnownHosts: filepath.Join(repo, ".depper/known_hosts")}
		env := func(k string) (string, bool) {
			if k == config.EnvKnownHostsKey {
				return "/somewhere/else", true
			}
			return "", false
		}
		_, _, err := planConfig(repo, "", o, "", env)
		if err == nil || !strings.Contains(err.Error(), "known_hosts") || !strings.Contains(err.Error(), "was not changed") {
			t.Fatalf("err = %v", err)
		}
	})
}

// Item 5: the reviewer's repro. ”' inside basic strings is not a
// multi-line delimiter, so the target table is found and nothing else
// changes.
func TestEditConfigIgnoresDelimitersInsideStrings(t *testing.T) {
	const doc = "[service]\nname = \"app\"\ncommand = \"x\"\n\n[deploy]\nhost=\"old.example\"\nsetup=[\"echo '''\"]\n\n[targets.prod]\nsetup=[\"echo '''\"]\nhost=\"prod.example\"\n"
	repo := writeRepoConfig(t, doc)
	o := &Options{Host: "1.2.3.4", User: "deploy", Port: 22, KnownHosts: filepath.Join(repo, ".depper/known_hosts")}
	edit, cfg, err := planConfig(repo, "prod", o, "", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Deploy.Host != "1.2.3.4" {
		t.Errorf("prod host = %q", cfg.Deploy.Host)
	}
	if !strings.Contains(edit.Edited, "[deploy]\nhost=\"old.example\"\n") || !strings.Contains(edit.Edited, "host = \"1.2.3.4\"\n") || strings.Contains(edit.Edited, "prod.example") {
		t.Errorf("edited:\n%s", edit.Edited)
	}
	before, _ := config.Parse(repo, filepath.Join(repo, "depper.toml"), doc)
	after, _ := config.Parse(repo, filepath.Join(repo, "depper.toml"), edit.Edited)
	b, _ := before.ResolveWithEnv("", noEnv)
	a, _ := after.ResolveWithEnv("", noEnv)
	if d := connectionOf(a).diff(connectionOf(b)); len(d) > 0 {
		t.Errorf("[deploy] changed: %v", d)
	}

	// Editing [deploy] leaves the target alone.
	edit, cfg, err = planConfig(repo, "", o, "", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Deploy.Host != "1.2.3.4" || !strings.Contains(edit.Edited, "[targets.prod]\nsetup=[\"echo '''\"]\nhost=\"prod.example\"\n") {
		t.Errorf("edited:\n%s", edit.Edited)
	}
}

func TestTOMLScanner(t *testing.T) {
	for _, tt := range []struct {
		lines []string
		top   []bool // topLevel before each line
	}{
		{[]string{`a = "x '''"`, `b = 1`}, []bool{true, true}},
		{[]string{`a = 'x """'`, `b = 1`}, []bool{true, true}},
		{[]string{`a = """`, `[deploy]`, `"""`, `b = 1`}, []bool{true, false, false, true}},
		{[]string{`a = """x\"""`, `"""`, `b = 1`}, []bool{true, false, true}},
		{[]string{`a = """x""""`, `b = 1`}, []bool{true, true}},
		{[]string{`a = [`, `  ["x"]`, `]`, `b = 1`}, []bool{true, false, false, true}},
		{[]string{`a = 1 # """`, `b = 1`}, []bool{true, true}},
		{[]string{`a = { x = "]" }`, `b = 1`}, []bool{true, true}},
	} {
		var sc tomlScanner
		for i, l := range tt.lines {
			if sc.topLevel() != tt.top[i] {
				t.Errorf("%q: line %d topLevel = %v", tt.lines, i, sc.topLevel())
			}
			sc.scan(l, headerRe.MatchString(l) && sc.topLevel())
		}
	}
	// A header-looking element of a multi-line array is not a header.
	out, _, err := editConfigText("[deploy]\nsetup = [\n[\"x\"]\n]\nhost = \"a\"\n", "deploy", []configValue{{key: "host", value: `"b"`}})
	if err != nil || out != "[deploy]\nsetup = [\n[\"x\"]\n]\nhost = \"b\"\n" {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

// Item 4 (docs agent): a failing doctor leaves depper.toml alone and prints
// the edit that would have been made.
func TestInitDoesNotWriteConfigWhenDoctorFails(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	cfg := strings.Replace(sampleConfig, "command = \"./run\" # start command\n", "", 1) + "\n[container]\nengine = \"podman\"\nimage = \"nginx:1.27\"\n"
	h.write("repo/depper.toml", cfg, 0o644)
	o := h.options()
	o.Container = ContainerNone
	res, err := h.runner().Init(context.Background(), o)
	var doctorFailed *DoctorFailedError
	if !errors.As(err, &doctorFailed) {
		t.Fatalf("err = %v", err)
	}
	if got := h.read("repo/depper.toml"); got != cfg || res.ConfigWritten || contains(res.Applied, StepConfig) {
		t.Errorf("depper.toml written after a failed doctor:\n%s", got)
	}
	out := h.Out.String()
	if !strings.Contains(out, "was not changed. The edit host init would have made to [deploy]:") || !strings.Contains(out, `[deploy] host: "deploy@203.0.113.10" -> "127.0.0.1"`) {
		t.Errorf("output:\n%s", out)
	}
	// The record is still written, without the configuration step.
	if rec := readRecord(t, h); contains(rec.Steps, StepConfig) || !contains(rec.Steps, StepUser) {
		t.Errorf("record = %+v", rec)
	}
}

// Item 6: compose projects get a compose provider.
func TestInitInstallsComposeProvider(t *testing.T) {
	t.Parallel()
	compose := strings.Replace(sampleConfig, "command = \"./run\" # start command\n", "", 1) + "\n[container]\nengine = \"%s\"\ncompose = \"compose.yaml\"\n"
	for _, tt := range []struct {
		name      string
		d         distro
		engine    string
		available string
		want      string
		wantErr   string
	}{
		{"podman on debian", debian12, "podman", "podman-compose 1.0.6-1\n", "podman-compose", ""},
		{"docker on ubuntu", ubuntu2404, "docker", "docker-compose-v2 2.27.1\ndocker-compose 1.29.2-6\n", "docker-compose-v2", ""},
		{"docker on debian prefers the plugin", debian12, "docker", "docker-compose-plugin 2.29.1-1\ndocker-compose 2.26.1-4\n", "docker-compose-plugin", ""},
		{"docker on debian with only compose v1", debian12, "docker", "docker-compose 1.29.2-3\n", "", "docker-compose must be version 2 or later"},
		{"podman on fedora", fedora40, "podman", "podman-compose 1.2.0\n", "podman-compose", ""},
		{"podman on rocky without EPEL", rocky9, "podman", "", "", "EPEL"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newFakeHost(t, tt.d)
			h.write("repo/depper.toml", strings.Replace(compose, "%s", tt.engine, 1), 0o644)
			h.write("compose.yaml", "", 0o644)
			h.write("available", tt.available, 0o644)
			h.write("fs/var/lib/apt/lists/deb_Packages", "", 0o644)
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
				return
			}
			if err != nil {
				t.Fatalf("%v\n%s", err, h.Out.String())
			}
			if !strings.Contains("\n"+h.read("packages"), "\n"+tt.want+"\n") {
				t.Errorf("%s not installed:\n%s", tt.want, h.read("packages"))
			}
			if !contains(readRecord(t, h).Packages, tt.want) || !res.Facts.Compose[tt.want].isSet() {
				t.Errorf("record/facts lack %s", tt.want)
			}
			// A second run sees the provider installed.
			res = mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerAuto }))
			if ids := planChanges(res); len(ids) != 0 {
				t.Errorf("second run changes %v", ids)
			}
		})
	}
	// Empty apt lists: say how to fix it.
	h := newFakeHost(t, debian12)
	h.write("repo/depper.toml", strings.Replace(compose, "%s", "podman", 1), 0o644)
	o := h.options()
	o.Container = ContainerAuto
	if _, err := h.runner().Init(context.Background(), o); err == nil || !strings.Contains(err.Error(), "apt-get update") {
		t.Errorf("err = %v", err)
	}
}

func (v PkgVersions) isSet() bool { return v.Installed != "" || v.Candidate != "" }

// Item 7: Match blocks are reported, not refused.
func TestHardenSSHWarnsAboutMatchBlocks(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.write("fs/etc/ssh/sshd_config", "Include /etc/ssh/sshd_config.d/*.conf\nPermitRootLogin yes\nPasswordAuthentication yes\nMatch User backup\n  PasswordAuthentication yes\n", 0o644)
	h.write("fs/etc/ssh/sshd_config.d/60-sftp.conf", "Match Group sftp\n    KbdInteractiveAuthentication yes\n", 0o644)
	o := h.options()
	o.HardenSSH = true
	res := mustInit(t, h, o)
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Match blocks") && strings.Contains(w, "/etc/ssh/sshd_config:5 PasswordAuthentication") && strings.Contains(w, "/etc/ssh/sshd_config.d/60-sftp.conf:2 KbdInteractiveAuthentication") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v", res.Warnings)
	}
	cfg, err := config.LoadTarget(h.Repo, "", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	st, err := h.runner().Status(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var match *StatusCheck
	for i := range st.Checks {
		if st.Checks[i].Name == "ssh match blocks" {
			match = &st.Checks[i]
		}
	}
	if match == nil || match.Status != StatusWarn || !strings.Contains(match.Detail, "60-sftp.conf:2") {
		t.Errorf("status = %+v", st.Checks)
	}
}

// Item 8: firewalld ports go into the runtime and permanent configuration
// of the zone of the admin connection's interface.
func TestFirewalldUsesTheZoneOfTheSSHInterface(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, rocky9)
	h.write("packages", "coreutils\nca-certificates\nshadow-utils\ncurl\nfirewalld\n", 0o644)
	h.write("pkg-bin/firewall-cmd", fakeFirewallCmd, 0o755)
	h.write("pkg-bin/firewall-offline-cmd", fakeFirewallCmd, 0o755)
	h.write("units/firewalld.service.active", "", 0o644)
	h.write("firewalld-iface-zones", "eth0 internal\n", 0o644)
	o := h.options()
	o.Firewall = []int{443}
	res := mustInit(t, h, o)
	if res.Facts.SSHInterface != "eth0" || res.Facts.FirewalldZone != "internal" || res.Facts.FirewalldZoneDefault {
		t.Errorf("facts: iface %q zone %q default %v", res.Facts.SSHInterface, res.Facts.FirewalldZone, res.Facts.FirewalldZoneDefault)
	}
	for _, f := range []string{"fwd/internal.runtime", "fwd/internal.perm"} {
		if got := h.read(f); got != "443/tcp\n" {
			t.Errorf("%s = %q (22 is covered by the ssh service)", f, got)
		}
	}
	if strings.Contains(h.read("firewalld.log"), "--reload") || h.has("fwd/public.perm") && strings.Contains(h.read("fwd/public.perm"), "443") {
		t.Errorf("firewalld.log:\n%s", h.read("firewalld.log"))
	}
	// A second run sees the ports.
	res = mustInit(t, h, h.optionsWith(func(o *Options) { o.Firewall = []int{443} }))
	if ids := planChanges(res); len(ids) != 0 {
		t.Errorf("second run changes %v", ids)
	}

	// No zone for the interface: the default zone, with a warning.
	h2 := newFakeHost(t, rocky9)
	h2.write("packages", "coreutils\nca-certificates\nshadow-utils\ncurl\nfirewalld\n", 0o644)
	h2.write("pkg-bin/firewall-cmd", fakeFirewallCmd, 0o755)
	h2.write("pkg-bin/firewall-offline-cmd", fakeFirewallCmd, 0o755)
	h2.write("units/firewalld.service.active", "", 0o644)
	o2 := h2.options()
	o2.Firewall = []int{443}
	res = mustInit(t, h2, o2)
	warned := false
	for _, w := range res.Warnings {
		warned = warned || strings.Contains(w, "eth0, has no firewalld zone") && strings.Contains(w, "default zone public")
	}
	if !warned || h2.read("fwd/public.runtime") != "443/tcp\n" || h2.read("fwd/public.perm") != "443/tcp\n" {
		t.Errorf("warnings %v runtime %q perm %q", res.Warnings, h2.read("fwd/public.runtime"), h2.read("fwd/public.perm"))
	}
}

// Item 9: commented and restricted authorized_keys lines.
func TestAuthorizedKeyDetection(t *testing.T) {
	t.Parallel()
	t.Run("a commented-out key is not authorized", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		h.addUser("deploy", "*", "/bin/bash", "# "+h.clientKey()+" old")
		res := mustInit(t, h, h.options())
		if !contains(res.Applied, StepAuthorizedKey) {
			t.Errorf("applied = %v", res.Applied)
		}
		if got := h.read("fs/home/deploy/.ssh/authorized_keys"); !strings.HasSuffix(got, "\n"+h.clientKey()+" me@laptop\n") {
			t.Errorf("authorized_keys = %q", got)
		}
	})
	t.Run("a restricted key is left alone without --key", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		line := `restrict,command="/usr/bin/true" ` + h.clientKey() + " ci"
		h.addUser("deploy", "*", "/bin/bash", line)
		res, _ := h.runner().Init(context.Background(), h.options())
		if res.Facts == nil || !res.Facts.KeyRestricted || res.Facts.KeyPresent {
			t.Fatalf("facts = %+v", res.Facts)
		}
		if got := h.read("fs/home/deploy/.ssh/authorized_keys"); got != line+"\n" {
			t.Errorf("authorized_keys changed: %q", got)
		}
		warned := false
		for _, w := range res.Warnings {
			warned = warned || strings.Contains(w, "only on a line with options") && strings.Contains(w, "--key")
		}
		if !warned {
			t.Errorf("warnings = %v", res.Warnings)
		}
	})
	t.Run("a restricted key gets an unrestricted line with --key", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		line := `from="10.0.0.0/8",no-pty ` + h.clientKey() + " ci"
		h.addUser("deploy", "*", "/bin/bash", line)
		o := h.options()
		o.KeyExplicit = true
		mustInit(t, h, o)
		if got := h.read("fs/home/deploy/.ssh/authorized_keys"); got != line+"\n"+h.clientKey()+" me@laptop\n" {
			t.Errorf("authorized_keys = %q", got)
		}
	})
}

// Item 10: the auth key file is trapped and tailscale up is bounded.
func TestTailscaleUpIsTrappedAndBounded(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.TailscaleKey = NewSecret(tsKey)
	mustInit(t, h, o)
	if !strings.Contains(h.read("timeout.log"), "timeout 120 tailscale up --authkey=file:") {
		t.Errorf("timeout.log = %q", h.read("timeout.log"))
	}
	joined := strings.Join(h.Exec.Scripts(), "\n")
	if !strings.Contains(joined, `trap 'rm -f "$depper_kf"' EXIT HUP INT TERM`) {
		t.Error("no trap around the key file")
	}
	if entries, _ := os.ReadDir(filepath.Join(h.Root, "tmp")); len(entries) != 0 {
		t.Errorf("leftover files: %v", entries)
	}
}

// Item 11: every host init connection disables forwarding, dry run too.
func TestInitConnectionsDisableForwarding(t *testing.T) {
	t.Parallel()
	for _, dry := range []bool{true, false} {
		h := newFakeHost(t, debian12)
		o := h.options()
		o.DryRun = dry
		o.HardenSSH = true
		mustInit(t, h, o)
		n := 0
		for _, argv := range h.Exec.Argv() {
			if !strings.Contains(argv, "/bin/ssh ") {
				continue
			}
			n++
			if !strings.Contains(argv, "-o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes") {
				t.Errorf("dry=%v: ssh without the forwarding options: %s", dry, argv)
			}
		}
		if n == 0 {
			t.Errorf("dry=%v: no ssh calls", dry)
		}
	}
}

// Item 12: --host-key-fingerprint and --trust-on-first-use.
func TestHostKeyFingerprint(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	ed := strings.Fields(strings.Split(h.read("hostkeys"), "\n")[0])
	fp := HostKey{Type: ed[0], Blob: ed[1]}.Fingerprint()

	// No pin, --yes, no fingerprint, no --trust-on-first-use: refused.
	o := h.options()
	o.TrustOnFirstUse = false
	_, err := h.runner().Init(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "--host-key-fingerprint") || !strings.Contains(err.Error(), "--trust-on-first-use") {
		t.Fatalf("err = %v", err)
	}
	if h.has("ssh.log") || h.has("repo/.depper/known_hosts") {
		t.Error("connected or pinned without verification")
	}

	// A fingerprint that matches nothing: refused.
	o = h.options()
	o.TrustOnFirstUse = false
	o.HostKeyFingerprints = []string{"SHA256:" + strings.Repeat("A", 43)}
	if _, err := h.runner().Init(context.Background(), o); err == nil || !strings.Contains(err.Error(), "none of the host keys") {
		t.Fatalf("err = %v", err)
	}
	if h.has("ssh.log") || h.has("repo/.depper/known_hosts") {
		t.Error("connected or pinned after a fingerprint mismatch")
	}

	// A matching fingerprint: only that key is pinned, without a question.
	o = h.options()
	o.TrustOnFirstUse, o.Yes = false, false
	o.HostKeyFingerprints = []string{fp}
	r := h.runner()
	r.Confirm = func(q string) (bool, error) {
		if strings.Contains(q, "Trust") {
			t.Errorf("asked %q although the fingerprint matched", q)
		}
		return true, nil
	}
	res, err := r.Init(context.Background(), o)
	if err != nil {
		t.Fatalf("%v\n%s", err, h.Out.String())
	}
	if kh := h.read("repo/.depper/known_hosts"); kh != "127.0.0.1 "+ed[0]+" "+ed[1]+"\n" {
		t.Errorf("known_hosts = %q", kh)
	}
	if res.HostKeyStatus != HostKeyAdded || len(res.HostKeys) != 1 {
		t.Errorf("result = %s %+v", res.HostKeyStatus, res.HostKeys)
	}
	o = h.options()
	o.HostKeyFingerprints = []string{"MD5:ab:cd"}
	if _, err := h.runner().Init(context.Background(), o); err == nil || !strings.Contains(err.Error(), "invalid --host-key-fingerprint") {
		t.Errorf("err = %v", err)
	}
}

// Item 13: depper.toml must not change under host init, and symbolic links
// are written through.
func TestInitRefusesAConfigChangedDuringTheRun(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.Yes = false
	r := h.runner()
	r.Confirm = func(q string) (bool, error) {
		if strings.Contains(q, "Apply") {
			appendFile(filepath.Join(h.Repo, "depper.toml"), "# edited meanwhile")
		}
		return true, nil
	}
	res, err := r.Init(context.Background(), o)
	if !errors.Is(err, ErrConfigChanged) || !strings.Contains(err.Error(), "depper.toml changed while host init was running") {
		t.Fatalf("err = %v", err)
	}
	if got := h.read("repo/depper.toml"); got != sampleConfig+"# edited meanwhile\n" || res.ConfigWritten {
		t.Errorf("depper.toml = %q", got)
	}
}

func TestAtomicWriteFollowsSymlinks(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	target := filepath.Join(dir, "real", "depper.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "depper.toml")
	if err := os.Symlink("real/depper.toml", link); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(link, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v %v", info, err)
	}
	if data, _ := os.ReadFile(target); string(data) != "new" {
		t.Errorf("target = %q", data)
	}
	// A dangling link (known_hosts kept elsewhere) creates its target.
	kh := filepath.Join(dir, "known_hosts")
	if err := os.Symlink(filepath.Join(dir, "real", "kh"), kh); err != nil {
		t.Fatal(err)
	}
	if err := writeKnownHosts(kh, "", "h", []HostKey{{Type: "ssh-ed25519", Blob: "AAAA"}}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "real", "kh")); string(data) != "h ssh-ed25519 AAAA\n" {
		t.Errorf("known_hosts target = %q", data)
	}
	if info, _ := os.Lstat(kh); info.Mode()&os.ModeSymlink == 0 {
		t.Error("the known_hosts link was replaced")
	}
}

// Item 14: half-created accounts.
func TestInitRepairsHalfCreatedUser(t *testing.T) {
	t.Parallel()
	t.Run("locked password with UsePAM no", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		h.write("fs/etc/ssh/sshd_config", "Include /etc/ssh/sshd_config.d/*.conf\nUsePAM no\n", 0o644)
		h.addUser("deploy", "!", "/bin/bash", h.clientKey())
		res := mustInit(t, h, h.options())
		if res.Facts.UserPassword != "locked" || !strings.Contains(h.changes(), "usermod -p * deploy") || shadowField(h, "deploy") != "*" {
			t.Errorf("password %q changes:\n%s", res.Facts.UserPassword, h.changes())
		}
	})
	t.Run("locked password with UsePAM yes is fine", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		h.addUser("deploy", "!!", "/bin/bash", h.clientKey())
		mustInit(t, h, h.options())
		if strings.Contains(h.changes(), "usermod -p") || shadowField(h, "deploy") != "!!" {
			t.Errorf("changes:\n%s", h.changes())
		}
	})
	t.Run("nologin shell of a user depper created", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		h.addUser("deploy", "*", "/usr/sbin/nologin", h.clientKey())
		rec, _ := json.Marshal(Record{DeployUser: "deploy", Container: "none", Steps: []string{StepUser}})
		h.write("fs/etc/depper/host.json", string(rec), 0o644)
		mustInit(t, h, h.options())
		if !strings.Contains(h.changes(), "usermod -s /bin/sh deploy") || !strings.Contains(h.read("fs/etc/passwd"), ":/bin/sh\n") {
			t.Errorf("changes:\n%s", h.changes())
		}
	})
	t.Run("nologin shell of a foreign user", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		h.addUser("deploy", "*", "/bin/false", h.clientKey())
		res, _ := h.runner().Init(context.Background(), h.options())
		if strings.Contains(h.changes(), "usermod -s") {
			t.Errorf("changed a foreign account's shell:\n%s", h.changes())
		}
		warned := false
		for _, w := range res.Warnings {
			warned = warned || strings.Contains(w, "login shell /bin/false")
		}
		if !warned {
			t.Errorf("warnings = %v", res.Warnings)
		}
	})
}

// Item 17: Amazon Linux.
func TestInitAmazonLinux(t *testing.T) {
	t.Parallel()
	amzn := distro{
		osRelease: "NAME=\"Amazon Linux\"\nVERSION_ID=\"2023\"\nID=\"amzn\"\nID_LIKE=\"fedora\"\nPRETTY_NAME=\"Amazon Linux 2023\"\n",
		sshUnit:   "sshd.service",
		installed: []string{"coreutils", "ca-certificates", "shadow-utils", "curl"},
		preBin:    []string{"newuidmap", "curl"},
	}
	h := newFakeHost(t, amzn)
	o := h.options()
	o.Container = ContainerPodman
	_, err := h.runner().Init(context.Background(), o)
	var unsupported *ErrUnsupported
	if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "Amazon Linux") || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("err = %v", err)
	}
	o = h.options()
	o.Container = ContainerDocker
	o.TailscaleKey = NewSecret(tsKey)
	mustInit(t, h, o)
	if !strings.Contains(h.changes(), "dnf -y -q install rsync docker") || !strings.Contains(h.changes(), "https://pkgs.tailscale.com/stable/amazon-linux/2023/tailscale.repo") {
		t.Errorf("changes:\n%s", h.changes())
	}
}

// Items 17 and 19: host status reads the effective sshd settings, and
// hardening recorded but no longer in effect is a warning.
func TestStatusChecksEffectiveSSHD(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.HardenSSH = true
	o.Firewall = []int{80}
	mustInit(t, h, o)
	cfg, err := config.LoadTarget(h.Repo, "", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string) StatusCheck {
		st, err := h.runner().Status(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range st.Checks {
			if c.Name == name {
				return c
			}
		}
		return StatusCheck{}
	}
	if c := check("ssh hardening"); c.Status != StatusPass || !strings.Contains(c.Detail, "PasswordAuthentication no") || !strings.Contains(c.Detail, "sshd -T") {
		t.Errorf("hardened: %+v", c)
	}
	if !strings.Contains(h.read("sshd.log"), "-T -h ") || !strings.Contains(h.read("sshd.log"), "-C user=deploy,host=192.0.2.7,addr=192.0.2.7,laddr=10.0.0.5,lport=22") {
		t.Errorf("sshd.log:\n%s", h.read("sshd.log"))
	}
	// An earlier drop-in now turns passwords back on: the file is still
	// there, but the effective value says otherwise.
	h.write("fs/etc/ssh/sshd_config.d/01-override.conf", "PasswordAuthentication yes\n", 0o644)
	if c := check("ssh hardening"); c.Status != StatusWarn || !strings.Contains(c.Detail, "recorded as applied by depper but not in effect now") || !strings.Contains(c.Detail, "PasswordAuthentication yes") {
		t.Errorf("overridden: %+v", c)
	}
	h.remove("ufw-active")
	h.write("fs/etc/ufw/ufw.conf", "ENABLED=no\n", 0o644)
	if c := check("firewall"); c.Status != StatusWarn || !strings.Contains(c.Detail, "recorded as applied by depper but not in effect now") {
		t.Errorf("firewall off: %+v", c)
	}
}
