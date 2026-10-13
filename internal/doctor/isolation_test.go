package doctor

// PATH isolation for tests that run the doctor probe script locally. The
// script sees only "sysbin", a directory of symlinks to an explicit allowlist
// of basic utilities, so the machine's own docker, podman, curl, cloudflared,
// systemctl and journalctl are never found, whatever the CI runner has
// installed.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// remoteToolAllowlist is every real utility the remote scripts (or the mv and
// sha256sum shims) may use. Tools missing on this platform are skipped.
var remoteToolAllowlist = []string{
	"sh", "dash", "[", "test", "true", "false", "echo", "printf", "pwd", "expr", "env",
	"cat", "mkdir", "rmdir", "mv", "ln", "rm", "cp", "chmod", "readlink", "realpath", "ls",
	"date", "stat", "touch", "find", "xargs", "tee", "sleep", "timeout",
	"grep", "egrep", "fgrep", "sed", "awk", "head", "tail", "tr", "cut", "sort", "uniq", "wc",
	"basename", "dirname", "base64", "sha256sum", "shasum", "mktemp", "od", "diff", "cmp",
	"id", "uname", "hostname", "whoami", "kill", "perl", "df", "rsync",
}

// remoteGuardedTools are the tools that must only ever resolve to a fake.
var remoteGuardedTools = []string{"docker", "podman", "curl", "cloudflared", "systemctl", "journalctl"}

// setupSysBin creates root/sysbin holding symlinks to the allowlisted tools.
func setupSysBin(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "sysbin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range remoteToolAllowlist {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if err := os.Symlink(p, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// scriptEnv returns the environment for running the probe script: a
// throwaway HOME and a PATH of only the allowlisted utilities.
func scriptEnv(t *testing.T, home string) []string {
	t.Helper()
	root := t.TempDir()
	setupSysBin(t, root)
	return []string{"HOME=" + home, "PATH=" + filepath.Join(root, "sysbin")}
}

// resolveIn finds name the way a shell would on the given PATH.
func resolveIn(pathList, name string) string {
	for _, dir := range filepath.SplitList(pathList) {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// leakedTools returns the guarded tools that resolve outside binDir.
func leakedTools(pathList, binDir string) []string {
	var leaked []string
	for _, name := range remoteGuardedTools {
		if p := resolveIn(pathList, name); p != "" && filepath.Dir(p) != binDir {
			leaked = append(leaked, p)
		}
	}
	return leaked
}

// The probe script must not see real docker, podman, curl, cloudflared,
// systemctl or journalctl, even when they exist on the machine's PATH.
func TestScriptPathIsolation(t *testing.T) {
	scratch := t.TempDir()
	for _, name := range remoteGuardedTools {
		if err := os.WriteFile(filepath.Join(scratch, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", scratch+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The detector must see through a leaky PATH (otherwise this guard is blind).
	if got := leakedTools(os.Getenv("PATH"), "/nonexistent"); len(got) != len(remoteGuardedTools) {
		t.Fatalf("detector found %v, want all of %v", got, remoteGuardedTools)
	}

	env := scriptEnv(t, t.TempDir())
	path := strings.TrimPrefix(env[1], "PATH=")
	if leaked := leakedTools(path, "/nonexistent"); len(leaked) > 0 {
		t.Fatalf("real tools resolve through the probe PATH: %v", leaked)
	}

	script := "for t in " + strings.Join(remoteGuardedTools, " ") + "; do command -v $t || true; done"
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("shell resolved guarded tools: %s", out)
	}
}
