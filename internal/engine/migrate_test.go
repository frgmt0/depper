package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	legacyR1   = "release-20250101T000000Z-00000001"
	legacyR2   = "release-20250102T000000Z-00000002"
	tunnelUUID = "6f1d3c1e-2b9a-4c55-9a51-0d2f2e7f3a10"
)

func legacyUnit(base, restart string) string {
	return "[Unit]\nDescription=deployer: api\nAfter=network.target\n\n[Service]\nType=simple\nWorkingDirectory=" + base + "/current\nEnvironmentFile=" + base + "/.deployer/environment\nExecStart=/bin/sh \"" + base + "/current/.deployer-start.sh\"\nRestart=" + restart + "\nRestartSec=3\n\n[Install]\nWantedBy=default.target\n"
}

// buildLegacy writes a deployment exactly as the Python deployer left it
// (absolute links, .deployer metadata, deployer-* units) under rel.
func buildLegacy(t *testing.T, h *fakeHost, rel string) string {
	t.Helper()
	legacy := h.path(rel)
	for _, id := range []string{legacyR1, legacyR2} {
		writeFile(t, filepath.Join(legacy, "releases", id, "app.py"), id+"\n")
		writeFile(t, filepath.Join(legacy, "releases", id, "healthy"), "")
		writeFile(t, filepath.Join(legacy, "releases", id, ".deployer-start.sh"), "#!/bin/sh\nexec python3 app.py\n")
	}
	meta := filepath.Join(legacy, ".deployer")
	writeFile(t, filepath.Join(meta, "environment"), "TOKEN=\"two\"\n")
	writeFile(t, filepath.Join(meta, "previous-transaction"), meta+"/transactions/"+legacyR2+"\n")
	writeFile(t, filepath.Join(meta, "transactions", legacyR2, "environment"), "TOKEN=\"one\"\n")
	writeFile(t, filepath.Join(meta, "transactions", legacyR2, "unit"), legacyUnit(legacy, "always"))
	writeFile(t, filepath.Join(meta, "transactions", legacyR2, "current"), legacy+"/releases/"+legacyR1+"\n")
	for _, marker := range []string{"no-environment", "no-unit", "no-current"} {
		writeFile(t, filepath.Join(meta, "transactions", legacyR1, marker), "")
	}
	writeFile(t, filepath.Join(meta, "tunnel.json"), `{"hostname": "api.demo.example.com", "id": "`+tunnelUUID+`", "name": "deployer-api", "service": "http://127.0.0.1:8080"}`+"\n")
	writeFile(t, filepath.Join(meta, "cloudflared.yml"), "old\n")
	for link, target := range map[string]string{filepath.Join(legacy, "current"): legacy + "/releases/" + legacyR2, filepath.Join(meta, "previous"): legacy + "/releases/" + legacyR1} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	units := h.path(".config/systemd/user")
	writeFile(t, filepath.Join(units, "deployer-api.service"), legacyUnit(legacy, "on-failure"))
	writeFile(t, filepath.Join(units, "deployer-tunnel@api.service"), "[Unit]\nDescription=deployer managed Cloudflare Tunnel: api\n[Service]\nExecStart=\"/usr/bin/cloudflared\" --no-autoupdate --config \""+meta+"/cloudflared.yml\" tunnel run "+tunnelUUID+"\n")
	for _, f := range []string{"deployer-api.service.enabled", "deployer-api.service.active", "deployer-tunnel@api.service.enabled", "deployer-tunnel@api.service.active"} {
		h.touch("../systemd/" + f)
	}
	h.writeExec("remote-bin/cloudflared", "#!/bin/sh\nexit 0\n")
	return legacy
}

