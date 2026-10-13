package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/shell"
)

const sentinel = "synthetic-secret-not-for-output"

const processToml = `[service]
name = "api"
command = "python3 app.py --label 'it works'"

[deploy]
host = "deploy@fakehost"
setup = ["printf setup-ran > setup.txt"]
keep_releases = 2

[env]
PORT = "8080"

[secrets]
file = ".env.prod"

[endpoint]
url = "http://203.0.113.10:8080"

[health]
command = "test ! -f \"$HOME/unhealthy\" && test -f healthy && printf '{\"state\":\"ready\",\"port\":\"%s\"}' \"$PORT\""
start_period = 1
interval = 0.05
`

func processRepo(t *testing.T) string {
	return writeRepo(t, processToml, map[string]string{
		"app.py":    "print('hello')\n",
		"healthy":   "",
		".env.prod": "TOKEN=" + sentinel + "\nMULTI=\"line one\nline two\"\n",
		"sub/x.txt": "nested\n",
	})
}

const base = ".local/share/depper/api"

func deploy(t *testing.T, e *Engine, opts DeployOptions) *DeployResult {
	t.Helper()
	res, err := e.Deploy(context.Background(), opts)
	if err != nil {
		t.Fatalf("deploy failed: %v\noutput:\n%s", err, e.Out)
	}
	return res
}

