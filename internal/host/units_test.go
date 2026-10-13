package host

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frgmt0/depper/internal/config"
)

func TestEditConfigText(t *testing.T) {
	vals := []configValue{{"host", `"203.0.113.7"`}, {"user", `"deploy"`}, {"known_hosts", `".depper/known_hosts"`}}
	tests := []struct {
		name, table, in, want string
		changed               []bool
		wantErr               string
	}{
		{
			name:    "replace keeps indentation and comments, inserts the rest after the header",
			table:   "deploy",
			in:      "# top\n[deploy] # the host\n  host = 'old@x' # placeholder\n# user = \"bob\"\npath = \"/srv/app\"\n\n[env]\nhost = \"not this one\"\n",
			want:    "# top\n[deploy] # the host\nuser = \"deploy\"\nknown_hosts = \".depper/known_hosts\"\n  host = \"203.0.113.7\" # placeholder\n# user = \"bob\"\npath = \"/srv/app\"\n\n[env]\nhost = \"not this one\"\n",
			changed: []bool{true, true, true},
		},
		{
			name:    "values already present are left alone",
			table:   "deploy",
			in:      "[deploy]\nhost = \"203.0.113.7\"\nuser = 'deploy'\nknown_hosts = \".depper/known_hosts\"\n",
			want:    "[deploy]\nhost = \"203.0.113.7\"\nuser = 'deploy'\nknown_hosts = \".depper/known_hosts\"\n",
			changed: []bool{false, false, false},
		},
		{
			name:    "a missing target table is appended",
			table:   "targets.prod",
			in:      "[deploy]\nhost = \"a\"",
			want:    "[deploy]\nhost = \"a\"\n\n[targets.prod]\nhost = \"203.0.113.7\"\nuser = \"deploy\"\nknown_hosts = \".depper/known_hosts\"\n",
			changed: []bool{true, true, true},
		},
		{
			name:    "an existing quoted target header is updated",
			table:   "targets.prod",
			in:      "[deploy]\nhost = \"a\"\n\n[ targets . \"prod\" ]\nhost = \"b\"\n\n[targets.prod.env]\nuser = \"x\"\n",
			want:    "[deploy]\nhost = \"a\"\n\n[ targets . \"prod\" ]\nuser = \"deploy\"\nknown_hosts = \".depper/known_hosts\"\nhost = \"203.0.113.7\"\n\n[targets.prod.env]\nuser = \"x\"\n",
			changed: []bool{true, true, true},
		},
		{
			name:    "headers inside multi-line strings are ignored",
			table:   "deploy",
			in:      "[service]\ncommand = \"\"\"\n[deploy]\nhost = 1\n\"\"\"\n[deploy]\nhost = \"x\"\n",
			want:    "[service]\ncommand = \"\"\"\n[deploy]\nhost = 1\n\"\"\"\n[deploy]\nuser = \"deploy\"\nknown_hosts = \".depper/known_hosts\"\nhost = \"203.0.113.7\"\n",
			changed: []bool{true, true, true},
		},
		{name: "no deploy header", table: "deploy", in: "deploy.host = \"x\"\n", wantErr: "could not find a [deploy] table header"},
		{name: "unsafe value", table: "deploy", in: "[deploy]\nhost = \"\"\"\nx\"\"\"\n", wantErr: "cannot rewrite safely"},
		{name: "duplicate table", table: "deploy", in: "[deploy]\n[deploy]\n", wantErr: "more than once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changes, err := editConfigText(tt.in, tt.table, vals)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got\n%s\nwant\n%s", got, tt.want)
			}
			for i, c := range changes {
				if c.Changed != tt.changed[i] {
					t.Errorf("change %d = %+v", i, c)
				}
			}
		})
	}
}

func TestConfigChangeShowsOldAndNew(t *testing.T) {
	_, changes, err := editConfigText("[deploy]\nhost = \"deploy@203.0.113.10\"\n", "deploy", []configValue{{"host", `"1.2.3.4"`}, {"user", `"deploy"`}})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].String() != `[deploy] host: "deploy@203.0.113.10" -> "1.2.3.4"` || changes[1].String() != `[deploy] user = "deploy" (new)` {
		t.Errorf("changes = %v", changes)
	}
}