func TestMigrateDefaultLayoutAndRollbackAfterwards(t *testing.T) {
	h := newFakeHost(t)
	repo := processRepo(t)
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()
	legacy := buildLegacy(t, h, ".local/share/deployer/api")

	if _, err := e.Status(ctx); err == nil || !strings.Contains(err.Error(), "depper migrate") {
		t.Fatalf("status on legacy layout = %v", err)
	}
	if _, err := e.Deploy(ctx, DeployOptions{}); err == nil || !strings.Contains(err.Error(), "depper migrate") {
		t.Fatalf("deploy on legacy layout = %v", err)
	}
	if h.exists(base) {
		t.Fatal("a refused deploy created the depper layout")
	}

	plan, err := e.Migrate(ctx, true)
	if err != nil || plan.Status != "planned" || len(plan.Plan) < 8 || plan.LegacyBase != legacy {
		t.Fatalf("dry run = %+v (%v)", plan, err)
	}
	if h.exists(base) || !h.exists(".local/share/deployer/api/.deployer") {
		t.Fatal("dry run changed the host")
	}

	res, err := e.Migrate(ctx, false)
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, h.Out)
	}
	if res.Status != "migrated" || res.Base != h.path(base) || res.Health == nil || !res.Health.OK {
		t.Errorf("result = %+v", res)
	}
	newBase := h.path(base)
	if h.exists(".local/share/deployer/api") || !h.exists(base+"/.depper") || h.exists(base+"/.deployer") {
		t.Error("base not moved / metadata not renamed")
	}
	if h.link(base+"/current") != "releases/"+legacyR2 || h.link(base+"/previous") != "releases/"+legacyR1 || h.exists(base+"/.depper/previous") {
		t.Error("current/previous not converted to relative links")
	}
	if h.read(base+"/.depper/previous-transaction") != "transactions/"+legacyR2+"\n" {
		t.Errorf("previous-transaction = %q", h.read(base+"/.depper/previous-transaction"))
	}
	tx := base + "/.depper/transactions/" + legacyR2
	if h.read(tx+"/current") != "releases/"+legacyR1+"\n" {
		t.Errorf("tx current = %q", h.read(tx+"/current"))
	}
	if txUnit := h.read(tx + "/unit"); !strings.Contains(txUnit, "Restart=always") || !strings.Contains(txUnit, newBase+"/current/.depper-start.sh") || strings.Contains(txUnit, "deployer") {
		t.Errorf("tx unit not rewritten:\n%s", txUnit)
	}
	for _, id := range []string{legacyR1, legacyR2} {
		if !h.exists(base + "/releases/" + id + "/" + StartScriptName) {
			t.Errorf("%s missing in %s", StartScriptName, id)
		}
	}
	unitDir := ".config/systemd/user/"
	if h.exists(unitDir+"deployer-api.service") || h.exists(unitDir+"deployer-tunnel@api.service") {
		t.Error("legacy units not removed")
	}
	if app := h.read(unitDir + "depper-api.service"); !strings.Contains(app, "EnvironmentFile="+newBase+"/.depper/environment") || !strings.Contains(app, "Restart=on-failure") {
		t.Errorf("app unit:\n%s", app)
	}
	tunnelUnit := h.read(unitDir + "depper-tunnel@api.service")
	if !strings.Contains(tunnelUnit, TunnelUnitMarker("api")) || !strings.Contains(tunnelUnit, `--config "`+newBase+`/.depper/cloudflared.yml" tunnel run `+tunnelUUID) || !strings.Contains(tunnelUnit, h.Root+"/remote-bin/cloudflared") {
		t.Errorf("tunnel unit:\n%s", tunnelUnit)
	}
	if !strings.Contains(h.read(base+"/.depper/tunnel.json"), `"name": "deployer-api"`) {
		t.Error("tunnel.json changed")
	}
	if yml := h.read(base + "/.depper/cloudflared.yml"); !strings.Contains(yml, h.Home+"/.cloudflared/"+tunnelUUID+".json") || !strings.Contains(yml, "api.demo.example.com") {
		t.Errorf("cloudflared.yml:\n%s", yml)
	}
	for _, f := range []string{"depper-api.service.active", "depper-api.service.enabled", "depper-tunnel@api.service.active", "depper-tunnel@api.service.enabled"} {
		if !h.exists("../systemd/" + f) {
			t.Errorf("systemd state %s missing", f)
		}
	}
	if h.exists("../systemd/deployer-api.service.enabled") || h.exists("../systemd/deployer-api.service.active") {
		t.Error("legacy unit still enabled/active")
	}
	if h.exists(base+"/.depper/lock") || !h.exists(tx+"/applied/environment") {
		t.Error("lock left behind or applied snapshot missing")
	}

	// Other commands work; rollback uses the converted transaction backup.
	if st, err := e.Status(ctx); err != nil || st.CurrentRelease != legacyR2 {
		t.Fatalf("status after migrate = %+v (%v)", st, err)
	}
	if _, err := e.Rollback(ctx, ""); err != nil {
		t.Fatalf("rollback after migrate: %v", err)
	}
	if h.link(base+"/current") != "releases/"+legacyR1 || h.read(base+"/.depper/environment") != "TOKEN=\"one\"\n" || !strings.Contains(h.read(unitDir+"depper-api.service"), "Restart=always") {
		t.Error("rollback did not restore the previous release's configuration")
	}
	if _, err := e.Rollback(ctx, ""); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	if h.link(base+"/current") != "releases/"+legacyR2 || h.read(base+"/.depper/environment") != "TOKEN=\"two\"\n" {
		t.Error("rolling forward did not restore the migrated configuration")
	}

	again, err := e.Migrate(ctx, false)
	if err != nil || again.Status != "already-migrated" {
		t.Errorf("second migrate = %+v (%v)", again, err)
	}
	writeFile(t, h.path(".local/share/deployer/api/.deployer/environment"), "")
	if _, err := e.Migrate(ctx, false); err == nil || !strings.Contains(err.Error(), "both") {
		t.Errorf("both layouts = %v", err)
	}
	checkScriptSyntax(t, h.Exec.Scripts())
}