// The full lifecycle against the fake host: two deploys, a failed deploy
// that is restored, rollbacks, pruning, status, and releases.
func TestDeployLifecycle(t *testing.T) {
	h := newFakeHost(t)
	repo := processRepo(t)
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()

	// First deploy.
	r1 := deploy(t, e, DeployOptions{})
	if !ValidReleaseID(r1.Release) || r1.Health == nil || !r1.Health.OK || r1.Health.Check != CheckCommand {
		t.Fatalf("result = %+v health %+v", r1, r1.Health)
	}
	if string(r1.Health.Body) != `{"state":"ready","port":"8080"}` {
		t.Errorf("health body = %s", r1.Health.Body)
	}
	if r1.Env.Known || strings.Join(r1.Env.Added, ",") != "MULTI,PORT,TOKEN" {
		t.Errorf("first env summary = %+v", r1.Env)
	}
	rel1 := base + "/releases/" + r1.Release
	if got := h.link(base + "/current"); got != "releases/"+r1.Release {
		t.Errorf("current = %q, want relative link", got)
	}
	if h.exists(rel1 + "/.env.prod") {
		t.Error("secrets file was synced into the release")
	}
	if h.read(rel1+"/setup.txt") != "setup-ran" || h.read(rel1+"/sub/x.txt") != "nested\n" {
		t.Error("release content or setup output missing")
	}
	if h.read(rel1+"/"+StartScriptName) != "#!/bin/sh\n"+strings.Join(shell.SessionEnv(), "\n")+"\nexec python3 app.py --label 'it works'\n" || h.mode(rel1+"/"+StartScriptName) != 0o700 {
		t.Errorf("start script wrong (mode %v)", h.mode(rel1+"/"+StartScriptName))
	}
	var meta ReleaseMetadata
	if err := json.Unmarshal([]byte(h.read(rel1+"/"+ReleaseMetadataName)), &meta); err != nil || meta.ID != r1.Release || meta.Source != "worktree" || meta.DeployedBy != "tester@laptop" {
		t.Errorf("metadata = %+v (%v)", meta, err)
	}
	envFile := h.read(base + "/.depper/environment")
	if !strings.Contains(envFile, `TOKEN="`+sentinel+`"`) || !strings.Contains(envFile, "MULTI=\"line one\nline two\"") || h.mode(base+"/.depper/environment") != 0o600 {
		t.Errorf("environment file = %q mode %v", envFile, h.mode(base+"/.depper/environment"))
	}
	if h.mode(base+"/.depper") != 0o700 || h.mode(base+"/.depper/fingerprint.salt") != 0o600 || h.mode(base+"/.depper/environment.manifest") != 0o600 {
		t.Error("metadata permissions are not private")
	}
	manifest, err := ParseManifest(h.read(base + "/.depper/environment.manifest"))
	if err != nil || manifest.FileSHA256 != SHA256Hex([]byte(envFile)) || len(manifest.Keys) != 3 {
		t.Fatalf("manifest = %+v (%v)", manifest, err)
	}
	if strings.Contains(h.read(base+"/.depper/environment.manifest"), sentinel) {
		t.Error("manifest contains a secret value")
	}
	unitFile := h.read(".config/systemd/user/depper-api.service")
	for _, want := range []string{"WorkingDirectory=" + h.path(base) + "/current", "EnvironmentFile=" + h.path(base) + "/.depper/environment", `ExecStart=/bin/sh "` + h.path(base) + `/current/.depper-start.sh"`, "Restart=on-failure"} {
		if !strings.Contains(unitFile, want) {
			t.Errorf("unit missing %q:\n%s", want, unitFile)
		}
	}
	if !h.exists(base+"/.depper/transactions/"+r1.Release+"/applied/environment") || !h.exists(base+"/.depper/transactions/"+r1.Release+"/no-current") {
		t.Error("transaction backup / applied snapshot missing")
	}
	if h.exists(base + "/.depper/lock") {
		t.Error("lock not released")
	}

	// Second deploy with a changed and an added secret.
	writeFile(t, filepath.Join(repo, ".env.prod"), "TOKEN=rotated-"+sentinel+"\nMULTI=\"line one\nline two\"\nNEW=1\n")
	r2 := deploy(t, e, DeployOptions{})
	if !r2.Env.Known || strings.Join(r2.Env.Changed, ",") != "TOKEN" || strings.Join(r2.Env.Added, ",") != "NEW" || len(r2.Env.Unchanged) != 2 {
		t.Errorf("second env summary = %+v", r2.Env)
	}
	if h.link(base+"/previous") != "releases/"+r1.Release || strings.TrimSpace(h.read(base+"/.depper/previous-transaction")) != "transactions/"+r2.Release {
		t.Error("previous release not recorded")
	}
	env2 := h.read(base + "/.depper/environment")

	// Third deploy fails its health check and is restored.
	if err := os.Remove(filepath.Join(repo, "healthy")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, ".env.prod"), "TOKEN=broken\n")
	res, err := e.Deploy(ctx, DeployOptions{})
	if err == nil || !strings.Contains(err.Error(), "rolled back") || !strings.Contains(err.Error(), "start_period") {
		t.Fatalf("failed deploy error = %v", err)
	}
	if res.Health == nil || res.Health.OK {
		t.Errorf("failed deploy health = %+v", res.Health)
	}
	if h.link(base+"/current") != "releases/"+r2.Release || h.link(base+"/previous") != "releases/"+r1.Release {
		t.Error("current/previous not restored")
	}
	if h.read(base+"/.depper/environment") != env2 {
		t.Error("environment not restored")
	}
	if h.exists(base+"/releases/"+res.Release) || h.exists(base+"/.depper/transactions/"+res.Release) {
		t.Error("failed release or its backup was kept")
	}
	m2, _ := ParseManifest(h.read(base + "/.depper/environment.manifest"))
	if m2.FileSHA256 != SHA256Hex([]byte(env2)) {
		t.Error("manifest not restored with the environment")
	}
	writeFile(t, filepath.Join(repo, "healthy"), "")

	// Rollback to the previous release restores its environment.
	rb, err := e.Rollback(ctx, "")
	if err != nil {
		t.Fatalf("rollback: %v\n%s", err, e.Out)
	}
	if rb.From != r2.Release || rb.To != r1.Release || h.link(base+"/current") != "releases/"+r1.Release || h.link(base+"/previous") != "releases/"+r2.Release {
		t.Errorf("rollback = %+v", rb)
	}
	if !strings.Contains(h.read(base+"/.depper/environment"), `TOKEN="`+sentinel+`"`) {
		t.Error("rollback did not restore the release's environment")
	}
	// Rollback --to the newer release.
	if _, err := e.Rollback(ctx, r2.Release); err != nil {
		t.Fatalf("rollback --to: %v", err)
	}
	if h.link(base+"/current") != "releases/"+r2.Release || h.read(base+"/.depper/environment") != env2 {
		t.Error("rollback --to did not restore release 2")
	}
	if _, err := e.Rollback(ctx, r2.Release); err == nil || !strings.Contains(err.Error(), "already the current") {
		t.Errorf("rollback to current = %v", err)
	}
	if _, err := e.Rollback(ctx, "release-20000101T000000Z-deadbeef"); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("rollback to missing = %v", err)
	}
	entries, _ := os.ReadDir(h.path(base + "/.depper/transactions"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "rollback-") {
			t.Errorf("rollback transaction left behind: %s", entry.Name())
		}
	}

	// Releases listing and status.
	l, err := e.Probe(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.ListReleases(ctx, l)
	if err != nil || len(list.Releases) != 2 || list.Releases[0].ID != r2.Release || !list.Releases[0].Current || !list.Releases[1].Previous || !list.Releases[1].RollbackAvailable {
		t.Fatalf("releases = %+v (%v)", list, err)
	}
	st, err := e.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Deployed || st.ActiveState != "active" || st.CurrentRelease != r2.Release || st.Release == nil || st.Release.ID != r2.Release || st.EndpointURL != "http://203.0.113.10:8080" || st.Health == nil || !st.Health.OK {
		t.Errorf("status = %+v", st)
	}

	// Pruning with keep_releases = 2: two more deploys drop release 1 and
	// its transaction backup.
	r3 := deploy(t, e, DeployOptions{})
	r4 := deploy(t, e, DeployOptions{})
	if h.exists(base+"/releases/"+r1.Release) || h.exists(base+"/.depper/transactions/"+r1.Release) {
		t.Error("release 1 was not pruned")
	}
	if !h.exists(base+"/releases/"+r3.Release) || !h.exists(base+"/releases/"+r4.Release) || h.exists(base+"/releases/"+r2.Release) {
		t.Errorf("unexpected releases after pruning (pruned %v, %v)", r3.Pruned, r4.Pruned)
	}

	// Security properties across every command run.
	for _, argv := range h.Exec.Argv() {
		if strings.Contains(argv, sentinel) {
			t.Errorf("secret in argv: %s", argv)
		}
	}
	if strings.Contains(h.Out.String(), sentinel) || strings.Contains(h.log("rsync-args"), sentinel) {
		t.Error("secret leaked into output or rsync arguments")
	}
	if !strings.Contains(h.log("rsync-args"), "--exclude\n/.env.prod") {
		t.Error("the secrets file is not excluded from rsync")
	}
	for _, script := range h.Exec.Scripts() {
		if strings.Contains(script, sentinel) {
			t.Error("raw secret value in a remote script (must be base64 on stdin only)")
		}
	}
	checkScriptSyntax(t, h.Exec.Scripts())
}

