package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/transport"
)

// checkScriptHygiene asserts every remote script is wrapped (parsed fully
// before it runs) and moves files only with mv -T (never into a directory
// that happens to exist at the destination).
func checkScriptHygiene(t *testing.T, scripts []string) {
	t.Helper()
	mv := regexp.MustCompile(`(^|[\s;{(&|])mv\s+(\S+)`)
	for i, s := range scripts {
		if !strings.HasPrefix(s, "set +xv\n{\n") || !strings.HasSuffix(s, "\n}\n") {
			t.Errorf("script %d is not wrapped:\n%s", i, s)
		}
		for _, m := range mv.FindAllStringSubmatch(s, -1) {
			if !strings.HasPrefix(m[2], "-T") {
				t.Errorf("script %d moves without -T (%q):\n%s", i, m[0], s)
			}
		}
	}
}

// The rsync destination is host:/abs/path/ with no quoting (rsync >= 3.2.4
// protects arguments and would keep quotes as part of the path).
func TestRsyncDestinationIsUnquoted(t *testing.T) {
	h := newFakeHost(t)
	e := h.Engine(loadConfig(t, processRepo(t)))
	r := deploy(t, e, DeployOptions{})
	args := strings.Split(h.log("rsync-args"), "\n")
	if got, want := args[len(args)-1], "deploy@fakehost:"+h.path(base)+"/releases/"+r.Release+"/"; got != want {
		t.Errorf("rsync destination = %q, want %q", got, want)
	}
	scripts := h.Exec.Scripts()
	checkScriptHygiene(t, scripts)
	for _, argv := range h.Exec.Argv() {
		if strings.HasSuffix(argv, " sh -s") && !strings.Contains(argv, " -T -- ") {
			t.Errorf("script ssh without -T: %s", argv)
		}
	}
}

