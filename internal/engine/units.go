package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/unit"
)

// StartScriptName is the process-mode start script inside each release.
const StartScriptName = ".depper-start.sh"

// ReleaseMetadataName is the release metadata file inside each release.
const ReleaseMetadataName = ".depper-release.json"

// StartScript is the process-mode start script. It first applies
// shell.SessionEnv, so the service never runs with another user's
// XDG_RUNTIME_DIR or unusable XDG base directories inherited from the
// systemd user manager (pam_env and environment.d can put them there, and
// UnsetEnvironment= is not available to every user unit). `exec` then keeps
// the command as the unit's main process.
func StartScript(command string) string {
	return "#!/bin/sh\n" + shell.Script(append(shell.SessionEnv(), "exec "+command)...)
}

// ProcessUnit renders the unit for [service].command.
func ProcessUnit(name, restart, base string) string {
	return shell.Script(
		"[Unit]",
		"Description=depper: "+name,
		"After=network.target",
		"",
		"[Service]",
		"Type=simple",
		"WorkingDirectory="+unit.Path(base+"/current"),
		"EnvironmentFile="+unit.Path(base+"/.depper/environment"),
		"ExecStart=/bin/sh "+unit.Arg(base+"/current/"+StartScriptName),
		"Restart="+restart,
		"RestartSec=3",
		"",
		"[Install]",
		"WantedBy=default.target",
	)
}

// ComposeProject is the compose project name: depper-<name>, lowercased with
// characters compose rejects replaced.
func ComposeProject(cfg *config.Config) string {
	return strings.NewReplacer(".", "_").Replace(strings.ToLower(cfg.ContainerName()))
}

// containerImage is the image a build/image-mode release runs: its own
// per-release tag (image mode tags the pulled image during setup), so a
// rollback or restore runs exactly the image that release was deployed with.
func containerImage(cfg *config.Config, releaseID string) string {
	return cfg.ImageRef(releaseID)
}

func execLine(words ...string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = unit.Arg(w)
	}
	return strings.Join(quoted, " ")
}

// EngineWrapperScript is the `sh -c` script every container-mode Exec*= line
// runs the engine through: it applies shell.SessionEnv (rootless podman
// refuses a runtime directory it does not own, and the systemd user manager
// may carry another user's XDG_* variables, see StartScript) and then execs
// the engine, which sh receives as $0, with its arguments as "$@".
func EngineWrapperScript() string {
	return strings.Join(shell.SessionEnv(), "; ") + `; exec "$0" "$@"`
}

// engineLine renders an Exec*= command line that runs enginePath with args
// through /bin/sh -c EngineWrapperScript. Every word is quoted with
// unit.Arg, so systemd expands neither $ nor % in the script; callers may
// append further words, including an unquoted $VAR that systemd splits into
// extra arguments, which sh passes on through "$@".
func engineLine(enginePath string, args ...string) string {
	return "/bin/sh -c " + unit.Arg(EngineWrapperScript()) + " " + execLine(append([]string{enginePath}, args...)...)
}

// ContainerEnvArgsVar is the variable depper adds to the environment file in
// build/image mode: "--env KEY ..." for every key. The unit expands it
// unquoted in ExecStart= (systemd splits it into words), so the container
// gets exactly the configured keys, with values taken from the unit's
// EnvironmentFile= through the engine process' environment. Values never
// pass through an --env-file, whose format cannot carry line breaks (and,
// for podman, leading quotes); the key list lives in the environment file,
// so `depper secrets push` can add or remove keys without touching the
// unit.
const ContainerEnvArgsVar = "DEPPER_CONTAINER_ENV_ARGS"

// MaxContainerEnvValue is the largest value accepted in container mode
// (values travel in the engine's process environment and its API request).
const MaxContainerEnvValue = 60 << 10

