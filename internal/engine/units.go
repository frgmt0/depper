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

// StartScript is the process-mode start script: `exec` keeps the command as
// the unit's main process.
func StartScript(command string) string {
	return "#!/bin/sh\nexec " + command + "\n"
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

// containerImage is the image a build/image-mode release runs.
func containerImage(cfg *config.Config, releaseID string) string {
	if cfg.Container.Mode() == config.ModeBuild {
		return cfg.ImageRef(releaseID)
	}
	return cfg.Container.Image
}

func execLine(words ...string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = unit.Arg(w)
	}
	return strings.Join(quoted, " ")
}

// ContainerUnit renders the unit for [container] build/image/compose modes.
// enginePath is the absolute engine path on the host.
func ContainerUnit(cfg *config.Config, base, enginePath, releaseID string) (string, error) {
	c := cfg.Container
	if c == nil {
		return "", fmt.Errorf("[container] is not configured")
	}
	envFile := base + "/.depper/container.env"
	lines := []string{
		"[Unit]",
		"Description=depper: " + cfg.Service.Name,
		"After=network.target",
		"",
		"[Service]",
		"Type=simple",
		"WorkingDirectory=" + unit.Path(base+"/current"),
	}
	switch c.Mode() {
	case config.ModeCompose:
		project := ComposeProject(cfg)
		lines = append(lines,
			"EnvironmentFile="+unit.Path(base+"/.depper/environment"),
			"ExecStart="+execLine(enginePath, "compose", "-p", project, "-f", c.Compose, "--env-file", envFile, "up", "--build", "--remove-orphans"),
			"ExecStop="+execLine(enginePath, "compose", "-p", project, "-f", c.Compose, "down"),
		)
	default:
		name := cfg.ContainerName()
		run := []string{enginePath, "run", "--rm", "--name", name, "--env-file", envFile}
		for _, p := range c.Ports {
			run = append(run, "-p", p)
		}
		for _, v := range c.Volumes {
			run = append(run, "-v", v)
		}
		run = append(run, c.Args...)
		run = append(run, containerImage(cfg, releaseID))
		if c.Command != "" {
			words, err := SplitWords(c.Command)
			if err != nil {
				return "", fmt.Errorf("[container].command: %w", err)
			}
			run = append(run, words...)
		}
		lines = append(lines,
			"ExecStartPre=-"+execLine(enginePath, "rm", "-f", name),
			"ExecStart="+execLine(run...),
			"ExecStop="+execLine(enginePath, "stop", name),
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

// ContainerEnvFile renders values for `--env-file`: one KEY=VALUE per line,
// sorted, values literal. That format cannot carry line breaks, so a value
// containing CR, LF, or NUL is rejected (naming the key, never the value).
func ContainerEnvFile(values map[string]string) (string, error) {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if shell.HasLineBreak(values[k]) {
			return "", fmt.Errorf("environment variable %s has a multi-line value, which the container --env-file format cannot carry; store it on one line (for example base64-encoded)", k)
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(values[k])
		b.WriteByte('\n')
	}
	return b.String(), nil
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