// After a failed automatic restore the error explains the recovery, the
// transaction stays marked interrupted, and `depper rollback` restores the
// state from before that deploy (not "the previous release").
func TestFailedRestoreAndInterruptedRollback(t *testing.T) {
	h := newFakeHost(t)
	toml := strings.Replace(processToml, `command = "test ! -f \"$HOME/unhealthy\"`, `command = "if test -f \"$HOME/break\"; then printf %s \"--user daemon-reload\" > \"$FAKE_ROOT/fail-systemctl\"; exit 1; fi; test ! -f \"$HOME/unhealthy\"`, 1)
	repo := writeRepo(t, toml, map[string]string{"app.py": "", "healthy": "", ".env.prod": "TOKEN=one\n"})
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()
	r1 := deploy(t, e, DeployOptions{})
	env1 := h.read(base + "/.depper/environment")

	h.touch("break")
	writeFile(t, filepath.Join(repo, ".env.prod"), "TOKEN=two\n")
	res, err := e.Deploy(ctx, DeployOptions{})
	if err == nil {
		t.Fatal("deploy succeeded")
	}
	msg := err.Error()
	for _, want := range []string{"automatic restore also failed", "depper unlock --force", "`depper rollback`", "depper rollback --to " + r1.Release} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	tx := base + "/.depper/transactions/" + res.Release
	if !h.exists(tx + "/" + InterruptedMarker) {
		t.Fatal("interrupted marker missing")
	}
	if h.exists(base + "/.depper/lock") {
		t.Error("lock not released after the failed restore")
	}

	// Fix the cause; plain `depper rollback` restores the pre-deploy state.
	if err := os.Remove(filepath.Join(h.Root, "fail-systemctl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.path("break")); err != nil {
		t.Fatal(err)
	}
	h.Out.Reset()
	rb, err := e.Rollback(ctx, "")
	if err != nil {
		t.Fatalf("rollback: %v\n%s", err, h.Out)
	}
	if rb.RestoredTransaction != res.Release || rb.To != r1.Release || !strings.Contains(h.Out.String(), "interrupted transaction "+res.Release) {
		t.Errorf("rollback = %+v\n%s", rb, h.Out)
	}
	if h.link(base+"/current") != "releases/"+r1.Release || h.read(base+"/.depper/environment") != env1 {
		t.Error("the pre-deploy state was not restored")
	}
	if h.exists(tx) || h.exists(base+"/releases/"+res.Release) {
		t.Error("the interrupted deploy's backup or release was kept")
	}
	if !h.exists("../systemd/depper-api.service.active") {
		t.Error("service not running after the restore")
	}
	// No interrupted transaction is left: the next rollback is a normal one.
	if _, err := e.Rollback(ctx, ""); err == nil || !strings.Contains(err.Error(), "No previous release") {
		t.Errorf("second rollback = %v", err)
	}
	checkScriptHygiene(t, h.Exec.Scripts())
	checkScriptSyntax(t, h.Exec.Scripts())
}

// Ctrl-C during setup: the release is left in place (marked discarded) for
// a later prune instead of being deleted under a still-running command, and
// the lock is released only after the remote command returned.
func TestInterruptedSetupKeepsTheRelease(t *testing.T) {
	h := newFakeHost(t)
	toml := strings.Replace(processToml, `setup = ["printf setup-ran > setup.txt"]`, `setup = ["touch \"$HOME/setup-started\"; sleep 3"]`, 1)
	toml = strings.Replace(toml, "keep_releases = 2", "keep_releases = 5", 1)
	repo := writeRepo(t, toml, map[string]string{"app.py": "", "healthy": "", ".env.prod": "TOKEN=one\n"})
	e := h.Engine(loadConfig(t, repo))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for i := 0; i < 500 && !h.exists("setup-started"); i++ {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	res, err := e.Deploy(ctx, DeployOptions{})
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("interrupted deploy = %v", err)
	}
	if time.Since(start) < transport.WaitDelay {
		t.Error("the setup command was not given WaitDelay to finish")
	}
	rel := base + "/releases/" + res.Release
	if !h.exists(rel) || !h.exists(rel+"/"+DiscardedMarker) {
		t.Fatal("the interrupted release was removed (or not marked discarded)")
	}
	if h.exists(base+"/.depper/transactions/"+res.Release) || h.exists(base+"/.depper/lock") {
		t.Error("transaction or lock left behind")
	}
	// The next deploy prunes the discarded release regardless of keep_releases.
	if err := os.Remove(h.path("setup-started")); err != nil {
		t.Fatal(err)
	}
	ok := deploy(t, e, DeployOptions{})
	if h.exists(rel) || !strings.Contains(strings.Join(ok.Pruned, ","), res.Release) {
		t.Errorf("discarded release not pruned (pruned %v)", ok.Pruned)
	}
}

// Text a login shell prints before the script runs (motd, rc files) must
// not break any parser.
func TestLoginShellNoiseIsIgnored(t *testing.T) {
	h := newFakeHost(t)
	writeFile(t, filepath.Join(h.Root, "login-noise"), "Welcome to fakehost!\nlayout=both\nheld\n@@unit\nActiveState=failed\nTOKEN=noise\n")
	e := h.Engine(loadConfig(t, processRepo(t)))
	ctx := context.Background()
	r1 := deploy(t, e, DeployOptions{})
	if string(r1.Health.Body) != `{"state":"ready","port":"8080"}` {
		t.Errorf("health body = %s", r1.Health.Body)
	}
	r2 := deploy(t, e, DeployOptions{})
	st, err := e.Status(ctx)
	if err != nil || st.CurrentRelease != r2.Release || st.ActiveState != "active" {
		t.Fatalf("status = %+v (%v)", st, err)
	}
	l := mustProbe(t, e)
	if list, err := e.ListReleases(ctx, l); err != nil || list.Current != r2.Release || len(list.Releases) != 2 {
		t.Errorf("releases = %+v (%v)", list, err)
	}
	if lock, err := e.ReadLock(ctx, l); err != nil || lock.Held {
		t.Errorf("lock = %+v (%v)", lock, err)
	}
	if _, err := e.Rollback(ctx, ""); err != nil {
		t.Errorf("rollback: %v", err)
	}
	if cur, err := e.readCurrent(ctx, l); err != nil || cur != r1.Release {
		t.Errorf("current = %q (%v)", cur, err)
	}
}

// A health command exiting 255 is an unhealthy result, not an ssh failure.
func TestHealthCommandExit255IsUnhealthy(t *testing.T) {
	h := newFakeHost(t)
	e := h.Engine(loadConfig(t, processRepo(t)))
	deploy(t, e, DeployOptions{})
	l := mustProbe(t, e)
	e.Config.Health.Command = "exit 255"
	r, err := e.CheckHealth(context.Background(), l)
	if err != nil || r.OK || !strings.Contains(r.Error, "status 255") {
		t.Fatalf("health = %+v, %v", r, err)
	}
	e.Config.Health.Command = "exit 3"
	if r, err := e.CheckHealth(context.Background(), l); err != nil || r.OK || !strings.Contains(r.Error, "status 3") {
		t.Errorf("health = %+v, %v", r, err)
	}
}

// [health].timeout bounds the HTTP exchange only; the SSH handshake gets
// its own allowance (max(15s, timeout)).
func TestURLHealthTimeoutExcludesSSHHandshake(t *testing.T) {
	h := newFakeHost(t)
	repo := writeRepo(t, strings.Replace(urlToml, "timeout = 2", "timeout = 0.5", 1), map[string]string{"x": ""})
	e := h.Engine(loadConfig(t, repo))
	writeFile(t, filepath.Join(h.Root, "http-status"), "200")
	writeFile(t, filepath.Join(h.Root, "forward-delay"), "1200")
	res := deploy(t, e, DeployOptions{})
	if !res.Health.OK || res.Health.Method != MethodForward {
		t.Fatalf("slow handshake made the check fail: %+v", res.Health)
	}
	// A slow response is still bounded by [health].timeout.
	if err := os.Remove(filepath.Join(h.Root, "forward-delay")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(h.Root, "response-delay"), "1500")
	r, err := e.CheckHealth(context.Background(), mustProbe(t, e))
	if err != nil || r.OK || !strings.Contains(r.Error, "0.5s") {
		t.Errorf("slow response = %+v, %v", r, err)
	}
	if DialTimeout(e.Config.Health) != 15*time.Second {
		t.Errorf("DialTimeout = %s", DialTimeout(e.Config.Health))
	}
}

// When both a depper and a legacy layout exist, read-only commands warn and
// every command that changes the deployment refuses with the migrate hint.
func TestBothLayoutsRefuseChanges(t *testing.T) {
	h := newFakeHost(t)
	repo := processRepo(t)
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()
	deploy(t, e, DeployOptions{})
	deploy(t, e, DeployOptions{})
	writeFile(t, h.path(".local/share/deployer/api/.deployer/environment"), "")
	h.Out.Reset()
	if _, err := e.Status(ctx); err != nil || !strings.Contains(h.Out.String(), "Warning: both") {
		t.Errorf("status = %v, output %q", err, h.Out)
	}
	env, err := LoadEnvironment(e.Config, EnvSource{})
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]func() error{
		"deploy":       func() error { _, err := e.Deploy(ctx, DeployOptions{}); return err },
		"rollback":     func() error { _, err := e.Rollback(ctx, ""); return err },
		"secrets push": func() error { _, err := e.PushEnvironment(ctx, env); return err },
		"restart":      func() error { return e.Control(ctx, "restart") },
		"stop":         func() error { return e.Control(ctx, "stop") },
		"unlock":       func() error { _, err := e.BreakLock(ctx, true); return err },
	}
	for name, run := range changes {
		if err := run(); err == nil || !strings.Contains(err.Error(), "both a depper deployment") || !strings.Contains(err.Error(), "depper migrate") {
			t.Errorf("%s with both layouts = %v", name, err)
		}
	}
	// The compatibility link left by a migration is not a legacy layout.
	if err := os.RemoveAll(h.path(".local/share/deployer/api")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../depper/api", h.path(".local/share/deployer/api")); err != nil {
		t.Fatal(err)
	}
	if l := mustProbe(t, e); l.State != LayoutDepper {
		t.Errorf("state with the compatibility link = %s", l.State)
	}
	if err := e.Control(ctx, "restart"); err != nil {
		t.Errorf("restart with the compatibility link: %v", err)
	}
}

