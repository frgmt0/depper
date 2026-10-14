package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// managerGroups returns the groups of the fake user manager of uid (its
// /proc status), and whether it is running.
func managerGroups(h *fakeHost, uid string) ([]string, bool) {
	pid := strings.TrimSpace(h.read("units/user@" + uid + ".service.mainpid"))
	if pid == "" {
		return nil, false
	}
	for _, line := range strings.Split(h.read("fs/proc/"+pid+"/status"), "\n") {
		if rest, ok := strings.CutPrefix(line, "Groups:"); ok {
			return strings.Fields(rest), true
		}
	}
	return nil, true
}

func deployUID(t *testing.T, h *fakeHost) string {
	t.Helper()
	for _, line := range strings.Split(h.read("fs/etc/passwd"), "\n") {
		if f := strings.Split(line, ":"); len(f) >= 7 && f[0] == "deploy" {
			return f[2]
		}
	}
	t.Fatal("no deploy user")
	return ""
}

func stepByID(res *InitResult, id string) *Step {
	for _, s := range res.Plan {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// On a fresh host the deploy user joins the docker group before linger
// starts its user manager, so the manager (and every service it starts)
// has the group and no restart is needed.
func TestInitDockerGroupBeforeUserManager(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	res := mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
	groups, running := managerGroups(h, deployUID(t, h))
	if !running || !contains(groups, "998") {
		t.Errorf("user manager running %v with groups %v, want the docker group 998\nevents:\n%s", running, groups, h.read("events"))
	}
	if strings.Contains(h.changes(), "systemctl restart") {
		t.Errorf("a fresh setup restarted the user manager:\n%s", h.changes())
	}
	if s := stepByID(res, StepManagerGroups); s == nil || s.Change {
		t.Errorf("manager groups step = %+v", s)
	}
	changes := h.changes()
	if i, j := strings.Index(changes, "usermod -aG docker deploy"), strings.Index(changes, "loginctl enable-linger deploy"); i < 0 || j < 0 || i > j {
		t.Errorf("usermod -aG docker does not come before enable-linger:\n%s", changes)
	}
}

// A user manager that was running before the deploy user joined the docker
// group (an earlier host init, or linger enabled by hand) lacks the group:
// host init restarts it, says so in the plan, and checks the result. A
// later run that finds the user already in the group but the manager still
// without it (the state the real host was left in) restarts it as well.
func TestInitRestartsUserManagerWithoutDockerGroup(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, archLinux)
	mustInit(t, h, h.options()) // no container: user, linger, manager
	uid := deployUID(t, h)
	if groups, running := managerGroups(h, uid); !running || contains(groups, "998") {
		t.Fatalf("setup: manager running %v groups %v", running, groups)
	}

	res := mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
	s := stepByID(res, StepManagerGroups)
	if s == nil || !s.Change || !strings.Contains(s.Detail, "restarts all of deploy's services (a short interruption for anything already deployed)") {
		t.Fatalf("manager groups step = %+v", s)
	}
	if !strings.Contains(h.Out.String(), "restarts user@"+uid+".service") {
		t.Errorf("no warning about the restart:\n%s", h.Out.String())
	}
	changes := h.changes()
	i, j := strings.Index(changes, "usermod -aG docker deploy"), strings.Index(changes, "systemctl restart user@"+uid+".service")
	if i < 0 || j < 0 || i > j {
		t.Errorf("want usermod, then the restart:\n%s", changes)
	}
	if groups, _ := managerGroups(h, uid); !contains(groups, "998") {
		t.Errorf("after host init the user manager has groups %v, want the docker group 998", groups)
	}
	if !contains(readRecord(t, h).Steps, StepManagerGroups) {
		t.Errorf("record steps = %v", readRecord(t, h).Steps)
	}
	if c := statusChecks(t, h)["docker group"]; c.Status != StatusPass {
		t.Errorf("status docker group = %+v", c)
	}

	// Already a member, manager started before that: restart again.
	h.write("fs/proc/"+strings.TrimSpace(h.read("units/user@"+uid+".service.mainpid"))+"/status", "Name:\tsystemd\nGroups:\t"+uid+" \n", 0o644)
	if c := statusChecks(t, h)["docker group"]; c.Status != StatusFail || !strings.Contains(c.Detail, "service manager lacks docker group") || !strings.Contains(c.Detail, "depper host init") || !strings.Contains(c.Detail, "user@"+uid+".service") {
		t.Errorf("status docker group = %+v", c)
	}
	before := h.changes()
	res = mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
	if ids := planChanges(res); !contains(ids, StepManagerGroups) || contains(ids, StepDockerGroup) {
		t.Errorf("plan changes %v", ids)
	}
	if !strings.Contains(strings.TrimPrefix(h.changes(), before), "systemctl restart user@"+uid+".service") {
		t.Errorf("not restarted:\n%s", strings.TrimPrefix(h.changes(), before))
	}
	if groups, _ := managerGroups(h, uid); !contains(groups, "998") {
		t.Errorf("groups %v", groups)
	}

	// Now nothing is left to do.
	before = h.changes()
	res = mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
	if ids := planChanges(res); len(ids) != 0 || h.changes() != before {
		t.Errorf("third run changes %v:\n%s", ids, strings.TrimPrefix(h.changes(), before))
	}
	checkScriptSyntax(t, h.Exec.Scripts())
}

// The restart is verified: a manager that comes back without the group
// fails the step.
func TestInitManagerRestartVerifiesGroups(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	mustInit(t, h, h.options())
	// A restart that does not pick up the new group.
	h.write("manager-ignores-groups", "", 0o644)
	_, err := h.runner().Init(context.Background(), h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
	if err == nil || !strings.Contains(h.Out.String(), "still lacks the docker group (gid 998) after the restart") {
		t.Fatalf("err = %v\n%s", err, h.Out.String())
	}
}

// Socket activation: docker.socket enabled with docker.service disabled
// (Arch's normal setup) needs nothing; a host without either gets
// docker.socket, never a boot-time docker.service the admin did not choose,
// and host.json records what depper enabled.
func TestInitDockerSocketActivation(t *testing.T) {
	t.Parallel()
	t.Run("socket already enabled", func(t *testing.T) {
		t.Parallel()
		d := archLinux
		d.installed = append(append([]string{}, d.installed...), "docker")
		d.preBin = append(append([]string{}, d.preBin...), "docker")
		h := newFakeHost(t, d)
		h.write("fs/etc/group", h.read("fs/etc/group")+"docker:x:998:\n", 0o644)
		for _, f := range []string{"docker.service.file", "docker.socket.file", "docker.socket.enabled", "docker.socket.active"} {
			h.write("units/"+f, "", 0o644)
		}
		res := mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
		if s := stepByID(res, StepDockerService); s == nil || s.Change || !strings.Contains(s.Detail, "docker.socket is enabled") {
			t.Errorf("docker step = %+v", s)
		}
		if strings.Contains(h.changes(), "enable --now docker") || h.has("units/docker.service.enabled") {
			t.Errorf("docker enabled again:\n%s", h.changes())
		}
		if res.Facts == nil || !res.Facts.DockerSocketEnabled || !res.Facts.DockerSocketActive || res.Facts.DockerEnabled {
			t.Errorf("facts = %+v", res.Facts)
		}
		if rec := readRecord(t, h); len(rec.EnabledUnits) != 0 {
			t.Errorf("enabled units = %v", rec.EnabledUnits)
		}
	})
	t.Run("fresh arch", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, archLinux)
		mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
		if !strings.Contains(h.changes(), "systemctl enable --now docker.socket") || strings.Contains(h.changes(), "docker.service") {
			t.Errorf("changes:\n%s", h.changes())
		}
		if rec := readRecord(t, h); strings.Join(rec.EnabledUnits, ",") != "docker.socket" {
			t.Errorf("enabled units = %v", rec.EnabledUnits)
		}
		// A second run finds the socket enabled and keeps the record.
		res := mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
		if ids := planChanges(res); len(ids) != 0 {
			t.Errorf("second run changes %v", ids)
		}
		if rec := readRecord(t, h); strings.Join(rec.EnabledUnits, ",") != "docker.socket" {
			t.Errorf("enabled units after a second run = %v", rec.EnabledUnits)
		}
	})
	t.Run("no docker.socket unit", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, fedora40)
		h.write("no-docker-socket", "", 0o644)
		mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
		if !strings.Contains(h.changes(), "systemctl enable --now docker.service") {
			t.Errorf("changes:\n%s", h.changes())
		}
		if rec := readRecord(t, h); strings.Join(rec.EnabledUnits, ",") != "docker.service" {
			t.Errorf("enabled units = %v", rec.EnabledUnits)
		}
	})
	t.Run("debian package enabled it", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
		if strings.Contains(h.changes(), "enable --now docker") {
			t.Errorf("enabled docker on top of the package:\n%s", h.changes())
		}
		if !strings.Contains(h.Out.String(), "docker is already enabled") {
			t.Errorf("output:\n%s", h.Out.String())
		}
		if rec := readRecord(t, h); len(rec.EnabledUnits) != 0 {
			t.Errorf("enabled units = %v", rec.EnabledUnits)
		}
	})
}