func TestMigrateCustomPathInPlaceAndRefusals(t *testing.T) {
	h := newFakeHost(t)
	custom := h.path("srv/api")
	toml := strings.Replace(processToml, "keep_releases = 2", "keep_releases = 2\npath = \""+custom+"\"", 1)
	repo := writeRepo(t, toml, map[string]string{"app.py": "", "healthy": "", ".env.prod": "A=1\n"})
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()
	buildLegacy(t, h, "srv/api")

	h.touch("srv/api/.deployer/lock/info")
	if _, err := e.Migrate(ctx, false); err == nil || !strings.Contains(err.Error(), "stale lock") {
		t.Errorf("legacy lock = %v", err)
	}
	if err := os.RemoveAll(h.path("srv/api/.deployer/lock")); err != nil {
		t.Fatal(err)
	}
	res, err := e.Migrate(ctx, false)
	if err != nil || res.Base != custom || res.LegacyBase != custom {
		t.Fatalf("migrate = %+v (%v)", res, err)
	}
	if !h.exists("srv/api/.depper") || h.exists("srv/api/.deployer") || h.link("srv/api/current") != "releases/"+legacyR2 {
		t.Error("custom path not converted in place")
	}
	// The migrated tunnel is still owned, so deploying without [tunnel] is refused.
	if _, err := e.Deploy(ctx, DeployOptions{}); err == nil || !strings.Contains(err.Error(), "owns a public tunnel") {
		t.Errorf("deploy without [tunnel] = %v", err)
	}
	if err := os.Remove(h.path("srv/api/.depper/tunnel.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Deploy(ctx, DeployOptions{}); err != nil {
		t.Errorf("deploy after migrate: %v", err)
	}
}

func TestMigrateWithoutLegacyDeployment(t *testing.T) {
	h := newFakeHost(t)
	e := h.Engine(loadConfig(t, processRepo(t)))
	if _, err := e.Migrate(context.Background(), false); err == nil || !strings.Contains(err.Error(), "no Python deployer deployment") {
		t.Errorf("err = %v", err)
	}
}
