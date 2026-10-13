// Package config loads and validates depper.toml (or the legacy
// deploy.toml) and resolves [targets.<name>] overlays into one immutable
// Config.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/frgmt0/depper/internal/transport"
)

// Configuration file names, in order of preference.
const (
	FileName       = "depper.toml"
	LegacyFileName = "deploy.toml"
)

// Environment variables that override SSH credentials at resolve time (CI).
const (
	EnvSSHKeyFile    = "DEPPER_SSH_KEY_FILE"
	EnvKnownHostsKey = "DEPPER_KNOWN_HOSTS_FILE"
)

// Defaults applied when keys are absent.
const (
	DefaultRestart      = "on-failure"
	DefaultKeepReleases = 5
	DefaultSource       = SourceWorktree
	DefaultEngine       = "docker"
	DefaultTimeout      = 5 * time.Second
	DefaultStartPeriod  = 30 * time.Second
	DefaultInterval     = 2 * time.Second
)

// Values of [deploy].source.
const (
	SourceWorktree = "worktree"
	SourceGit      = "git"
)

// DefaultExcludes are always passed to rsync, ahead of [deploy].exclude.
var DefaultExcludes = []string{
	".git/", ".env", ".env.*", LegacyFileName, FileName, ".cloudflared/", ".cache/", "node_modules/",
	"venv/", ".venv/", "__pycache__/", ".pytest_cache/", ".mypy_cache/", ".ruff_cache/",
}

// Error is a configuration error.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

// Service is [service].
type Service struct {
	Name string
	// Command is the process command; empty when [container] is used.
	Command string
	// Restart is on-failure (default), always, or no.
	Restart string
}

// Deploy is [deploy] after target overlay and env overrides.
type Deploy struct {
	// Host is an SSH alias, DNS name, IPv4, or bare IPv6 literal (brackets
	// removed), optionally prefixed with user@.
	Host string
	User string
	// Port is 0 when unset (ssh default / ssh config).
	Port int
	// IdentityFile and KnownHosts are absolute local paths or "".
	IdentityFile string
	KnownHosts   string
	// Path is the absolute remote base path, or "" for the default
	// ~/.local/share/depper/<name>.
	Path  string
	Setup []string
	// Exclude is DefaultExcludes followed by [deploy].exclude, deduplicated.
	Exclude      []string
	KeepReleases int
	// Source is SourceWorktree or SourceGit.
	Source string
}

// Secrets is [secrets].
type Secrets struct {
	// File is the absolute local path of the dotenv file.
	File string
}

// Tunnel is [tunnel].
type Tunnel struct {
	// Hostname is the lowercased public FQDN without a trailing dot.
	Hostname string
	// Service is the origin URL (scheme://host:port, no trailing slash).
	Service string
}

// URL is the public tunnel URL.
func (t *Tunnel) URL() string { return "https://" + t.Hostname }

// Container modes.
const (
	ModeProcess = "process"
	ModeBuild   = "build"
	ModeImage   = "image"
	ModeCompose = "compose"
)

// Container is [container]. Exactly one of Build, Image, Compose is set.
type Container struct {
	// Engine is "docker" or "podman".
	Engine string
	// Build is the build context directory relative to the repository.
	Build string
	// Dockerfile is relative to the repository ("" for the engine default).
	Dockerfile string
	// Image is a prebuilt image reference to pull.
	Image string
	// Compose is the compose file relative to the repository.
	Compose string
	Ports   []string
	Volumes []string
	Args    []string
	// Command overrides the image command (a single string).
	Command string
}

// Mode returns ModeBuild, ModeImage, or ModeCompose.
func (c *Container) Mode() string {
	switch {
	case c.Build != "":
		return ModeBuild
	case c.Image != "":
		return ModeImage
	default:
		return ModeCompose
	}
}

// Endpoint is [endpoint].
type Endpoint struct{ URL string }