func writeRepoConfig(t *testing.T, body string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "depper.toml"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return dir
}

func noEnv(string) (string, bool) { return "", false }

func TestPlanConfigValidatesAndProtectsOtherTargets(t *testing.T) {
	// Writing base user would break a target whose host carries a user.
	repo := writeRepoConfig(t, "[service]\nname = \"app\"\ncommand = \"x\"\n\n[deploy]\nhost = \"a.example.com\"\n\n[targets.other]\nhost = \"ops@b.example.com\"\n")
	o := &Options{Host: "1.2.3.4", User: "deploy", Port: 22, KnownHosts: filepath.Join(repo, ".depper/known_hosts")}
	_, _, err := planConfig(repo, "", o, "", noEnv)
	if err == nil || !strings.Contains(err.Error(), `break target "other"`) {
		t.Fatalf("err = %v", err)
	}
	// Into the target itself it is fine.
	edit, cfg, err := planConfig(repo, "other", o, "", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Deploy.Host != "1.2.3.4" || cfg.Deploy.User != "deploy" || cfg.Deploy.KnownHosts != filepath.Join(repo, ".depper/known_hosts") {
		t.Errorf("cfg = %+v", cfg.Deploy)
	}
	if err := edit.apply(repo, "other", noEnv); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(repo, "depper.toml"))
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode not preserved: %v", info.Mode().Perm())
	}

	// No config file: refuse with a pointer to depper init.
	empty, _ := filepath.EvalSymlinks(t.TempDir())
	if _, _, err := planConfig(empty, "", o, "", noEnv); err == nil || !strings.Contains(err.Error(), "depper init") {
		t.Errorf("err = %v", err)
	}
}

func TestConfigApplyRestoresOnValidationFailure(t *testing.T) {
	body := "[service]\nname = \"app\"\ncommand = \"x\"\n\n[deploy]\nhost = \"a.example.com\"\n"
	repo := writeRepoConfig(t, body)
	o := &Options{Host: "1.2.3.4", User: "deploy", Port: 2200, KnownHosts: filepath.Join(repo, ".depper/known_hosts")}
	edit, _, err := planConfig(repo, "", o, "", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(edit.Edited, "port = 2200\n") {
		t.Errorf("edited = %s", edit.Edited)
	}
	// An environment override that cannot be used makes the reload fail.
	bad := func(k string) (string, bool) {
		if k == config.EnvKnownHostsKey {
			return "/tmp/with space", true
		}
		return "", false
	}
	err = edit.apply(repo, "", bad)
	if err == nil || !strings.Contains(err.Error(), "restored the original") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "depper.toml")); string(got) != body {
		t.Errorf("not restored:\n%s", got)
	}
}

func TestDisplayPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, tt := range []struct{ path, repo, want string }{
		{"/r/.depper/known_hosts", "/r", ".depper/known_hosts"},
		{"/elsewhere/kh", "/r", "/elsewhere/kh"},
		{filepath.Join(home, ".ssh", "k"), "/r", "~/.ssh/k"},
	} {
		if got := displayPath(tt.path, tt.repo); got != tt.want {
			t.Errorf("displayPath(%q, %q) = %q", tt.path, tt.repo, got)
		}
	}
}

