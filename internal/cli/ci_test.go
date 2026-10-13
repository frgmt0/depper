package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frgmt0/depper/internal/ci"
)

func TestCIInitWritesWorkflowAndRefusesOverwrite(t *testing.T) {
	toml := cliToml + "[health]\nurl = \"http://127.0.0.1:8080/health\"\n"
	repo := repoWith(t, toml, nil)
	app, stdout, stderr := testApp(nil)
	if code := Run(context.Background(), app, []string{"ci", "init", repo}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	path := filepath.Join(repo, ".github", "workflows", "depper.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"group: depper-default", `branches: ["main"]`, "--env-stdin", "run: depper health\n", "go install github.com/frgmt0/depper/cmd/depper@main"} {
		if !strings.Contains(text, want) {
			t.Errorf("workflow lacks %q:\n%s", want, text)
		}
	}
	for _, want := range []string{"Created " + path, "DEPPER_SSH_KEY", "DEPPER_KNOWN_HOSTS", "DEPPER_ENV", "ssh-keyscan -p 22 server"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}

	// A second run refuses without --force and leaves the file untouched.
	if err := os.WriteFile(path, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, _, stderr = testApp(nil)
	if code := Run(context.Background(), app, []string{"ci", "init", repo}); code != 1 || !strings.Contains(stderr.String(), "--force") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if got, _ := os.ReadFile(path); string(got) != "keep\n" {
		t.Errorf("file was overwritten: %q", got)
	}
	app, _, _ = testApp(nil)
	if code := Run(context.Background(), app, []string{"ci", "init", "--force", "--branch", "release", repo}); code != 0 {
		t.Fatalf("--force exit %d", code)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), `branches: ["release"]`) {
		t.Errorf("file was not regenerated: %q", got)
	}
}

func TestCIInitTargetWithoutSecretsOrHealth(t *testing.T) {
	toml := "[service]\nname = \"api\"\ncommand = \"./run\"\n[deploy]\n[targets.prod]\nhost = \"203.0.113.9\"\nport = 2222\n"
	repo := repoWith(t, toml, nil)
	app, stdout, stderr := testApp(nil)
	if code := Run(context.Background(), app, []string{"--target", "prod", "ci", "init", repo}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(repo, ci.WorkflowPath))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "group: depper-prod") || !strings.Contains(text, "run: depper deploy --target prod\n") {
		t.Errorf("workflow:\n%s", text)
	}
	if strings.Contains(text, "DEPPER_ENV") || strings.Contains(text, "depper health") {
		t.Errorf("workflow has secrets/health steps:\n%s", text)
	}
	if strings.Contains(stdout.String(), "DEPPER_ENV") || !strings.Contains(stdout.String(), "ssh-keyscan -p 2222 203.0.113.9") {
		t.Errorf("stdout:\n%s", stdout.String())
	}
}

func TestCIInitRejectsBadBranchWithoutWriting(t *testing.T) {
	repo := repoWith(t, cliToml, nil)
	app, _, _ := testApp(nil)
	if code := Run(context.Background(), app, []string{"ci", "init", "--branch", "a b", repo}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(repo, ".github")); !os.IsNotExist(err) {
		t.Errorf(".github exists: %v", err)
	}
}