// Health is [health] (or the [deploy].healthcheck alias). Exactly one of
// URL and Command is set.
type Health struct {
	URL     string
	Command string
	// ExpectStatus is the required HTTP status; 0 means any 2xx.
	ExpectStatus int
	Timeout      time.Duration
	StartPeriod  time.Duration
	Interval     time.Duration
}

// Env is the [env] table. Its values are hidden from fmt output so a
// printed Config never leaks them.
type Env map[string]string

// Format implements fmt.Formatter, printing keys only.
func (e Env) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "config.Env{")
	for i, key := range sortedKeys(e) {
		if i > 0 {
			_, _ = io.WriteString(f, ", ")
		}
		_, _ = io.WriteString(f, key+":<redacted>")
	}
	_, _ = io.WriteString(f, "}")
}

// Config is a fully merged, validated configuration for one target. Resolve
// returns a fresh deep copy each time; treat it as read-only.
type Config struct {
	// Repo is the absolute, symlink-resolved repository directory.
	Repo string
	// File is the absolute path of the loaded configuration file.
	File string
	// Target is the selected target name, "" for the base configuration.
	Target    string
	Service   Service
	Deploy    Deploy
	Env       Env
	Secrets   *Secrets
	Tunnel    *Tunnel
	Container *Container
	Endpoint  *Endpoint
	Health    *Health
}

// UnitName is the app's systemd user unit: depper-<name>.service.
func (c *Config) UnitName() string { return "depper-" + c.Service.Name + ".service" }

// TunnelUnitName is depper-tunnel@<name>.service.
func (c *Config) TunnelUnitName() string { return "depper-tunnel@" + c.Service.Name + ".service" }

// LegacyUnitName is the Python deployer's unit: deployer-<name>.service.
func (c *Config) LegacyUnitName() string { return "deployer-" + c.Service.Name + ".service" }

// LegacyTunnelUnitName is deployer-tunnel@<name>.service.
func (c *Config) LegacyTunnelUnitName() string {
	return "deployer-tunnel@" + c.Service.Name + ".service"
}

// ContainerName is the container / compose project name: depper-<name>.
func (c *Config) ContainerName() string { return "depper-" + c.Service.Name }

// ImageRef is the local image tag for a release: depper/<name>:<release>
// (name lowercased; validated as an image component when Build is used).
func (c *Config) ImageRef(releaseID string) string {
	return "depper/" + strings.ToLower(c.Service.Name) + ":" + releaseID
}

// Mode returns ModeProcess or the container mode.
func (c *Config) Mode() string {
	if c.Container == nil {
		return ModeProcess
	}
	return c.Container.Mode()
}

// DefaultBaseRel is the default base path relative to the remote $HOME.
func (c *Config) DefaultBaseRel() string { return ".local/share/depper/" + c.Service.Name }

// LegacyDefaultBaseRel is the Python deployer's default base relative to $HOME.
func (c *Config) LegacyDefaultBaseRel() string { return ".local/share/deployer/" + c.Service.Name }

// SSHTarget returns the transport target for this configuration.
func (c *Config) SSHTarget() transport.Target {
	return transport.Target{
		Host:         c.Deploy.Host,
		User:         c.Deploy.User,
		Port:         c.Deploy.Port,
		IdentityFile: c.Deploy.IdentityFile,
		KnownHosts:   c.Deploy.KnownHosts,
	}
}

var (
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	envRe      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	targetRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	imageNameR = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
)

// Project is a loaded configuration file: the base configuration plus its
// named target overlays, each validated field by field. Call Resolve to get
// a usable Config.
type Project struct {
	Repo    string
	File    string
	base    baseLayer
	targets map[string]targetLayer
}

// TargetNames returns the defined target names, sorted.
func (p *Project) TargetNames() []string { return sortedKeys(p.targets) }

