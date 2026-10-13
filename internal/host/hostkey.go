package host

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/frgmt0/depper/internal/transport"
)

// HostKey is one SSH host public key.
type HostKey struct {
	Type string `json:"type"`
	// Blob is the base64 public key blob, as in known_hosts.
	Blob string `json:"-"`
}

// Fingerprint returns the key's SHA256 fingerprint in the format
// `ssh-keygen -lf` prints ("SHA256:<unpadded base64>").
func (k HostKey) Fingerprint() string {
	raw, err := base64.StdEncoding.DecodeString(k.Blob)
	if err != nil {
		return "invalid key"
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// hostKeyTypes are the key types depper accepts from ssh-keyscan.
var hostKeyTypes = map[string]bool{
	"ssh-ed25519": true, "ssh-rsa": true,
	"ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true, "ecdsa-sha2-nistp521": true,
	"sk-ssh-ed25519@openssh.com": true, "sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// KnownHostsName is the host field ssh looks up for addr and port: the bare
// address for port 22, "[addr]:port" otherwise.
func KnownHostsName(addr string, port int) string {
	if port == 0 || port == 22 {
		return addr
	}
	return "[" + addr + "]:" + strconv.Itoa(port)
}

// parseKeyscan parses ssh-keyscan output into keys, ignoring comments and the
// host field (depper writes its own canonical host name).
func parseKeyscan(out string) ([]HostKey, error) {
	var keys []HostKey
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("unexpected ssh-keyscan output line %q", clean(line))
		}
		k := HostKey{Type: fields[1], Blob: fields[2]}
		if !hostKeyTypes[k.Type] {
			continue
		}
		if _, err := base64.StdEncoding.DecodeString(k.Blob); err != nil {
			return nil, fmt.Errorf("ssh-keyscan returned an invalid %s key", k.Type)
		}
		if seen[k.Type+" "+k.Blob] {
			continue
		}
		seen[k.Type+" "+k.Blob] = true
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool { return keyRank(keys[i].Type) < keyRank(keys[j].Type) })
	return keys, nil
}

func keyRank(t string) int {
	switch {
	case t == "ssh-ed25519":
		return 0
	case strings.HasPrefix(t, "ecdsa"):
		return 1
	case t == "ssh-rsa":
		return 2
	}
	return 3
}

// knownKeysFor returns the keys recorded in known_hosts content for name,
// matching plain (comma-separated) and hashed (|1|salt|hash) host fields.
// It reports an error when a matching line is marked @revoked.
func knownKeysFor(content, name string) ([]HostKey, error) {
	var keys []HostKey
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		marker := ""
		if strings.HasPrefix(fields[0], "@") {
			marker, fields = fields[0], fields[1:]
		}
		if len(fields) < 3 || !hostFieldMatches(fields[0], name) {
			continue
		}
		switch marker {
		case "":
			keys = append(keys, HostKey{Type: fields[1], Blob: fields[2]})
		case "@revoked":
			return nil, fmt.Errorf("the known_hosts file marks a %s key for %s as @revoked", fields[1], name)
		}
	}
	return keys, sc.Err()
}

func hostFieldMatches(field, name string) bool {
	if strings.HasPrefix(field, "|1|") {
		parts := strings.Split(field, "|")
		if len(parts) != 4 {
			return false
		}
		salt, err1 := base64.StdEncoding.DecodeString(parts[2])
		want, err2 := base64.StdEncoding.DecodeString(parts[3])
		if err1 != nil || err2 != nil {
			return false
		}
		mac := hmac.New(sha1.New, salt)
		mac.Write([]byte(name))
		return hmac.Equal(mac.Sum(nil), want)
	}
	for _, pattern := range strings.Split(field, ",") {
		if pattern == name {
			return true
		}
	}
	return false
}

// pinState is how the scanned keys relate to the known_hosts file.
type pinState int

const (
	pinNew    pinState = iota // the file has no key for the host
	pinPinned                 // the file already holds a matching key
)

// HostKeyMismatchError reports that known_hosts holds a different key for
// the host than the one it presents now.
type HostKeyMismatchError struct {
	File, Name string
	Existing   []HostKey
	Scanned    []HostKey
}

func (e *HostKeyMismatchError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "HOST KEY MISMATCH: %s already holds a different key for %s than the host presents now.\n", e.File, e.Name)
	b.WriteString("This can mean the host was reinstalled, or that someone is intercepting the connection.\n")
	b.WriteString("Recorded:\n")
	for _, k := range e.Existing {
		fmt.Fprintf(&b, "  %s %s\n", k.Type, k.Fingerprint())
	}
	b.WriteString("Presented:\n")
	for _, k := range e.Scanned {
		fmt.Fprintf(&b, "  %s %s\n", k.Type, k.Fingerprint())
	}
	b.WriteString("Verify the new fingerprint through a trusted channel (the provider console), then remove the old entry from the file by hand")
	return b.String()
}

// comparePins checks scanned keys against the keys already recorded for the
// host. Every key type present in both must match exactly; recorded keys
// with no overlapping type also count as a mismatch.
func comparePins(file, name string, existing, scanned []HostKey) (pinState, error) {
	if len(existing) == 0 {
		return pinNew, nil
	}
	overlap := false
	for _, s := range scanned {
		for _, e := range existing {
			if e.Type != s.Type {
				continue
			}
			overlap = true
			if e.Blob != s.Blob {
				return 0, &HostKeyMismatchError{File: file, Name: name, Existing: existing, Scanned: scanned}
			}
		}
	}
	if !overlap {
		return 0, &HostKeyMismatchError{File: file, Name: name, Existing: existing, Scanned: scanned}
	}
	return pinPinned, nil
}

// keyscan runs `ssh-keyscan -p PORT HOST` locally.
func (r *Runner) keyscan(ctx context.Context, addr string, port int) ([]HostKey, error) {
	program := r.KeyscanProgram
	if program == "" {
		program = "ssh-keyscan"
	}
	res, err := r.Exec.Run(ctx, transport.Cmd{Args: []string{program, "-T", "15", "-p", strconv.Itoa(port), addr}, Capture: true})
	if err != nil {
		var nf *transport.NotFoundError
		if errors.As(err, &nf) {
			return nil, fmt.Errorf("ssh-keyscan is not installed locally (it ships with the OpenSSH client)")
		}
		return nil, fmt.Errorf("ssh-keyscan %s port %d failed: %w", addr, port, err)
	}
	keys, err := parseKeyscan(res.Stdout)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("could not fetch SSH host keys from %s port %d (is the host reachable and running sshd on that port?)", addr, port)
	}
	return keys, nil
}

// readKnownHosts returns the file's content, or "" when it does not exist.
func readKnownHosts(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", path, err)
	}
	return string(data), nil
}

// knownHostsLines renders keys as known_hosts lines for name.
func knownHostsLines(name string, keys []HostKey) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(name + " " + k.Type + " " + k.Blob + "\n")
	}
	return b.String()
}

// writeKnownHosts appends keys for name to path (creating its directory),
// atomically and with mode 600.
func writeKnownHosts(path, existing, name string, keys []HostKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", filepath.Dir(path), err)
	}
	content := existing
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += knownHostsLines(name, keys)
	return atomicWrite(path, []byte(content), 0o600)
}

// atomicWrite replaces path with data through a temporary file in the same
// directory and a rename.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".depper-*")
	if err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	return nil
}