func TestFirstDeployFailureRemovesUnitAndCurrent(t *testing.T) {
	h := newFakeHost(t)
	repo := processRepo(t)
	if err := os.Remove(filepath.Join(repo, "healthy")); err != nil {
		t.Fatal(err)
	}
	e := h.Engine(loadConfig(t, repo))
	_, err := e.Deploy(context.Background(), DeployOptions{})
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if h.exists(base+"/current") || h.exists(".config/systemd/user/depper-api.service") || h.exists(base+"/.depper/environment") {
		t.Error("first failed deploy left current, unit, or environment behind")
	}
	if !strings.Contains(h.log("systemctl.log"), "disable --now depper-api.service") {
		t.Error("service was not disabled and stopped")
	}
}

func TestDeployWithoutHealthUsesUnitState(t *testing.T) {
	h := newFakeHost(t)
	toml := strings.Split(processToml, "[health]")[0]
	repo := writeRepo(t, toml, map[string]string{"app.py": "x\n", ".env.prod": "A=1\n"})
	e := h.Engine(loadConfig(t, repo))
	res := deploy(t, e, DeployOptions{})
	if res.Health.Check != CheckUnit || !res.Health.OK {
		t.Errorf("health = %+v", res.Health)
	}
	// A crashing unit fails the deploy and restores.
	h.touch("../systemd/depper-api.service.crash")
	if _, err := e.Deploy(context.Background(), DeployOptions{}); err == nil || !strings.Contains(err.Error(), "not active") {
		t.Errorf("crash err = %v", err)
	}
	if h.link(base+"/current") != "releases/"+res.Release {
		t.Error("current not restored after crash")
	}
}

