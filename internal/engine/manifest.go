package engine

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/frgmt0/depper/internal/shell"
)

// SaltBytes is the size of the fingerprint salt.
const SaltBytes = 32

// Manifest is <base>/.depper/environment.manifest: which keys the live
// environment file holds, their salted fingerprints, and the SHA-256 of the
// file depper wrote. It contains no secret values.
type Manifest struct {
	FileSHA256 string `json:"file_sha256"`
	// Keys maps each variable name to hex HMAC-SHA256(salt, value).
	Keys      map[string]string `json:"keys"`
	UpdatedAt string            `json:"updated_at"`
}

// KeyNames returns the manifest's variable names, sorted.
func (m *Manifest) KeyNames() []string {
	names := make([]string, 0, len(m.Keys))
	for k := range m.Keys {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

var (
	hex64Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
	envKey  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ParseManifest validates manifest JSON.
func ParseManifest(raw string) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("the remote environment manifest is not valid JSON")
	}
	if !hex64Re.MatchString(m.FileSHA256) || m.Keys == nil {
		return nil, fmt.Errorf("the remote environment manifest is invalid")
	}
	for k, v := range m.Keys {
		if !envKey.MatchString(k) || !hex64Re.MatchString(v) {
			return nil, fmt.Errorf("the remote environment manifest is invalid")
		}
	}
	return &m, nil
}

// Fingerprints returns hex HMAC-SHA256(salt, value) for every value. The
// HMAC key is the raw salt bytes.
func Fingerprints(salt []byte, values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		mac := hmac.New(sha256.New, salt)
		mac.Write([]byte(v))
		out[k] = hex.EncodeToString(mac.Sum(nil))
	}
	return out
}

// SHA256Hex returns the hex SHA-256 of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// NewManifest builds the manifest for an environment file body and the
// values it encodes.
func NewManifest(salt []byte, fileBody []byte, values map[string]string, now time.Time) *Manifest {
	return &Manifest{FileSHA256: SHA256Hex(fileBody), Keys: Fingerprints(salt, values), UpdatedAt: now.UTC().Format(time.RFC3339)}
}

// Encode returns the manifest as one line of JSON.
func (m *Manifest) Encode() []byte {
	data, _ := json.Marshal(m)
	return append(data, '\n')
}

// KeyChanges is a names-only comparison of two environments.
type KeyChanges struct {
	// Known is false when there was no previous manifest to compare with;
	// then every key is listed in Added.
	Known     bool     `json:"known"`
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Changed   []string `json:"changed"`
	Unchanged []string `json:"unchanged"`
}

// InSync reports that nothing was added, removed, or changed.
func (k KeyChanges) InSync() bool {
	return k.Known && len(k.Added) == 0 && len(k.Removed) == 0 && len(k.Changed) == 0
}

// CompareFingerprints compares previous (nil when unknown) with next.
func CompareFingerprints(previous, next map[string]string) KeyChanges {
	k := KeyChanges{Known: previous != nil, Added: []string{}, Removed: []string{}, Changed: []string{}, Unchanged: []string{}}
	for name, fp := range next {
		old, ok := previous[name]
		switch {
		case !ok:
			k.Added = append(k.Added, name)
		case hmac.Equal([]byte(old), []byte(fp)):
			k.Unchanged = append(k.Unchanged, name)
		default:
			k.Changed = append(k.Changed, name)
		}
	}
	for name := range previous {
		if _, ok := next[name]; !ok {
			k.Removed = append(k.Removed, name)
		}
	}
	sort.Strings(k.Added)
	sort.Strings(k.Removed)
	sort.Strings(k.Changed)
	sort.Strings(k.Unchanged)
	return k
}