// CheckContainerEnv validates values for container mode: the reserved
// ContainerEnvArgsVar name and the value size limit. Errors name the key,
// never the value.
func CheckContainerEnv(values map[string]string) error {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == ContainerEnvArgsVar {
			return fmt.Errorf("environment variable %s is reserved by depper in container mode; rename it", k)
		}
		if len(values[k]) > MaxContainerEnvValue {
			return fmt.Errorf("environment variable %s is %d bytes; container mode accepts values up to 60 KiB", k, len(values[k]))
		}
	}
	return nil
}

// ContainerEnvArgs returns "--env KEY" for every key, sorted.
func ContainerEnvArgs(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	words := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		words = append(words, "--env", k)
	}
	return strings.Join(words, " ")
}

// ContainerUnit renders the unit for [container] build/image/compose modes.
// enginePath is the absolute engine path on the host. Every mode reads the
// environment through EnvironmentFile=; see ContainerEnvArgsVar (build,
// image) and runSetup (compose) for how it reaches the containers.
func ContainerUnit(cfg *config.Config, base, enginePath, releaseID string) (string, error) {
	c := cfg.Container
	if c == nil {
		return "", fmt.Errorf("[container] is not configured")
	}
	lines := []string{
		"[Unit]",
		"Description=depper: " + cfg.Service.Name,
		"After=network.target",
		"",
		"[Service]",
		"Type=simple",
		"WorkingDirectory=" + unit.Path(base+"/current"),
		"EnvironmentFile=" + unit.Path(base+"/.depper/environment"),
	}
	switch c.Mode() {
	case config.ModeCompose:
		// --env-file /dev/null: interpolate ${VAR} only from the process
		// environment (EnvironmentFile= above), never from a .env file in
		// the release; `environment: [KEY]` passes values through as well.
		project := ComposeProject(cfg)
		lines = append(lines,
			"ExecStart="+engineLine(enginePath, "compose", "-p", project, "-f", c.Compose, "--env-file", "/dev/null", "up", "--build", "--remove-orphans"),
			"ExecStop="+engineLine(enginePath, "compose", "-p", project, "-f", c.Compose, "--env-file", "/dev/null", "down"),
		)
	default:
		name := cfg.ContainerName()
		run := engineLine(enginePath, "run", "--rm", "--name", name) + " $" + ContainerEnvArgsVar
		var rest []string
		for _, p := range c.Ports {
			rest = append(rest, "-p", p)
		}
		for _, v := range c.Volumes {
			rest = append(rest, "-v", v)
		}
		rest = append(rest, c.Args...)
		rest = append(rest, containerImage(cfg, releaseID))
		if c.Command != "" {
			words, err := SplitWords(c.Command)
			if err != nil {
				return "", fmt.Errorf("[container].command: %w", err)
			}
			rest = append(rest, words...)
		}
		lines = append(lines,
			"ExecStartPre=-"+engineLine(enginePath, "rm", "-f", name),
			"ExecStart="+run+" "+execLine(rest...),
			"ExecStop="+engineLine(enginePath, "stop", name),
		)
	}
	lines = append(lines,
		"Restart="+cfg.Service.Restart,
		"RestartSec=3",
		"",
		"[Install]",
		"WantedBy=default.target",
	)
	return shell.Script(lines...), nil
}

// RenderUnit renders the app unit for cfg's runtime mode.
func RenderUnit(cfg *config.Config, l *Layout, releaseID string) (string, error) {
	if cfg.Container == nil {
		return ProcessUnit(cfg.Service.Name, cfg.Service.Restart, l.Base), nil
	}
	if l.EnginePath == "" {
		return "", fmt.Errorf("%s is not installed on the deploy host (required by [container])", cfg.Container.Engine)
	}
	return ContainerUnit(cfg, l.Base, l.EnginePath, releaseID)
}

// SplitWords splits a command string into words with POSIX shell quoting
// rules (single quotes, double quotes with \\ \" \$ \` escapes, backslash
// escapes), without any expansion.
func SplitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			inWord = true
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			inWord = true
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\\\"$`", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated double quote")
			}
		case c == '\\':
			inWord = true
			if i+1 >= len(s) {
				return nil, fmt.Errorf("trailing backslash")
			}
			i++
			cur.WriteByte(s[i])
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	return words, nil
}
