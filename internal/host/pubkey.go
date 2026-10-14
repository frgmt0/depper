package host

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// PublicKey is an OpenSSH public key to authorize for the deploy user.
type PublicKey struct {
	Type    string
	Blob    string
	Comment string
}

// userKeyTypes are the key types accepted for authorized_keys.
var userKeyTypes = map[string]bool{
	"ssh-ed25519": true, "ssh-rsa": true,
	"ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true, "ecdsa-sha2-nistp521": true,
	"sk-ssh-ed25519@openssh.com": true, "sk-ecdsa-sha2-nistp256@openssh.com": true,
}

var commentRe = regexp.MustCompile(`^[\x20-\x7e]*$`)

// Line is the authorized_keys line.
func (k PublicKey) Line() string {
	if k.Comment == "" {
		return k.Type + " " + k.Blob
	}
	return k.Type + " " + k.Blob + " " + k.Comment
}

// Fingerprint returns the SHA256 fingerprint of the key.
func (k PublicKey) Fingerprint() string { return HostKey{Type: k.Type, Blob: k.Blob}.Fingerprint() }

// ParsePublicKey parses one OpenSSH public key line (no options prefix).
func ParsePublicKey(text string) (PublicKey, error) {
	lines := 0
	var line string
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			lines++
			line = l
		}
	}
	if lines != 1 {
		return PublicKey{}, fmt.Errorf("expected exactly one public key line")
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return PublicKey{}, fmt.Errorf("not an OpenSSH public key")
	}
	k := PublicKey{Type: fields[0], Blob: fields[1], Comment: strings.Join(fields[2:], " ")}
	if !userKeyTypes[k.Type] {
		return PublicKey{}, fmt.Errorf("unsupported public key type %q (a private key, or a key with options?)", clean(k.Type))
	}
	raw, err := base64.StdEncoding.DecodeString(k.Blob)
	if err != nil || len(raw) < 4 {
		return PublicKey{}, fmt.Errorf("the public key data is not valid base64")
	}
	if !commentRe.MatchString(k.Comment) {
		return PublicKey{}, fmt.Errorf("the public key comment contains unsupported characters")
	}
	return k, nil
}

// ReadPublicKey reads and parses a .pub file.
func ReadPublicKey(path string) (PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PublicKey{}, fmt.Errorf("cannot read public key: %w", err)
	}
	k, err := ParsePublicKey(string(data))
	if err != nil {
		return PublicKey{}, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

// DefaultKeyNames are the public keys --key defaults to, in order.
var DefaultKeyNames = []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"}

// DefaultKeyFile returns the first existing default public key under
// home/.ssh.
func DefaultKeyFile(home string) (string, error) {
	for _, name := range DefaultKeyNames {
		p := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no SSH public key found (looked for ~/.ssh/%s); create one with `ssh-keygen -t ed25519` or pass --key FILE.pub", strings.Join(DefaultKeyNames, ", ~/.ssh/"))
}

// PrivateKeyFor returns the private key next to a .pub file, or "" when it
// does not exist (the key may then live in an agent).
func PrivateKeyFor(pub string) string {
	if !strings.HasSuffix(pub, ".pub") {
		return ""
	}
	p := strings.TrimSuffix(pub, ".pub")
	if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
		return p
	}
	return ""
}