// Summary renders the change summary (names only).
func (k KeyChanges) Summary() string {
	if !k.Known {
		if len(k.Added) == 0 {
			return "no variables (no previous manifest)"
		}
		return fmt.Sprintf("%d set (%s; no previous manifest to compare)", len(k.Added), strings.Join(k.Added, ", "))
	}
	var parts []string
	for _, p := range []struct {
		label string
		names []string
	}{{"added", k.Added}, {"changed", k.Changed}, {"removed", k.Removed}} {
		if len(p.names) > 0 {
			parts = append(parts, fmt.Sprintf("%d %s (%s)", len(p.names), p.label, strings.Join(p.names, ", ")))
		}
	}
	parts = append(parts, fmt.Sprintf("%d unchanged", len(k.Unchanged)))
	return strings.Join(parts, ", ")
}

// SecretsState is what the host knows about the live environment, without
// any secret value: the salt, the manifest, and the live file's SHA-256.
type SecretsState struct {
	// Salt is nil when no salt exists yet.
	Salt []byte
	// Manifest is nil when no manifest exists.
	Manifest *Manifest
	// LiveExists reports that the environment file exists.
	LiveExists bool
	// LiveSHA256 is `sha256sum` of the live environment file.
	LiveSHA256 string
}

// FileModified reports that the live environment file differs from the one
// depper recorded (edited outside depper), when both are known.
func (s *SecretsState) FileModified() bool {
	if s.Manifest == nil {
		return false
	}
	return !s.LiveExists || s.LiveSHA256 != s.Manifest.FileSHA256
}

// ReadSecretsState reads the salt, manifest, and live file hash in one round
// trip.
func (e *Engine) ReadSecretsState(ctx context.Context, l *Layout) (*SecretsState, error) {
	script := shell.Script(
		"set +xv",
		"set -eu",
		"meta="+shell.Quote(l.Meta()),
		refuseSymlink(`"$meta/fingerprint.salt"`, "fingerprint salt"),
		refuseSymlink(`"$meta/environment.manifest"`, "environment manifest"),
		refuseSymlink(`"$meta/environment"`, "environment file"),
		`echo @@salt`,
		`if test -f "$meta/fingerprint.salt"; then head -c 256 "$meta/fingerprint.salt"; echo; fi`,
		`echo @@manifest`,
		`if test -f "$meta/environment.manifest"; then head -c 4194304 "$meta/environment.manifest"; echo; fi`,
		`echo @@live`,
		`if test -f "$meta/environment"; then sha256sum < "$meta/environment"; fi`,
	)
	out, err := e.Remote.Capture(ctx, script)
	if err != nil {
		return nil, err
	}
	return parseSecretsState(out)
}

func parseSecretsState(out string) (*SecretsState, error) {
	m := sectionMap(out)
	state := &SecretsState{}
	if raw := strings.TrimSpace(m["salt"]); raw != "" {
		salt, err := hex.DecodeString(raw)
		if err != nil || len(salt) != SaltBytes {
			return nil, fmt.Errorf("the remote fingerprint salt is invalid")
		}
		state.Salt = salt
	}
	if raw := strings.TrimSpace(m["manifest"]); raw != "" {
		manifest, err := ParseManifest(raw)
		if err != nil {
			return nil, err
		}
		state.Manifest = manifest
	}
	if raw := strings.TrimSpace(m["live"]); raw != "" {
		sum, _, _ := strings.Cut(raw, " ")
		if !hex64Re.MatchString(sum) {
			return nil, fmt.Errorf("the remote host returned an invalid sha256sum")
		}
		state.LiveExists, state.LiveSHA256 = true, sum
	}
	return state, nil
}

// EnsureSalt returns state.Salt, creating and uploading a new random salt
// (mode 600) when none exists. Call it while holding the lock.
func (e *Engine) EnsureSalt(ctx context.Context, l *Layout, state *SecretsState) ([]byte, error) {
	if state.Salt != nil {
		return state.Salt, nil
	}
	salt := make([]byte, SaltBytes)
	if _, err := io.ReadFull(e.Rand, salt); err != nil {
		return nil, fmt.Errorf("generating the fingerprint salt: %w", err)
	}
	if err := e.Upload(ctx, []byte(hex.EncodeToString(salt)+"\n"), l.Salt(), "600"); err != nil {
		return nil, err
	}
	state.Salt = salt
	return salt, nil
}