func resolveRepo(repo string) (string, error) {
	abs, err := filepath.Abs(expandUser(repo))
	if err != nil {
		return "", errorf("invalid repository path: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// FindFile returns the configuration file in repo: depper.toml, else
// deploy.toml. Both present is an error.
func FindFile(repo string) (string, error) {
	preferred := filepath.Join(repo, FileName)
	legacy := filepath.Join(repo, LegacyFileName)
	hasPreferred, err := exists(preferred)
	if err != nil {
		return "", errorf("cannot read %s: %v", preferred, err)
	}
	hasLegacy, err := exists(legacy)
	if err != nil {
		return "", errorf("cannot read %s: %v", legacy, err)
	}
	switch {
	case hasPreferred && hasLegacy:
		return "", errorf("both %s and %s exist in %s; keep only %s", FileName, LegacyFileName, repo, FileName)
	case hasPreferred:
		return preferred, nil
	case hasLegacy:
		return legacy, nil
	}
	return "", errorf("no %s (or %s) found in %s", FileName, LegacyFileName, repo)
}

// Load reads and validates the configuration file of repo.
func Load(repo string) (*Project, error) {
	root, err := resolveRepo(repo)
	if err != nil {
		return nil, err
	}
	file, err := FindFile(root)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, errorf("cannot read %s: %v", file, err)
	}
	return parse(root, file, string(data))
}

// LoadTarget loads repo and resolves target using lookup for environment
// overrides (nil lookup means os.LookupEnv).
func LoadTarget(repo, target string, lookup func(string) (string, bool)) (*Config, error) {
	project, err := Load(repo)
	if err != nil {
		return nil, err
	}
	return project.ResolveWithEnv(target, lookup)
}

func parse(repo, file, text string) (*Project, error) {
	name := filepath.Base(file)
	var raw map[string]any
	if _, err := toml.Decode(text, &raw); err != nil {
		return nil, errorf("invalid %s: %v", name, err)
	}
	if err := checkKeys(raw, name, "service", "deploy", "env", "secrets", "tunnel", "container", "endpoint", "health", "targets"); err != nil {
		return nil, errorf("%s may contain only [service], [deploy], [env], [secrets], [tunnel], [container], [endpoint], [health], and [targets.<name>] (%v)", name, err)
	}
	p := &Project{Repo: repo, File: file, targets: map[string]targetLayer{}}
	if err := p.base.parse(raw, repo); err != nil {
		return nil, err
	}
	targets, err := optTable(raw, "targets", "[targets]")
	if err != nil {
		return nil, err
	}
	for _, targetName := range sortedKeys(targets) {
		if !targetRe.MatchString(targetName) {
			return nil, errorf("invalid target name %q (letters, digits, '-' and '_', max 63 chars)", targetName)
		}
		label := "[targets." + targetName + "]"
		table, err := asTable(targets[targetName], label)
		if err != nil {
			return nil, err
		}
		var t targetLayer
		if err := t.parse(table, label, repo); err != nil {
			return nil, err
		}
		p.targets[targetName] = t
	}
	return p, nil
}

// Resolve merges target (or the base configuration when target is "") and
// applies DEPPER_SSH_KEY_FILE / DEPPER_KNOWN_HOSTS_FILE from the process
// environment.
func (p *Project) Resolve(target string) (*Config, error) {
	return p.ResolveWithEnv(target, os.LookupEnv)
}

// ResolveWithEnv is Resolve with an injectable environment lookup.
func (p *Project) ResolveWithEnv(target string, lookup func(string) (string, bool)) (*Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	b := p.base
	d := b.deploy.clone()
	env := Env{}
	for k, v := range b.env {
		env[k] = v
	}
	cfg := &Config{
		Repo:      p.Repo,
		File:      p.File,
		Target:    target,
		Service:   Service{Name: b.name, Command: b.command, Restart: b.restart},
		Secrets:   cloneSecrets(b.secrets),
		Tunnel:    cloneTunnel(b.tunnel),
		Container: cloneContainer(b.container),
		Endpoint:  cloneEndpoint(b.endpoint),
	}
	health := b.health.clone()
	if target != "" {
		t, ok := p.targets[target]
		if !ok {
			if len(p.targets) == 0 {
				return nil, errorf("unknown target %q: %s defines no [targets]", target, filepath.Base(p.File))
			}
			return nil, errorf("unknown target %q (available: %s)", target, strings.Join(p.TargetNames(), ", "))
		}
		if t.name != nil {
			cfg.Service.Name = *t.name
		}
		d.overlay(t.deploy)
		for k, v := range t.env {
			env[k] = v
		}
		if t.secrets != nil {
			cfg.Secrets = cloneSecrets(t.secrets)
		}
		if t.endpoint != nil {
			cfg.Endpoint = cloneEndpoint(t.endpoint)
		}
		health.overlay(t.health)
	}
	cfg.Env = env

	if d.host == nil {
		if target == "" && len(p.targets) > 0 {
			return nil, errorf("no target selected and [deploy].host is not set; choose one with --target or DEPPER_TARGET (available: %s)", strings.Join(p.TargetNames(), ", "))
		}
		if target != "" {
			return nil, errorf("[deploy].host is required (target %q does not set host)", target)
		}
		return nil, errorf("[deploy].host must be a non-empty string")
	}
	cfg.Deploy = Deploy{
		Host:         *d.host,
		User:         deref(d.user),
		IdentityFile: deref(d.identityFile),
		KnownHosts:   deref(d.knownHosts),
		Path:         deref(d.path),
		Setup:        append([]string{}, d.setup...),
		KeepReleases: DefaultKeepReleases,
		Source:       DefaultSource,
	}
	if d.port != nil {
		cfg.Deploy.Port = *d.port
	}
	if d.keepReleases != nil {
		cfg.Deploy.KeepReleases = *d.keepReleases
	}
	if d.source != nil {
		cfg.Deploy.Source = *d.source
	}
	cfg.Deploy.Exclude = dedupe(append(append([]string{}, DefaultExcludes...), b.exclude...))
	if strings.Contains(cfg.Deploy.Host, "@") && cfg.Deploy.User != "" {
		return nil, errorf("[deploy].host includes a user (user@host) and [deploy].user is also set; use only one")
	}

	for _, override := range []struct {
		name string
		dest *string
	}{{EnvSSHKeyFile, &cfg.Deploy.IdentityFile}, {EnvKnownHostsKey, &cfg.Deploy.KnownHosts}} {
		value, ok := lookup(override.name)
		if !ok || value == "" {
			continue
		}
		if hasControl(value) {
			return nil, errorf("%s may not contain control characters", override.name)
		}
		abs, err := filepath.Abs(expandUser(value))
		if err != nil {
			return nil, errorf("%s is not a valid path: %v", override.name, err)
		}
		*override.dest = abs
	}

	if health.any() {
		h, err := health.finish()
		if err != nil {
			return nil, err
		}
		cfg.Health = h
	}
	if cfg.Container != nil && cfg.Container.Build != "" && !imageNameR.MatchString(strings.ToLower(cfg.Service.Name)) {
		return nil, errorf("[service].name %q cannot be used as an image name for [container].build (use lowercase letters, digits, and single separators)", cfg.Service.Name)
	}
	return cfg, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func dedupe(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func cloneSecrets(s *Secrets) *Secrets {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

func cloneTunnel(t *Tunnel) *Tunnel {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func cloneEndpoint(e *Endpoint) *Endpoint {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

func cloneContainer(c *Container) *Container {
	if c == nil {
		return nil
	}
	out := *c
	out.Ports = append([]string{}, c.Ports...)
	out.Volumes = append([]string{}, c.Volumes...)
	out.Args = append([]string{}, c.Args...)
	return &out
}
