package config

import (
	"net/netip"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/frgmt0/depper/internal/dotenv"
	"github.com/frgmt0/depper/internal/shell"
)

var (
	hostNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,252}$`)
	userRe       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}$`)
	zoneRe       = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	fqdnLabelRe  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	relPathRe    = regexp.MustCompile(`^[A-Za-z0-9_.@+][A-Za-z0-9_.@+/-]*$`)
	imageRefRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@+-]{0,254}$`)
	restartModes = map[string]bool{"on-failure": true, "always": true, "no": true}
)

func expandUser(p string) string { return dotenv.ExpandUser(p) }

func hasControl(s string) bool { return shell.HasControl(s) }

// localPath expands ~ and makes a relative path relative to repo.
func localPath(value, repo string) string {
	expanded := expandUser(value)
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(repo, expanded)
	}
	return filepath.Clean(expanded)
}

// validateHost checks an SSH destination: alias, DNS name, IPv4, or IPv6
// literal (bare or bracketed), optionally prefixed with user@. It returns
// the host with IPv6 brackets removed.
func validateHost(host, label string) (string, error) {
	user, addr := "", host
	if i := strings.LastIndexByte(host, '@'); i >= 0 {
		user, addr = host[:i], host[i+1:]
		if !userRe.MatchString(user) {
			return "", errorf("%s.host has an invalid user part", label)
		}
	}
	bad := errorf("%s.host must be a safe SSH alias, DNS name, IPv4, or IPv6 address, optionally prefixed with user@", label)
	if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
		addr = addr[1 : len(addr)-1]
		if !strings.Contains(addr, ":") {
			return "", bad
		}
	}
	if strings.Contains(addr, ":") {
		ip, err := netip.ParseAddr(addr)
		if err != nil || !ip.Is6() || (ip.Zone() != "" && !zoneRe.MatchString(ip.Zone())) {
			return "", bad
		}
	} else if !hostNameRe.MatchString(addr) {
		return "", bad
	}
	if user != "" {
		return user + "@" + addr, nil
	}
	return addr, nil
}

// validateHTTPURL checks an http(s) URL with a host and without credentials
// or fragment.
func validateHTTPURL(raw, label string) (string, error) {
	if hasControl(raw) || strings.ContainsAny(raw, " ") {
		return "", errorf("%s may not contain spaces or control characters", label)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return "", errorf("%s must be an http:// or https:// URL with a host", label)
	}
	if u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return "", errorf("%s may not contain credentials or a fragment", label)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", errorf("%s has an invalid port", label)
		}
	}
	return raw, nil
}

func validateRelPath(value, label string) error {
	if !relPathRe.MatchString(value) {
		return errorf("%s must be a plain relative path inside the repository", label)
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return errorf("%s must be a plain relative path inside the repository", label)
		}
	}
	return nil
}

func validPort(s string) bool {
	if s == "" || len(s) > 5 || strings.TrimLeft(s, "0123456789") != "" {
		return false
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// validatePortMapping accepts host:container, ip:host:container, or
// [ipv6]:host:container, each with an optional /tcp or /udp suffix.
func validatePortMapping(spec string) bool {
	rest := spec
	if i := strings.LastIndexByte(rest, '/'); i >= 0 {
		if proto := rest[i+1:]; proto != "tcp" && proto != "udp" {
			return false
		}
		rest = rest[:i]
	}
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]:")
		if end < 0 {
			return false
		}
		ip, err := netip.ParseAddr(rest[1:end])
		if err != nil || !ip.Is6() || ip.Zone() != "" {
			return false
		}
		rest = rest[end+2:]
		parts := strings.Split(rest, ":")
		return len(parts) == 2 && validPort(parts[0]) && validPort(parts[1])
	}
	parts := strings.Split(rest, ":")
	switch len(parts) {
	case 2:
		return validPort(parts[0]) && validPort(parts[1])
	case 3:
		ip, err := netip.ParseAddr(parts[0])
		return err == nil && ip.Is4() && validPort(parts[1]) && validPort(parts[2])
	}
	return false
}

// validateVolume accepts /abs/host:/container[:ro|:rw].
func validateVolume(spec string) bool {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	if len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw" {
		return false
	}
	for _, p := range parts[:2] {
		if !shell.IsPlainPath(p) || strings.Contains(p, ",") || path.Clean(p) != p {
			return false
		}
	}
	return true
}

// deployLayer holds the [deploy] keys a target may override; nil means
// unset.
type deployLayer struct {
	host, user               *string
	port                     *int
	identityFile, knownHosts *string
	path                     *string
	setup                    []string
	setupSet                 bool
	keepReleases             *int
	source                   *string
}

func (d deployLayer) clone() deployLayer {
	d.setup = append([]string(nil), d.setup...)
	return d
}

func (d *deployLayer) overlay(o deployLayer) {
	for _, f := range []struct{ dst, src **string }{
		{&d.host, &o.host}, {&d.user, &o.user}, {&d.identityFile, &o.identityFile},
		{&d.knownHosts, &o.knownHosts}, {&d.path, &o.path}, {&d.source, &o.source},
	} {
		if *f.src != nil {
			*f.dst = *f.src
		}
	}
	if o.port != nil {
		d.port = o.port
	}
	if o.keepReleases != nil {
		d.keepReleases = o.keepReleases
	}
	if o.setupSet {
		d.setup, d.setupSet = append([]string(nil), o.setup...), true
	}
}

func (d *deployLayer) parse(t map[string]any, label, repo string) error {
	var err error
	if d.host, err = optString(t, "host", label); err != nil {
		return err
	}
	if d.host != nil {
		h, err := validateHost(*d.host, label)
		if err != nil {
			return err
		}
		d.host = &h
	}
	if d.user, err = optString(t, "user", label); err != nil {
		return err
	}
	if d.user != nil && !userRe.MatchString(*d.user) {
		return errorf("%s.user must be a plain user name", label)
	}
	if d.port, err = optInt(t, "port", label); err != nil {
		return err
	}
	if d.port != nil && (*d.port < 1 || *d.port > 65535) {
		return errorf("%s.port must be between 1 and 65535", label)
	}
	for _, f := range []struct {
		key string
		dst **string
	}{{"identity_file", &d.identityFile}, {"known_hosts", &d.knownHosts}} {
		value, err := optString(t, f.key, label)
		if err != nil {
			return err
		}
		if value != nil {
			if hasControl(*value) {
				return errorf("%s.%s may not contain control characters", label, f.key)
			}
			p := localPath(*value, repo)
			*f.dst = &p
		}
	}
	if d.path, err = optString(t, "path", label); err != nil {
		return err
	}
	if d.path != nil && !shell.IsPlainPath(*d.path) {
		return errorf("%s.path must be an absolute, plain remote path (no spaces, quotes, backslashes, or percent signs)", label)
	}
	if d.setup, d.setupSet, err = optStringList(t, "setup", label); err != nil {
		return err
	}
	for _, cmd := range d.setup {
		if shell.HasLineBreak(cmd) {
			return errorf("%s.setup commands may not contain NUL or a newline", label)
		}
	}
	if d.keepReleases, err = optInt(t, "keep_releases", label); err != nil {
		return err
	}
	if d.keepReleases != nil && *d.keepReleases < 2 {
		return errorf("%s.keep_releases must be an integer >= 2", label)
	}
	if d.source, err = optString(t, "source", label); err != nil {
		return err
	}
	if d.source != nil && *d.source != SourceWorktree && *d.source != SourceGit {
		return errorf("%s.source must be %q or %q", label, SourceWorktree, SourceGit)
	}
	return nil
}

// healthLayer holds [health] keys; nil means unset.
type healthLayer struct {
	url, command                   *string
	expectStatus                   *int
	timeout, startPeriod, interval *time.Duration
	label                          string
}

func (h healthLayer) clone() healthLayer { return h }

func (h healthLayer) any() bool {
	return h.url != nil || h.command != nil || h.expectStatus != nil || h.timeout != nil || h.startPeriod != nil || h.interval != nil
}

// overlay applies a target's [health]: setting url or command switches the
// check, replacing both base url and command.
func (h *healthLayer) overlay(o healthLayer) {
	if o.url != nil || o.command != nil {
		h.url, h.command = o.url, o.command
		h.label = o.label
	}
	if o.expectStatus != nil {
		h.expectStatus = o.expectStatus
	}
	if o.timeout != nil {
		h.timeout = o.timeout
	}
	if o.startPeriod != nil {
		h.startPeriod = o.startPeriod
	}
	if o.interval != nil {
		h.interval = o.interval
	}
}

func (h *healthLayer) parse(t map[string]any, label string) error {
	if err := checkKeys(t, label, "url", "command", "expect_status", "timeout", "start_period", "interval"); err != nil {
		return err
	}
	h.label = label
	var err error
	if h.url, err = optString(t, "url", label); err != nil {
		return err
	}
	if h.url != nil {
		if _, err := validateHTTPURL(*h.url, label+".url"); err != nil {
			return err
		}
	}
	if h.command, err = optString(t, "command", label); err != nil {
		return err
	}
	if h.command != nil && shell.HasLineBreak(*h.command) {
		return errorf("%s.command may not contain NUL or a newline", label)
	}
	if h.url != nil && h.command != nil {
		return errorf("%s requires exactly one of url or command", label)
	}
	if h.expectStatus, err = optInt(t, "expect_status", label); err != nil {
		return err
	}
	if h.expectStatus != nil && (*h.expectStatus < 100 || *h.expectStatus > 599) {
		return errorf("%s.expect_status must be an HTTP status between 100 and 599", label)
	}
	for _, f := range []struct {
		key      string
		dst      **time.Duration
		positive bool
	}{{"timeout", &h.timeout, true}, {"start_period", &h.startPeriod, false}, {"interval", &h.interval, true}} {
		if *f.dst, err = optSeconds(t, f.key, label); err != nil {
			return err
		}
		if f.positive && *f.dst != nil && **f.dst <= 0 {
			return errorf("%s.%s must be greater than 0", label, f.key)
		}
	}
	return nil
}

func (h healthLayer) finish() (*Health, error) {
	label := h.label
	if label == "" {
		label = "[health]"
	}
	if (h.url == nil) == (h.command == nil) {
		return nil, errorf("%s requires exactly one of url or command", label)
	}
	out := &Health{URL: deref(h.url), Command: deref(h.command), Timeout: DefaultTimeout, StartPeriod: DefaultStartPeriod, Interval: DefaultInterval}
	if h.expectStatus != nil {
		out.ExpectStatus = *h.expectStatus
	}
	if h.timeout != nil {
		out.Timeout = *h.timeout
	}
	if h.startPeriod != nil {
		out.StartPeriod = *h.startPeriod
	}
	if h.interval != nil {
		out.Interval = *h.interval
	}
	return out, nil
}

func parseEnv(t map[string]any, label string) (map[string]string, error) {
	env := map[string]string{}
	for _, key := range sortedKeys(t) {
		if !envRe.MatchString(key) {
			return nil, errorf("invalid %s name: %q", label, key)
		}
		value, ok := t[key].(string)
		if !ok || shell.HasLineBreak(value) {
			return nil, errorf("%s.%s must be a single-line string", label, key)
		}
		env[key] = value
	}
	return env, nil
}

func parseSecrets(t map[string]any, label, repo string) (*Secrets, error) {
	if err := checkKeys(t, label, "file"); err != nil {
		return nil, err
	}
	file, err := optString(t, "file", label)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, errorf("%s.file must be a non-empty string", label)
	}
	if hasControl(*file) {
		return nil, errorf("%s.file may not contain control characters", label)
	}
	return &Secrets{File: localPath(*file, repo)}, nil
}

func parseEndpoint(t map[string]any, label string) (*Endpoint, error) {
	if err := checkKeys(t, label, "url"); err != nil {
		return nil, err
	}
	u, err := optString(t, "url", label)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, errorf("%s.url must be a non-empty string", label)
	}
	if _, err := validateHTTPURL(*u, label+".url"); err != nil {
		return nil, err
	}
	return &Endpoint{URL: *u}, nil
}

func parseTunnel(t map[string]any) (*Tunnel, error) {
	const label = "[tunnel]"
	if err := checkKeys(t, label, "hostname", "service"); err != nil {
		return nil, err
	}
	hostname, err := optString(t, "hostname", label)
	if err != nil {
		return nil, err
	}
	origin, err := optString(t, "service", label)
	if err != nil {
		return nil, err
	}
	if hostname == nil {
		return nil, errorf("[tunnel].hostname must be a non-empty string")
	}
	if origin == nil {
		return nil, errorf("[tunnel].service must be a non-empty string")
	}
	if hasControl(*origin) {
		return nil, errorf("[tunnel].service may not contain control characters")
	}
	host := strings.TrimRight(strings.ToLower(*hostname), ".")
	labels := strings.Split(host, ".")
	if len(labels) < 3 {
		return nil, errorf("[tunnel].hostname must be an exact public FQDN with at least three labels")
	}
	for _, l := range labels {
		if !fqdnLabelRe.MatchString(l) {
			return nil, errorf("[tunnel].hostname must be an exact public FQDN with at least three labels")
		}
	}
	plain := errorf("[tunnel].service must be a plain http:// or https:// origin without credentials, query, fragment, or path")
	u, err := url.Parse(*origin)
	if err != nil {
		return nil, plain
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, plain
	}
	port := u.Port()
	if port == "" {
		return nil, errorf("[tunnel].service must include an explicit origin port")
	}
	if !validPort(port) {
		return nil, errorf("[tunnel].service has an invalid port")
	}
	return &Tunnel{Hostname: host, Service: strings.TrimRight(*origin, "/")}, nil
}

func parseContainer(t map[string]any) (*Container, error) {
	const label = "[container]"
	if err := checkKeys(t, label, "engine", "build", "dockerfile", "image", "compose", "ports", "volumes", "args", "command"); err != nil {
		return nil, err
	}
	c := &Container{Engine: DefaultEngine}
	engine, err := optString(t, "engine", label)
	if err != nil {
		return nil, err
	}
	if engine != nil {
		if *engine != "docker" && *engine != "podman" {
			return nil, errorf("[container].engine must be \"docker\" or \"podman\"")
		}
		c.Engine = *engine
	}
	fields := []struct {
		key string
		dst *string
	}{{"build", &c.Build}, {"dockerfile", &c.Dockerfile}, {"image", &c.Image}, {"compose", &c.Compose}, {"command", &c.Command}}
	for _, f := range fields {
		value, err := optString(t, f.key, label)
		if err != nil {
			return nil, err
		}
		if value != nil {
			*f.dst = *value
		}
	}
	set := 0
	for _, v := range []string{c.Build, c.Image, c.Compose} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return nil, errorf("[container] requires exactly one of build, image, or compose")
	}
	for _, f := range []struct{ key, value string }{{"build", c.Build}, {"dockerfile", c.Dockerfile}, {"compose", c.Compose}} {
		if f.value != "" {
			if err := validateRelPath(f.value, "[container]."+f.key); err != nil {
				return nil, err
			}
		}
	}
	if c.Dockerfile != "" && c.Build == "" {
		return nil, errorf("[container].dockerfile requires [container].build")
	}
	if c.Image != "" && !imageRefRe.MatchString(c.Image) {
		return nil, errorf("[container].image must be a plain image reference")
	}
	if c.Command != "" && shell.HasLineBreak(c.Command) {
		return nil, errorf("[container].command may not contain NUL or a newline")
	}
	if c.Ports, _, err = optStringList(t, "ports", label); err != nil {
		return nil, err
	}
	for _, p := range c.Ports {
		if !validatePortMapping(p) {
			return nil, errorf("[container].ports entry %q must be \"host:container\" or \"ip:host:container\" (optional /tcp or /udp)", p)
		}
	}
	if c.Volumes, _, err = optStringList(t, "volumes", label); err != nil {
		return nil, err
	}
	for _, v := range c.Volumes {
		if !validateVolume(v) {
			return nil, errorf("[container].volumes entry %q must be \"/abs/host:/container\" with optional :ro or :rw (plain paths)", v)
		}
	}
	if c.Args, _, err = optStringList(t, "args", label); err != nil {
		return nil, err
	}
	for _, a := range c.Args {
		if shell.HasLineBreak(a) {
			return nil, errorf("[container].args entries may not contain NUL or a newline")
		}
	}
	if c.Compose != "" && (len(c.Ports) > 0 || len(c.Volumes) > 0 || len(c.Args) > 0 || c.Command != "") {
		return nil, errorf("[container].ports, volumes, args, and command are not used with compose; set them in the compose file")
	}
	return c, nil
}

// baseLayer is the top-level configuration before target overlay.
type baseLayer struct {
	name, command, restart string
	deploy                 deployLayer
	exclude                []string
	env                    map[string]string
	secrets                *Secrets
	tunnel                 *Tunnel
	container              *Container
	endpoint               *Endpoint
	health                 healthLayer
}

func (b *baseLayer) parse(raw map[string]any, repo string) error {
	serviceRaw, ok := raw["service"]
	if !ok {
		return errorf("[service] must be a table")
	}
	service, err := asTable(serviceRaw, "[service]")
	if err != nil {
		return err
	}
	deployRaw, ok := raw["deploy"]
	if !ok {
		return errorf("[deploy] must be a table")
	}
	deploy, err := asTable(deployRaw, "[deploy]")
	if err != nil {
		return err
	}
	if err := checkKeys(service, "[service]", "name", "command", "restart"); err != nil {
		return err
	}
	if err := checkKeys(deploy, "[deploy]", "host", "user", "port", "identity_file", "known_hosts", "path", "setup", "exclude", "keep_releases", "source", "healthcheck"); err != nil {
		return err
	}

	name, err := optString(service, "name", "[service]")
	if err != nil {
		return err
	}
	if name == nil {
		return errorf("[service].name must be a non-empty string")
	}
	if !nameRe.MatchString(*name) {
		return errorf("[service].name must be a safe systemd unit name (letters, digits, '.', '_', '-'; max 63 chars)")
	}
	b.name = *name
	command, err := optString(service, "command", "[service]")
	if err != nil {
		return err
	}
	if command != nil {
		if shell.HasLineBreak(*command) {
			return errorf("[service].command may not contain NUL or a newline")
		}
		b.command = *command
	}
	b.restart = DefaultRestart
	restart, err := optString(service, "restart", "[service]")
	if err != nil {
		return err
	}
	if restart != nil {
		if !restartModes[*restart] {
			return errorf("[service].restart must be on-failure, always, or no")
		}
		b.restart = *restart
	}

	if err := b.deploy.parse(deploy, "[deploy]", repo); err != nil {
		return err
	}
	if b.exclude, _, err = optStringList(deploy, "exclude", "[deploy]"); err != nil {
		return err
	}
	for _, item := range b.exclude {
		if shell.HasLineBreak(item) || strings.HasPrefix(item, "/") || hasParentPart(item) {
			return errorf("[deploy].exclude entries must be safe relative patterns")
		}
	}
	healthcheck, err := optString(deploy, "healthcheck", "[deploy]")
	if err != nil {
		return err
	}
	if healthcheck != nil && shell.HasLineBreak(*healthcheck) {
		return errorf("[deploy].healthcheck may not contain NUL or a newline")
	}

	b.env = map[string]string{}
	if envTable, err := optTable(raw, "env", "[env]"); err != nil {
		return err
	} else if envTable != nil {
		if b.env, err = parseEnv(envTable, "[env]"); err != nil {
			return err
		}
	}
	if t, err := optTable(raw, "secrets", "[secrets]"); err != nil {
		return err
	} else if t != nil {
		if b.secrets, err = parseSecrets(t, "[secrets]", repo); err != nil {
			return err
		}
	}
	if t, err := optTable(raw, "tunnel", "[tunnel]"); err != nil {
		return err
	} else if t != nil {
		if b.tunnel, err = parseTunnel(t); err != nil {
			return err
		}
	}
	if t, err := optTable(raw, "container", "[container]"); err != nil {
		return err
	} else if t != nil {
		if b.container, err = parseContainer(t); err != nil {
			return err
		}
	}
	if t, err := optTable(raw, "endpoint", "[endpoint]"); err != nil {
		return err
	} else if t != nil {
		if b.endpoint, err = parseEndpoint(t, "[endpoint]"); err != nil {
			return err
		}
	}
	if t, err := optTable(raw, "health", "[health]"); err != nil {
		return err
	} else if t != nil {
		if err := b.health.parse(t, "[health]"); err != nil {
			return err
		}
	}
	if healthcheck != nil {
		if b.health.command != nil {
			return errorf("[deploy].healthcheck is an alias of [health].command; set only one")
		}
		if b.health.url != nil {
			return errorf("[health] requires exactly one of url or command ([deploy].healthcheck sets command)")
		}
		b.health.command = healthcheck
		if b.health.label == "" {
			b.health.label = "[health]"
		}
	}

	switch {
	case b.container != nil && command != nil:
		return errorf("[service].command and [container] are mutually exclusive (use [container].command to override the image command)")
	case b.container == nil && command == nil:
		return errorf("[service].command must be a non-empty string (or configure [container])")
	}
	return nil
}

func hasParentPart(pattern string) bool {
	for _, part := range strings.Split(pattern, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// targetLayer is one [targets.<name>] overlay.
type targetLayer struct {
	name     *string
	deploy   deployLayer
	env      map[string]string
	secrets  *Secrets
	endpoint *Endpoint
	health   healthLayer
}

func (t *targetLayer) parse(table map[string]any, label, repo string) error {
	if err := checkKeys(table, label, "name", "host", "user", "port", "identity_file", "known_hosts", "path", "setup", "keep_releases", "source", "env", "secrets", "endpoint", "health"); err != nil {
		return err
	}
	var err error
	if t.name, err = optString(table, "name", label); err != nil {
		return err
	}
	if t.name != nil && !nameRe.MatchString(*t.name) {
		return errorf("%s.name must be a safe systemd unit name (letters, digits, '.', '_', '-'; max 63 chars)", label)
	}
	if err := t.deploy.parse(table, label, repo); err != nil {
		return err
	}
	if sub, err := optTable(table, "env", label+".env"); err != nil {
		return err
	} else if sub != nil {
		if t.env, err = parseEnv(sub, label[:len(label)-1]+".env]"); err != nil {
			return err
		}
	}
	if sub, err := optTable(table, "secrets", label+".secrets"); err != nil {
		return err
	} else if sub != nil {
		if t.secrets, err = parseSecrets(sub, label[:len(label)-1]+".secrets]", repo); err != nil {
			return err
		}
	}
	if sub, err := optTable(table, "endpoint", label+".endpoint"); err != nil {
		return err
	} else if sub != nil {
		if t.endpoint, err = parseEndpoint(sub, label[:len(label)-1]+".endpoint]"); err != nil {
			return err
		}
	}
	if sub, err := optTable(table, "health", label+".health"); err != nil {
		return err
	} else if sub != nil {
		if err := t.health.parse(sub, label[:len(label)-1]+".health]"); err != nil {
			return err
		}
	}
	return nil
}
