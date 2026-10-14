// Package host prepares a fresh Linux host for depper (`depper host init`)
// and reports on a prepared one (`depper host status`).
//
// All remote work runs as an administrator (root, or a user with
// passwordless sudo) over SSH with the host key pinned in a repo-local
// known_hosts file. Every step is checked first and applied only when
// needed, so a second run changes nothing. Remote scripts are POSIX sh,
// built from lines with every interpolated value validated and quoted with
// internal/shell, and sent on stdin through shell.Wrap.
package host

import (
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/frgmt0/depper/internal/doctor"
	"github.com/frgmt0/depper/internal/transport"
)

// DefaultUser is the deploy account host init creates.
const DefaultUser = "deploy"

// Remote paths depper owns on the host.
const (
	RecordPath = "/etc/depper/host.json"
	// SSHDropIn is read before the drop-ins clouds ship (Ubuntu cloud images
	// have 50-cloud-init.conf), because sshd keeps the first value it reads.
	SSHDropIn     = "/etc/ssh/sshd_config.d/" + sshDropInName
	sshDropInName = "10-depper.conf"
	sshdMain      = "/etc/ssh/sshd_config"
	sshdDropInD   = "/etc/ssh/sshd_config.d"
	defaultSPath  = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// RevertDelay is how long an armed revert (a transient systemd timer on the
// host) waits before it undoes --harden-ssh or --firewall. host init disarms
// it once new admin and deploy logins succeed.
const RevertDelay = 120 * time.Second

// sessionOptions are added to every ssh connection host init opens (admin
// and deploy, dry run included): nothing from this machine is forwarded to
// the host.
var sessionOptions = []string{"ForwardAgent=no", "ForwardX11=no", "ClearAllForwardings=yes"}

// Container engine choices for --container. ContainerAuto (no flag) installs
// the engine the resolved configuration uses: [container].engine (docker
// when unset), or nothing without [container].
const (
	ContainerAuto   = ""
	ContainerNone   = "none"
	ContainerPodman = "podman"
	ContainerDocker = "docker"
)

var (
	// deployUserRe is the rule for the deploy account name.
	deployUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	// adminUserRe matches the SSH login name of the administrator.
	adminUserRe   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}$`)
	hostNameRe    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	envNameRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	targetNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	fingerprintRe = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
)

// Secret holds a value that must never be printed. Its String and Format
// methods redact it, and it is excluded from JSON.
type Secret struct{ value string }

// NewSecret wraps value.
func NewSecret(value string) Secret { return Secret{value: value} }

// Set reports whether a value is present.
func (s Secret) Set() bool { return s.value != "" }

func (s Secret) String() string { return "<redacted>" }

// Format implements fmt.Formatter so no verb prints the value.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "<redacted>") }

// MarshalJSON never emits the value.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"<redacted>"`), nil }

// Options are the inputs of `depper host init`.
type Options struct {
	// Repo is the repository whose depper.toml receives the connection.
	Repo string
	// Admin and Host come from ADMIN@HOST; Host has IPv6 brackets removed.
	Admin string
	Host  string
	// Port is the SSH port (22 when unset).
	Port int
	// Identity is the admin's private key file ("" for ssh defaults).
	Identity string
	// User is the deploy account to create.
	User string
	// KeyFile is the public key to authorize for User; KeyExplicit is true
	// when --key was given (its private key is then recorded as
	// identity_file).
	KeyFile     string
	KeyExplicit bool
	// Container is ContainerAuto, ContainerNone, ContainerPodman or
	// ContainerDocker; Init replaces ContainerAuto with the resolved choice.
	Container   string
	Cloudflared bool
	HardenSSH   bool
	// Firewall is the list of TCP ports to allow; nil means no firewall.
	Firewall    []int
	AutoUpdates bool
	// TailscaleKey is the auth key read from the environment variable
	// named by --tailscale-authkey-env.
	TailscaleKey Secret
	// Target is the [targets.<name>] table to write ("" for [deploy]).
	Target string
	// KnownHosts is the absolute known_hosts path.
	KnownHosts string
	// HostKeyFingerprints are "SHA256:..." fingerprints the scanned host
	// keys are checked against (--host-key-fingerprint); only matching
	// keys are pinned.
	HostKeyFingerprints []string
	// TrustOnFirstUse allows --yes to pin unverified host keys.
	TrustOnFirstUse bool
	// Compose is set by Init when the configuration uses
	// [container].compose, so a compose provider is installed.
	Compose     bool
	WriteConfig bool
	DryRun      bool
	Yes         bool
}

// Runner carries the process plumbing for host init and status.
type Runner struct {
	Exec transport.Executor
	// SSHProgram and KeyscanProgram default to "ssh" and "ssh-keyscan".
	SSHProgram     string
	KeyscanProgram string
	// Out receives human-readable progress (stderr in --json mode).
	Out io.Writer
	// Confirm asks a yes/no question; required unless Options.Yes.
	Confirm func(question string) (bool, error)
	// LookupEnv reads the environment for config resolution.
	LookupEnv func(string) (string, bool)
	// Now returns the current time (time.Now by default).
	Now func() time.Time
	// Version is the depper version recorded on the host.
	Version string

	// fsRoot prefixes every absolute remote path (tests only).
	fsRoot string
	// sysPath is prepended to PATH in root scripts, so sbin tools resolve
	// (tests use "").
	sysPath string
	// pathSet records that sysPath was configured (zero value: default).
	pathSet bool
	// managerWait is how many seconds to wait for the user manager (0: 30).
	managerWait int
	// retryDelay separates login attempts after an sshd reload (0: 1s).
	retryDelay time.Duration
	// revertDelay is how long an armed revert waits before it undoes a
	// guarded change (0: RevertDelay); revertGrace is how long host init
	// waits past it before checking that the revert worked (0: 15s).
	revertDelay time.Duration
	revertGrace time.Duration
	// interruptAt makes host init stop right after the named guarded step
	// changed the host, as if it were killed there (tests only).
	interruptAt string
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) out() io.Writer {
	if r.Out == nil {
		return io.Discard
	}
	return r.Out
}

