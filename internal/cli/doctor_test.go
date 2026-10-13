package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/frgmt0/depper/internal/transport"
	"github.com/frgmt0/depper/internal/transport/transporttest"
)

const doctorFacts = "user=app\nbase=/home/app/.local/share/depper/api\nrsync=/usr/bin/rsync\nsha256sum=/usr/bin/sha256sum\n" +
	"systemctl=/usr/bin/systemctl\nsystemd=ok\nlinger=yes\ndisk_kb=52428800\nlayout=depper\nlock=free\ndoctor=1\n"

func TestDoctorJSONAndExitCode(t *testing.T) {
	repo := repoWith(t, cliToml, nil)
	app, stdout, stderr := testApp(nil)
	recorderOf(app).Handler = func(transporttest.Call) (transport.Result, error) {
		return transport.Result{Stdout: doctorFacts}, nil
	}
	if code := Run(context.Background(), app, []string{"doctor", "--json", repo}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var report struct {
		Checks []struct{ Name, Status, Detail string } `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if len(report.Checks) == 0 || report.Checks[0].Name != "ssh" {
		t.Fatalf("checks = %+v", report.Checks)
	}

	app, stdout, _ = testApp(nil)
	recorderOf(app).Handler = func(transporttest.Call) (transport.Result, error) {
		return transport.Result{Stdout: strings.Replace(doctorFacts, "rsync=/usr/bin/rsync", "rsync=", 1)}, nil
	}
	if code := Run(context.Background(), app, []string{"doctor", repo}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "FAIL  rsync") {
		t.Errorf("stdout:\n%s", stdout.String())
	}

	app, stdout, _ = testApp(nil)
	recorderOf(app).Handler = func(transporttest.Call) (transport.Result, error) {
		return transport.Result{}, &transport.CommandError{Program: "ssh", ExitCode: 255, Stderr: "Connection refused"}
	}
	if code := Run(context.Background(), app, []string{"doctor", repo}); code != 1 || !strings.Contains(stdout.String(), "Connection refused") {
		t.Fatalf("exit %d, stdout %q", code, stdout.String())
	}
}