// When podman-compose cannot be installed but a docker-compose plugin is
// on the host, host init accepts it as podman's compose provider and
// enables podman.socket for the deploy user instead of refusing; the plan
// and host.json say so.
func TestInitPodmanComposeThroughDockerCompose(t *testing.T) {
	t.Parallel()
	d := archLinux
	d.sync = strings.Replace(archSync, "podman-compose 1.2.0-1\n", "", 1)
	h := newFakeHost(t, d)
	h.write("fs/usr/lib/docker/cli-plugins/docker-compose", stubTool, 0o755)
	h.write("repo/depper.toml", strings.Replace(sampleConfig, "command = \"./run\" # start command\n", "", 1)+"\n[container]\nengine = \"podman\"\ncompose = \"compose.yaml\"\n", 0o644)
	res := mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerAuto }))
	s := stepByID(res, StepPodmanSocket)
	if s == nil || !s.Change || !strings.Contains(s.Detail, "/usr/lib/docker/cli-plugins/docker-compose") {
		t.Fatalf("podman socket step = %+v", s)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "podman-compose cannot be installed") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	if strings.Contains(h.read("pacman.log"), "podman-compose") && strings.Contains(h.changes(), "podman-compose") {
		t.Errorf("tried to install podman-compose:\n%s", h.changes())
	}
	if !strings.Contains(h.changes(), "systemctl --user enable --now podman.socket (uid "+deployUID(t, h)+")") {
		t.Errorf("podman.socket not enabled as deploy:\n%s", h.changes())
	}
	rec := readRecord(t, h)
	if rec.ComposeProvider != "docker-compose (/usr/lib/docker/cli-plugins/docker-compose) through podman.socket" || strings.Join(rec.EnabledUnits, ",") != "podman.socket (user deploy)" || !contains(rec.Steps, StepPodmanSocket) {
		t.Errorf("record = %+v", rec)
	}
	data, _ := json.Marshal(res)
	if !strings.Contains(string(data), `"compose_plugin":"/usr/lib/docker/cli-plugins/docker-compose"`) {
		t.Errorf("JSON lacks the plugin: %s", data)
	}
	// Idempotent.
	before := h.changes()
	res = mustInit(t, h, h.optionsWith(func(o *Options) { o.Container = ContainerAuto }))
	if ids := planChanges(res); len(ids) != 0 || h.changes() != before {
		t.Errorf("second run changes %v", ids)
	}
	if rec2 := readRecord(t, h); rec2.ComposeProvider != rec.ComposeProvider || strings.Join(rec2.EnabledUnits, ",") != "podman.socket (user deploy)" {
		t.Errorf("record after a second run = %+v", rec2)
	}
	checkScriptSyntax(t, h.Exec.Scripts())

	// Without any docker-compose either, host init still refuses.
	h2 := newFakeHost(t, d)
	h2.write("repo/depper.toml", h.read("repo/depper.toml"), 0o644)
	if _, err := h2.runner().Init(context.Background(), h2.optionsWith(func(o *Options) { o.Container = ContainerAuto })); err == nil || !strings.Contains(err.Error(), "no compose provider for podman") {
		t.Errorf("err = %v", err)
	}
}

// A failed pacman download is reported once: pacman's and depper's
// messages are shown as they come, and the final error does not repeat
// them; the JSON report keeps them.
func TestInitPacmanFailureReportedOnce(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, archLinux)
	h.write("pacman-download-fails", "", 0o644)
	res, err := h.runner().Init(context.Background(), h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
	if err == nil {
		t.Fatal("init succeeded")
	}
	const msg = "pacman could not install rsync docker"
	shown := h.Out.String() + "depper: " + err.Error() + "\n"
	if n := strings.Count(shown, msg); n != 1 {
		t.Errorf("%q appears %d times:\n%s", msg, n, shown)
	}
	if !strings.Contains(err.Error(), "applying the plan failed after 0 of") || !strings.Contains(err.Error(), "the host's messages are above") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(res.Error, msg) {
		t.Errorf("JSON error = %q", res.Error)
	}
	noPacmanRefresh(t, h)
}
