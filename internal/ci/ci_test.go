package ci

import (
	"strings"
	"testing"
)

func TestGenerateReleaseVersion(t *testing.T) {
	for _, version := range []string{"v0.2.0", "0.2.0"} {
		out, err := Generate(Options{Target: "prod", Branch: "main", Version: version, Secrets: true, Health: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			`branches: ["main"]`,
			"  workflow_dispatch:",
			"group: depper-prod",
			"cancel-in-progress: false",
			"actions/checkout@v4",
			"fetch-depth: 0",
			`DEPPER_VERSION: "0.2.0"`,
			"releases/download/v${DEPPER_VERSION}",
			"depper_${DEPPER_VERSION}_linux_amd64.tar.gz",
			"sha256sum -c --ignore-missing checksums.txt",
			"umask 077",
			`DEPPER_SSH_KEY_FILE=$dir/id" >> "$GITHUB_ENV"`,
			`DEPPER_KNOWN_HOSTS_FILE=$dir/known_hosts" >> "$GITHUB_ENV"`,
			"DEPPER_ENV: ${{ secrets.DEPPER_ENV }}",
			`printf '%s' "$DEPPER_ENV" | depper deploy --target prod --env-stdin`,
			"run: depper health --target prod",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: workflow lacks %q:\n%s", version, want, out)
			}
		}
		if strings.Contains(out, "setup-go") {
			t.Errorf("release workflow must not use setup-go")
		}
	}
}

func TestArchiveNameMatchesDownload(t *testing.T) {
	if got := ArchiveName("v0.2.0"); got != "depper_0.2.0_linux_amd64.tar.gz" {
		t.Errorf("ArchiveName = %q", got)
	}
}

func TestGenerateDevVersion(t *testing.T) {
	out, err := Generate(Options{Branch: "release/1", Version: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"actions/setup-go@v5",
		"go install github.com/frgmt0/depper/cmd/depper@main",
		"group: depper-default",
		"run: depper deploy\n",
		`branches: ["release/1"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("workflow lacks %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"sha256sum -c", "DEPPER_ENV", "depper health", "--target"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("workflow unexpectedly contains %q", unwanted)
		}
	}
}

func TestGenerateNeverInterpolatesSecretsIntoScripts(t *testing.T) {
	out, err := Generate(Options{Target: "t", Branch: "main", Version: "v1.0.0", Secrets: true, Health: true})
	if err != nil {
		t.Fatal(err)
	}
	inRun := false
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "run:"):
			inRun = true
			if strings.Contains(line, "${{") {
				t.Errorf("expression in run line: %s", line)
			}
		case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "env:") || strings.HasPrefix(trimmed, "with:"):
			inRun = false
		case inRun && strings.Contains(line, "${{"):
			t.Errorf("expression in run script: %s", line)
		}
	}
}

func TestGenerateStructure(t *testing.T) {
	out, err := Generate(Options{Target: "prod", Branch: "main", Version: "v0.2.0", Secrets: true, Health: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\t") {
		t.Error("workflow contains tabs")
	}
	if !strings.HasSuffix(out, "\n") || strings.HasSuffix(out, "\n\n") {
		t.Error("workflow must end with exactly one newline")
	}
	for _, line := range strings.Split(out, "\n") {
		if indent := len(line) - len(strings.TrimLeft(line, " ")); indent%2 != 0 && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Errorf("odd indentation: %q", line)
		}
	}
}

func TestGenerateValidation(t *testing.T) {
	for _, o := range []Options{
		{Branch: "", Version: "dev"},
		{Branch: "main\nrun: evil", Version: "dev"},
		{Branch: `ma"in`, Version: "dev"},
		{Branch: "a..b", Version: "dev"},
		{Branch: "main", Target: "a b", Version: "dev"},
		{Branch: "main", Target: "x\n", Version: "dev"},
		{Branch: "main", Version: "v1.0.0$(id)"},
		{Branch: "main", Version: "not-a-version"},
	} {
		if _, err := Generate(o); err == nil {
			t.Errorf("Generate(%+v) succeeded, want error", o)
		}
	}
}

func TestSecretsHelp(t *testing.T) {
	text := strings.Join(SecretsHelp("deploy@203.0.113.7", 2222, true), "\n")
	for _, want := range []string{"DEPPER_SSH_KEY", "DEPPER_KNOWN_HOSTS", "DEPPER_ENV", "ssh-keyscan -p 2222 203.0.113.7"} {
		if !strings.Contains(text, want) {
			t.Errorf("help lacks %q:\n%s", want, text)
		}
	}
	text = strings.Join(SecretsHelp("alias", 0, false), "\n")
	if strings.Contains(text, "DEPPER_ENV") || !strings.Contains(text, "ssh-keyscan -p 22 alias") {
		t.Errorf("help = %s", text)
	}
}