func TestKnownHostsMatching(t *testing.T) {
	blob := fakeKeyBlob("ssh-ed25519", 32)
	if KnownHostsName("127.0.0.1", 22) != "127.0.0.1" || KnownHostsName("2001:db8::1", 2222) != "[2001:db8::1]:2222" {
		t.Error("KnownHostsName")
	}
	salt := randomBytes(20)
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte("[h.example]:2222"))
	hashed := "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	content := "# comment\nother ssh-ed25519 " + fakeKeyBlob("ssh-ed25519", 32) + "\n" +
		"a.example,h.example ssh-rsa " + fakeKeyBlob("ssh-rsa", 64) + "\n" +
		hashed + " ssh-ed25519 " + blob + "\n"
	keys, err := knownKeysFor(content, "[h.example]:2222")
	if err != nil || len(keys) != 1 || keys[0].Blob != blob {
		t.Fatalf("hashed lookup = %v %v", keys, err)
	}
	keys, _ = knownKeysFor(content, "h.example")
	if len(keys) != 1 || keys[0].Type != "ssh-rsa" {
		t.Errorf("pattern list lookup = %v", keys)
	}
	if _, err := knownKeysFor("@revoked h.example ssh-ed25519 "+blob+"\n", "h.example"); err == nil {
		t.Error("revoked key not reported")
	}

	scanned := []HostKey{{"ssh-ed25519", blob}, {"ssh-rsa", fakeKeyBlob("ssh-rsa", 64)}}
	if s, err := comparePins("f", "n", nil, scanned); err != nil || s != pinNew {
		t.Errorf("new: %v %v", s, err)
	}
	if s, err := comparePins("f", "n", []HostKey{{"ssh-ed25519", blob}}, scanned); err != nil || s != pinPinned {
		t.Errorf("pinned: %v %v", s, err)
	}
	var mm *HostKeyMismatchError
	if _, err := comparePins("f", "n", []HostKey{{"ssh-ed25519", fakeKeyBlob("ssh-ed25519", 32)}}, scanned); !errors.As(err, &mm) {
		t.Errorf("changed key: %v", err)
	}
	if _, err := comparePins("f", "n", []HostKey{{"ecdsa-sha2-nistp256", fakeKeyBlob("x", 8)}}, scanned); !errors.As(err, &mm) {
		t.Errorf("disjoint key types: %v", err)
	}
}

func TestParseKeyscan(t *testing.T) {
	ed, rsa := fakeKeyBlob("ssh-ed25519", 32), fakeKeyBlob("ssh-rsa", 64)
	keys, err := parseKeyscan("# 127.0.0.1:22 SSH-2.0-OpenSSH_9.6\n127.0.0.1 ssh-rsa " + rsa + "\n127.0.0.1 ssh-ed25519 " + ed + "\n127.0.0.1 ssh-ed25519 " + ed + "\n")
	if err != nil || len(keys) != 2 || keys[0].Type != "ssh-ed25519" || keys[1].Type != "ssh-rsa" {
		t.Fatalf("keys = %v err %v", keys, err)
	}
	if _, err := parseKeyscan("h ssh-ed25519 !!!\n"); err == nil {
		t.Error("invalid base64 accepted")
	}
}

// Fingerprints match `ssh-keygen -lf` for a real key.
func TestFingerprintMatchesSSHKeygen(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not available")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	out, err := exec.Command(keygen, "-lf", key+".pub").Output()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ReadPublicKey(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if fields := strings.Fields(string(out)); len(fields) < 2 || fields[1] != pub.Fingerprint() {
		t.Errorf("ssh-keygen says %q, depper %q", out, pub.Fingerprint())
	}
}

