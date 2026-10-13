package tunnel

// PATH isolation for the fake remote host. The remote side of every fake
// host sees only its own bin directory (the fakes) and "sysbin", a directory
// of symlinks to an explicit allowlist of basic utilities. Real host tools
// the fakes stand in for (docker, podman, curl, cloudflared, systemctl,
// journalctl) therefore never resolve to the machine's own software, whatever
// the CI runner has installed.

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
	"id", "uname", "hostname", "whoami", "kill", "perl",
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

// remotePATH is the only PATH the fake remote host ever uses.
func remotePATH(root string) string {
	return filepath.Join(root, "remote-bin") + string(os.PathListSeparator) + filepath.Join(root, "sysbin")
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

// The fake host must not see real docker, podman, curl, cloudflared,
// systemctl or journalctl, even when they exist on the machine's PATH.
func TestFakeHostPathIsolation(t *testing.T) {
	// Put dummy copies of every guarded tool on the real PATH.
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

	h := newFakeHost(t)
	binDir := filepath.Join(h.Root, "remote-bin")
	path := remotePATH(h.Root)
	if leaked := leakedTools(path, binDir); len(leaked) > 0 {
		t.Fatalf("real tools resolve through the fake host PATH: %v", leaked)
	}

	// Ask a real shell with exactly the environment the fake ssh uses.
	script := "for t in " + strings.Join(remoteGuardedTools, " ") + "; do command -v $t || true; done"
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + path, "HOME=" + h.Home}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Fields(string(out)) {
		if filepath.Dir(line) != binDir {
			t.Errorf("shell resolved %s outside the fake bin dir", line)
		}
	}

	// Tools the fakes do not provide must be absent, not the dummies.
	for _, name := range []string{"podman", "curl", "cloudflared"} {
		if _, err := os.Stat(filepath.Join(binDir, name)); err == nil {
			continue
		}
		if p := resolveIn(path, name); p != "" {
			t.Errorf("%s resolves to %s but no fake is installed", name, p)
		}
	}
}