func (r *Runner) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.out(), format, args...)
}

func (r *Runner) systemPath() string {
	if r.pathSet {
		return r.sysPath
	}
	return defaultSPath
}

// ParseDestination splits ADMIN@HOST, validating both parts. IPv6 literals
// may be bracketed; the brackets are removed.
func ParseDestination(dest string) (admin, addr string, err error) {
	i := strings.LastIndexByte(dest, '@')
	if i <= 0 {
		return "", "", fmt.Errorf("expected ADMIN@HOST (for example root@203.0.113.10), got %q", clean(dest))
	}
	admin, addr = dest[:i], dest[i+1:]
	if !adminUserRe.MatchString(admin) {
		return "", "", fmt.Errorf("invalid admin user name %q", clean(admin))
	}
	addr, err = ValidateHost(addr)
	if err != nil {
		return "", "", err
	}
	return admin, addr, nil
}

// ValidateHost checks a DNS name, IPv4 or IPv6 address (bracketed or bare)
// and returns it without brackets.
func ValidateHost(addr string) (string, error) {
	bad := fmt.Errorf("invalid host %q: use a DNS name, IPv4, or IPv6 address", clean(addr))
	if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
		addr = addr[1 : len(addr)-1]
		if !strings.Contains(addr, ":") {
			return "", bad
		}
	}
	if strings.Contains(addr, ":") {
		ip, err := netip.ParseAddr(addr)
		if err != nil || !ip.Is6() || ip.Zone() != "" {
			return "", bad
		}
		return addr, nil
	}
	if !hostNameRe.MatchString(addr) {
		return "", bad
	}
	return addr, nil
}

// ValidateUser checks a deploy account name.
func ValidateUser(user string) error {
	if !deployUserRe.MatchString(user) {
		return fmt.Errorf("invalid deploy user %q: use lowercase letters, digits, '_' and '-', starting with a letter or '_' (max 32 chars)", clean(user))
	}
	if user == "root" {
		return fmt.Errorf("the deploy user must not be root")
	}
	return nil
}

// ValidateEnvName checks the name given to --tailscale-authkey-env.
func ValidateEnvName(name string) error {
	if !envNameRe.MatchString(name) {
		return fmt.Errorf("invalid environment variable name %q", clean(name))
	}
	return nil
}

// ValidateTailscaleKey checks an auth key's shape without revealing it.
func ValidateTailscaleKey(key string) error {
	if key == "" {
		return fmt.Errorf("the Tailscale auth key is empty")
	}
	if strings.ContainsFunc(key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r > unicode.MaxASCII }) {
		return fmt.Errorf("the Tailscale auth key contains whitespace or non-ASCII characters")
	}
	return nil
}

// Validate checks Options before anything contacts the host.
func (o *Options) Validate() error {
	if o.Port == 0 {
		o.Port = 22
	}
	if o.Port < 1 || o.Port > 65535 {
		return fmt.Errorf("--port must be between 1 and 65535")
	}
	if err := ValidateUser(o.User); err != nil {
		return err
	}
	if o.User == o.Admin {
		return fmt.Errorf("the deploy user %q must differ from the admin account", o.User)
	}
	switch o.Container {
	case ContainerAuto, ContainerNone, ContainerPodman, ContainerDocker:
	default:
		return fmt.Errorf("--container must be podman, docker, or none")
	}
	for _, p := range o.Firewall {
		if p < 1 || p > 65535 {
			return fmt.Errorf("--firewall ports must be between 1 and 65535")
		}
	}
	for _, p := range []struct{ flag, value string }{{"--identity", o.Identity}, {"--known-hosts", o.KnownHosts}, {"--key", o.KeyFile}} {
		if strings.ContainsFunc(p.value, unicode.IsSpace) || strings.ContainsFunc(p.value, unicode.IsControl) {
			return fmt.Errorf("%s path %q contains whitespace or control characters, which ssh cannot use", p.flag, clean(p.value))
		}
	}
	if o.Target != "" && !targetNameRe.MatchString(o.Target) {
		return fmt.Errorf("invalid target name %q (letters, digits, '-' and '_', max 63 chars)", clean(o.Target))
	}
	if o.KnownHosts == "" {
		return fmt.Errorf("a known_hosts path is required")
	}
	for _, fp := range o.HostKeyFingerprints {
		if !fingerprintRe.MatchString(fp) {
			return fmt.Errorf("invalid --host-key-fingerprint %q: use the SHA256:... form `ssh-keygen -lf` prints", clean(fp))
		}
	}
	if o.TailscaleKey.Set() {
		if err := ValidateTailscaleKey(o.TailscaleKey.value); err != nil {
			return err
		}
	}
	return nil
}

// clean removes control characters from text that may come from the remote
// host or the user, so it cannot manipulate the terminal.
func clean(s string) string { return doctor.Clean(s) }