// A deployment whose unit still reads the container.env of an older depper
// must be redeployed before secrets can be pushed (push no longer writes
// that file).
func TestSecretsPushRefusesOldContainerEnvUnit(t *testing.T) {
	h := newFakeHost(t)
	e := h.Engine(loadConfig(t, processRepo(t)))
	ctx := context.Background()
	deploy(t, e, DeployOptions{})
	unitPath := ".config/systemd/user/depper-api.service"
	writeFile(t, h.path(unitPath), h.read(unitPath)+"# --env-file "+h.path(base)+"/.depper/container.env\n")
	env, err := LoadEnvironment(e.Config, EnvSource{})
	if err != nil {
		t.Fatal(err)
	}
	before := h.read(base + "/.depper/environment")
	if _, err := e.PushEnvironment(ctx, env); err == nil || !strings.Contains(err.Error(), "run depper deploy once") {
		t.Errorf("push = %v", err)
	}
	if h.read(base+"/.depper/environment") != before {
		t.Error("the environment changed")
	}
}

// Releases created within the same second keep their creation order:
// new ids carry milliseconds (and are strictly increasing per engine), and
// second-resolution ids fall back to created_at, never to the random suffix.
func TestReleaseOrderWithinOneSecond(t *testing.T) {
	h := newFakeHost(t)
	e := h.Engine(loadConfig(t, processRepo(t)))
	fixed := time.Date(2026, 10, 13, 6, 3, 38, 0, time.UTC)
	e.Now = func() time.Time { return fixed }
	ids := map[string]bool{}
	var prev string
	for i := 0; i < 50; i++ {
		id, err := e.newReleaseID()
		if err != nil || !ValidReleaseID(id) || ids[id] {
			t.Fatalf("id %q (%v)", id, err)
		}
		if prev != "" && !idNewer(id, prev) {
			t.Fatalf("%s does not sort after %s", id, prev)
		}
		ids[id], prev = true, id
	}
	if !ValidReleaseID("release-20250101T000000Z-00000001") || ValidReleaseID("release-20250101T000000.12Z-00000001") {
		t.Error("id validation")
	}

	// Two real deploys in the same (frozen) second: the second is current
	// and listed first, the first is previous.
	r1 := deploy(t, e, DeployOptions{})
	r2 := deploy(t, e, DeployOptions{})
	list, err := e.ListReleases(context.Background(), mustProbe(t, e))
	if err != nil {
		t.Fatal(err)
	}
	if list.Releases[0].ID != r2.Release || list.Current != r2.Release || list.Previous != r1.Release {
		t.Errorf("order = %s, %s (current %s previous %s)", list.Releases[0].ID, list.Releases[1].ID, list.Current, list.Previous)
	}

	// Legacy second-resolution ids: created_at decides, not the suffix.
	older := ReleaseInfo{ID: "release-20250101T000000Z-ffffffff", Metadata: &ReleaseMetadata{CreatedAt: "2025-01-01T00:00:00.100Z"}}
	newer := ReleaseInfo{ID: "release-20250101T000000Z-00000000", Metadata: &ReleaseMetadata{CreatedAt: "2025-01-01T00:00:00.900Z"}}
	if !releaseNewer(&newer, &older) || releaseNewer(&older, &newer) {
		t.Error("created_at does not break same-second ties")
	}
	if !releaseNewer(&ReleaseInfo{ID: "release-20250101T000000.500Z-00000000"}, &ReleaseInfo{ID: "release-20250101T000000.400Z-ffffffff"}) {
		t.Error("milliseconds ignored")
	}
}