func TestPublicKeys(t *testing.T) {
	blob := fakeKeyBlob("ssh-ed25519", 32)
	k, err := ParsePublicKey("ssh-ed25519 " + blob + " me@laptop with spaces\n")
	if err != nil || k.Line() != "ssh-ed25519 "+blob+" me@laptop with spaces" {
		t.Fatalf("%v %q", err, k.Line())
	}
	for _, bad := range []string{
		// A private key header, assembled so no literal looks like a real key.
		"-----BEGIN OPENSSH " + "PRIVATE KEY-----\nabc\n",
		"command=\"x\" ssh-ed25519 " + blob,
		"ssh-ed25519 " + blob + "\nssh-ed25519 " + blob,
		"ssh-ed25519 not*base64",
		"ssh-ed25519 " + blob + " evil\x1b[2J",
	} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	home := t.TempDir()
	if _, err := DefaultKeyFile(home); err == nil || !strings.Contains(err.Error(), "ssh-keygen") {
		t.Errorf("err = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"id_rsa.pub", "id_ecdsa.pub"} {
		if err := os.WriteFile(filepath.Join(home, ".ssh", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if p, _ := DefaultKeyFile(home); filepath.Base(p) != "id_ecdsa.pub" {
		t.Errorf("default key = %s", p)
	}
	if PrivateKeyFor(filepath.Join(home, ".ssh", "id_ecdsa.pub")) != "" {
		t.Error("private key reported although absent")
	}
}

func TestValidation(t *testing.T) {
	for _, dest := range []string{"root@203.0.113.7", "admin@[2001:db8::1]", "ubuntu@host.example.com", "root@127.0.0.1"} {
		if _, _, err := ParseDestination(dest); err != nil {
			t.Errorf("%s: %v", dest, err)
		}
	}
	for _, dest := range []string{"203.0.113.7", "root@-oProxyCommand=x", "ro ot@h", "root@h;rm", "root@[h]", "@h"} {
		if _, _, err := ParseDestination(dest); err == nil {
			t.Errorf("%s accepted", dest)
		}
	}
	for _, u := range []string{"Deploy", "1deploy", "de ploy", "root", "a$(x)", strings.Repeat("a", 33)} {
		if ValidateUser(u) == nil {
			t.Errorf("user %q accepted", u)
		}
	}
	if ValidateUser("_svc-1") != nil || ValidateUser("deploy") != nil {
		t.Error("valid user rejected")
	}
	o := &Options{Admin: "root", Host: "h", User: "root", Container: ContainerNone, KnownHosts: "/k"}
	if err := o.Validate(); err == nil {
		t.Error("root deploy user accepted")
	}
	o = &Options{Admin: "deploy", Host: "h", User: "deploy", Container: ContainerNone, KnownHosts: "/k"}
	if err := o.Validate(); err == nil {
		t.Error("deploy user equal to admin accepted")
	}
	o = &Options{Admin: "root", Host: "h", User: "deploy", Container: ContainerNone, KnownHosts: "/k", Target: "a]\n[x"}
	if err := o.Validate(); err == nil {
		t.Error("unsafe target name accepted")
	}
	if ValidateTailscaleKey("tskey with space") == nil || ValidateTailscaleKey("") == nil {
		t.Error("bad tailscale key accepted")
	}
	s := NewSecret("tskey" + "-abc") // assembled: no token-shaped literal
	if got := strings.Join([]string{s.String(), func() string { b, _ := s.MarshalJSON(); return string(b) }()}, " "); strings.Contains(got, "tskey"+"-abc") {
		t.Error("secret printed")
	}
}

func TestClassify(t *testing.T) {
	for _, tt := range []struct{ id, like, want string }{
		{"debian", "", FamilyDebian}, {"ubuntu", "debian", FamilyDebian}, {"linuxmint", "ubuntu debian", FamilyDebian},
		{"fedora", "", FamilyFedora}, {"rocky", "rhel centos fedora", FamilyFedora}, {"almalinux", "", FamilyFedora},
		{"ol", "fedora", FamilyFedora}, {"arch", "", ""}, {"alpine", "", ""}, {"opensuse-leap", "suse opensuse", ""},
	} {
		if got := classify(tt.id, tt.like); got != tt.want {
			t.Errorf("classify(%q, %q) = %q", tt.id, tt.like, got)
		}
	}
}

// The facts script and the root wrapper are valid POSIX sh, and every
// interpolated value is quoted.
func TestScriptsAreQuoted(t *testing.T) {
	r := &Runner{}
	o := &Options{User: "deploy"}
	key := PublicKey{Type: "ssh-ed25519", Blob: fakeKeyBlob("ssh-ed25519", 32), Comment: "it's me"}
	script, err := r.factsScript(o, key)
	if err != nil {
		t.Fatal(err)
	}
	checkScriptSyntax(t, []string{script})
	if !strings.Contains(script, "depper_asroot <<'"+rootHeredoc+"'") || !strings.Contains(script, "sudo -n sh -s") {
		t.Error("root wrapper missing")
	}
	if _, err := r.asRoot([]string{"echo hi", rootHeredoc}); err == nil {
		t.Error("delimiter inside the body accepted")
	}
}