func TestSecretsPushAndRestore(t *testing.T) {
	h := newFakeHost(t)
	repo := processRepo(t)
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()
	r1 := deploy(t, e, DeployOptions{})

	env, err := LoadEnvironment(e.Config, EnvSource{Stdin: strings.NewReader("TOKEN=pushed-" + sentinel + "\n")})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.PushEnvironment(ctx, env)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.Release != r1.Release || strings.Join(res.Env.Changed, ",") != "TOKEN" || strings.Join(res.Env.Removed, ",") != "MULTI" || !res.Health.OK {
		t.Errorf("push result = %+v", res)
	}
	pushed := h.read(base + "/.depper/environment")
	if !strings.Contains(pushed, "pushed-"+sentinel) || h.read(base+"/.depper/transactions/"+r1.Release+"/applied/environment") != pushed {
		t.Error("environment or applied snapshot not updated")
	}
	diff, err := e.DiffEnvironment(ctx, mustProbe(t, e), env.Values)
	if err != nil || diff.State != SecretsInSync {
		t.Errorf("diff after push = %+v (%v)", diff, err)
	}
	// Editing the live file outside depper is detected.
	if err := os.WriteFile(h.path(base+"/.depper/environment"), []byte(pushed+"X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diff, _ = e.DiffEnvironment(ctx, mustProbe(t, e), env.Values)
	if diff.State != SecretsDrift || !diff.FileModified {
		t.Errorf("diff after manual edit = %+v", diff)
	}
	if err := os.WriteFile(h.path(base+"/.depper/environment"), []byte(pushed), 0o600); err != nil {
		t.Fatal(err)
	}

	// A push that breaks health is restored.
	h.touch("unhealthy")
	bad, _ := LoadEnvironment(e.Config, EnvSource{Stdin: strings.NewReader("TOKEN=bad\n")})
	if _, err := e.PushEnvironment(ctx, bad); err == nil || !strings.Contains(err.Error(), "previous environment was restored") {
		t.Fatalf("bad push err = %v", err)
	}
	if h.read(base+"/.depper/environment") != pushed {
		t.Error("environment not restored after failed push")
	}
	entries, _ := os.ReadDir(h.path(base + "/.depper/transactions"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "secrets-") {
			t.Errorf("secrets transaction left behind: %s", entry.Name())
		}
	}
	checkScriptSyntax(t, h.Exec.Scripts())
}

func mustProbe(t *testing.T, e *Engine) *Layout {
	t.Helper()
	l, err := e.Probe(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLockAndUnlock(t *testing.T) {
	h := newFakeHost(t)
	repo := processRepo(t)
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()
	deploy(t, e, DeployOptions{})
	l := mustProbe(t, e)

	other := h.Engine(e.Config)
	other.Holder, other.Command, other.PID = "ci@runner", "deploy", 77
	if err := other.Lock(ctx, l); err != nil {
		t.Fatal(err)
	}
	_, err := e.Deploy(ctx, DeployOptions{})
	var held *LockHeldError
	if !errors.As(err, &held) || held.Info == nil || held.Info.Holder != "ci@runner" || held.Info.PID != 77 {
		t.Fatalf("deploy under lock = %v", err)
	}
	if !strings.Contains(err.Error(), "depper unlock") {
		t.Errorf("message = %v", err)
	}
	if _, err := e.BreakLock(ctx, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("young lock removed without force: %v", err)
	}
	res, err := e.BreakLock(ctx, true)
	if err != nil || !res.Removed || h.exists(base+"/.depper/lock") {
		t.Fatalf("force unlock = %+v, %v", res, err)
	}
	if res, err := e.BreakLock(ctx, false); err != nil || res.Removed {
		t.Errorf("unlock without lock = %+v, %v", res, err)
	}
	// Staleness uses the host's clock and the lock directory's mtime: a
	// client clock far in the future does not make a fresh lock stale...
	if err := other.Lock(ctx, l); err != nil {
		t.Fatal(err)
	}
	e.Now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if _, err := e.BreakLock(ctx, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("client clock skew made a fresh lock stale: %v", err)
	}
	// ...while a lock not refreshed for StaleLockAge is removed without
	// --force.
	old := time.Now().Add(-StaleLockAge - time.Minute)
	if err := os.Chtimes(h.path(base+"/.depper/lock"), old, old); err != nil {
		t.Fatal(err)
	}
	res, err = e.BreakLock(ctx, false)
	if err != nil || !res.Removed || !res.Lock.Stale || res.Lock.AgeSeconds < int64(StaleLockAge/time.Second) {
		t.Errorf("stale unlock = %+v, %v", res, err)
	}
}

// Unlock removes only the lock this engine took (by token) and fails
// loudly otherwise; BreakLock removes only the lock it inspected.
func TestLockOwnershipToken(t *testing.T) {
	h := newFakeHost(t)
	e := h.Engine(loadConfig(t, processRepo(t)))
	ctx := context.Background()
	deploy(t, e, DeployOptions{})
	l := mustProbe(t, e)

	a := h.Engine(e.Config)
	if err := a.Lock(ctx, l); err != nil {
		t.Fatal(err)
	}
	info := h.read(base + "/.depper/lock/info")
	if !strings.Contains(info, `"token":"`+a.held.token+`"`) || len(a.held.token) != 32 {
		t.Fatalf("lock info = %s", info)
	}
	// Someone breaks the lock and another operation takes it.
	if _, err := e.BreakLock(ctx, true); err != nil {
		t.Fatal(err)
	}
	b := h.Engine(e.Config)
	b.Holder = "other@host"
	if err := b.Lock(ctx, l); err != nil {
		t.Fatal(err)
	}
	err := a.Unlock(ctx, l)
	if err == nil || !strings.Contains(err.Error(), "now held by someone else") {
		t.Fatalf("unlock of a lock taken over = %v", err)
	}
	if !strings.Contains(h.read(base+"/.depper/lock/info"), "other@host") {
		t.Error("the other operation's lock was removed")
	}
	// BreakLock refuses when the lock changed hands after it was read.
	if err := e.Remote.Run(ctx, releaseScript(l.Lock(), strings.Repeat("0", 32))); err == nil {
		t.Errorf("release with a stale token = %v", err)
	}
	if !h.exists(base + "/.depper/lock/info") {
		t.Fatal("a release with the wrong token removed the lock")
	}
	if err := b.Unlock(ctx, l); err != nil || h.exists(base+"/.depper/lock") {
		t.Fatalf("owner unlock = %v", err)
	}
	// The lock vanished entirely.
	if err := a.Lock(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(h.path(base + "/.depper/lock")); err != nil {
		t.Fatal(err)
	}
	if err := a.Unlock(ctx, l); err == nil || !strings.Contains(err.Error(), "removed by someone else") {
		t.Errorf("unlock of a removed lock = %v", err)
	}
	checkScriptSyntax(t, h.Exec.Scripts())
}

// A held lock is refreshed (its directory touched) while the operation
// runs, so a long step never makes it look stale.
func TestWithLockRefreshesTheLock(t *testing.T) {
	h := newFakeHost(t)
	e := h.Engine(loadConfig(t, processRepo(t)))
	ctx := context.Background()
	deploy(t, e, DeployOptions{})
	l := mustProbe(t, e)
	e.LockRefresh = 50 * time.Millisecond
	lockDir := h.path(base + "/.depper/lock")
	err := e.WithLock(ctx, l, func() error {
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(lockDir, old, old); err != nil {
			return err
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if st, err := os.Stat(lockDir); err == nil && time.Since(st.ModTime()) < time.Minute {
				return nil
			}
			time.Sleep(20 * time.Millisecond)
		}
		return errors.New("the lock was not refreshed")
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.exists(base + "/.depper/lock") {
		t.Error("lock not released")
	}
	// Remote setup scripts also keep the lock fresh from the host itself.
	e.held = &heldLock{path: l.Lock(), token: strings.Repeat("a", 32)}
	lines := strings.Join(e.lockKeepaliveLines(), "\n")
	e.held = nil
	for _, want := range []string{"kill -0 $$", `touch "$depper_lock"`, `"token":"` + strings.Repeat("a", 32) + `"`, "trap"} {
		if !strings.Contains(lines, want) {
			t.Errorf("keepalive lines lack %q:\n%s", want, lines)
		}
	}
}

func TestGitSourceShipsOnlyCommittedContent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	h := newFakeHost(t)
	toml := strings.Replace(processToml, "keep_releases = 2", "keep_releases = 2\nsource = \"git\"", 1)
	repo := processRepo(t)
	writeFile(t, filepath.Join(repo, "depper.toml"), toml)
	writeFile(t, filepath.Join(repo, ".gitignore"), ".env.prod\n")
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "init")
	head := git("rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "uncommitted.txt"), "dirty\n")
	e := h.Engine(loadConfig(t, repo))
	res := deploy(t, e, DeployOptions{})
	rel := base + "/releases/" + res.Release
	if h.exists(rel+"/uncommitted.txt") || !h.exists(rel+"/app.py") || h.exists(rel+"/.env.prod") {
		t.Error("git source shipped the wrong files")
	}
	if res.Metadata.Source != "git" || res.Metadata.GitCommit != head || res.Metadata.GitRef != "HEAD" || res.Metadata.GitDirty != nil {
		t.Errorf("metadata = %+v", res.Metadata)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "uncommitted") {
		t.Errorf("notes = %q", res.Notes)
	}
	// check: up to date, then outdated after a new commit.
	check, err := e.Check(context.Background(), "", EnvSource{})
	if err != nil || check.State != CodeUpToDate || check.Secrets.State != SecretsInSync || check.Drift {
		t.Fatalf("check = %+v secrets %+v (%v)", check, check.Secrets, err)
	}
	git("add", "uncommitted.txt")
	git("commit", "-q", "-m", "second")
	check, err = e.Check(context.Background(), "", EnvSource{})
	if err != nil || check.State != CodeOutdated || !check.Drift {
		t.Errorf("check after commit = %+v (%v)", check, err)
	}
	// --ref deploys a specific ref.
	res2 := deploy(t, e, DeployOptions{Ref: head})
	if res2.Metadata.GitCommit != head || h.exists(base+"/releases/"+res2.Release+"/uncommitted.txt") {
		t.Error("--ref did not deploy the requested commit")
	}
}

func TestDryRunNeverExecutesAndHidesSecrets(t *testing.T) {
	h := newFakeHost(t)
	repo := processRepo(t)
	e := h.Engine(loadConfig(t, repo))
	res, err := e.Deploy(context.Background(), DeployOptions{DryRun: true})
	if err != nil || !res.DryRun {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	for _, argv := range h.Exec.Argv() {
		if !strings.HasPrefix(argv, "git ") {
			t.Errorf("dry run executed %q", argv)
		}
	}
	out := h.Out.String()
	if strings.Contains(out, sentinel) || !strings.Contains(out, "DRY RUN: deploy api to deploy@fakehost:~/.local/share/depper/api") || !strings.Contains(out, "variables=2") {
		t.Errorf("dry run output = %q", out)
	}
}

func TestFailedSetupLeavesTheActiveReleaseUntouched(t *testing.T) {
	h := newFakeHost(t)
	repo := processRepo(t)
	e := h.Engine(loadConfig(t, repo))
	r1 := deploy(t, e, DeployOptions{})
	env1 := h.read(base + "/.depper/environment")
	restarts := strings.Count(h.log("systemctl.log"), "restart depper-api.service")

	e.Config.Deploy.Setup = []string{"echo setup failed >&2; exit 3"}
	writeFile(t, filepath.Join(repo, ".env.prod"), "TOKEN=next\n")
	res, err := e.Deploy(context.Background(), DeployOptions{})
	if err == nil || !strings.Contains(err.Error(), "setup command failed") || strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if h.link(base+"/current") != "releases/"+r1.Release || h.read(base+"/.depper/environment") != env1 {
		t.Error("a failed setup changed the active release or environment")
	}
	if strings.Count(h.log("systemctl.log"), "restart depper-api.service") != restarts {
		t.Error("the service was restarted although activation never began")
	}
	if h.exists(base+"/releases/"+res.Release) || h.exists(base+"/.depper/transactions/"+res.Release) || h.exists(base+"/.depper/lock") {
		t.Error("failed release, its backup, or the lock was left behind")
	}
	entries, _ := os.ReadDir(h.path(base + "/.depper"))
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".next-") {
			t.Errorf("candidate left behind: %s", entry.Name())
		}
	}
}
