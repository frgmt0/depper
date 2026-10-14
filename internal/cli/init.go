package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/detect"
)

func init() { register(newInitCommand) }

func newInitCommand(app *App) *cobra.Command {
	var printOnly, force bool
	cmd := &cobra.Command{
		Use:   "init [REPO]",
		Short: "Create a starter depper.toml for the detected project type",
		Long: `Inspect the repository (Dockerfile, compose file, go.mod, package.json,
Python and Rust manifests, a static index.html) and write a depper.toml
pre-filled for what it finds. Every inferred line carries a comment naming
what it came from. Nothing in the repository is executed, and .env files are
never read.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := "."
			if len(args) == 1 {
				repo = args[0]
			}
			res, content, err := renderStarter(repo)
			if err != nil {
				return err
			}
			if printOnly {
				if app.JSON {
					return app.WriteJSON(map[string]any{"detection": res, "config": content})
				}
				app.Printf("%s", content)
				return nil
			}
			path, err := writeStarterConfig(repo, content, force)
			if err != nil {
				return err
			}
			if app.JSON {
				return app.WriteJSON(map[string]any{"created": path, "detection": res})
			}
			app.Printf("Created %s\n", path)
			app.Printf("Detected: %s\n", describeDetection(res))
			return nil
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "write the config to stdout instead of depper.toml")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing depper.toml")
	return cmd
}

func describeDetection(res detect.Result) string {
	if res.Kind == detect.Generic {
		return "no known project type (wrote a generic template)"
	}
	return string(res.Kind) + " (" + strings.Join(res.Evidence, ", ") + ")"
}

// renderStarter detects the project in repo and renders its depper.toml.
func renderStarter(repo string) (detect.Result, string, error) {
	res, err := detect.Detect(repo)
	if err != nil {
		return res, "", err
	}
	return res, starterConfig(res), nil
}

// writeStarterConfig writes content to repo's depper.toml. It refuses to
// replace an existing depper.toml unless force is set, and always refuses
// when a legacy deploy.toml is present.
func writeStarterConfig(repo, content string, force bool) (string, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("repository directory %s: %w", abs, errors.Unwrap(err))
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	legacy := filepath.Join(abs, config.LegacyFileName)
	if _, err := os.Lstat(legacy); err == nil {
		return "", fmt.Errorf("%s already exists", legacy)
	}
	path := filepath.Join(abs, config.FileName)
	if force {
		return path, replaceFile(path, content)
	}
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("%s already exists (use --force to overwrite it)", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("creating %s: %w", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

// replaceFile atomically replaces path (a symlink at path is replaced, not
// followed) via a temporary file in the same directory.
func replaceFile(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".depper.toml.*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// tomlString renders s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// oneLine makes s safe to put in a TOML comment.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

type tomlWriter struct{ strings.Builder }

func (w *tomlWriter) line(format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}

// comment writes text as "# " lines.
func (w *tomlWriter) comment(text string) {
	for _, l := range strings.Split(text, "\n") {
		w.line("# %s", oneLine(l))
	}
}

// note writes a value's note (if any) above its line.
func (w *tomlWriter) note(v detect.Value) {
	if v.Note != "" {
		w.comment(v.Note)
	}
}

func trailing(from string) string {
	if from == "" {
		return ""
	}
	return "  # " + oneLine(from)
}

// kv writes `key = "value"  # from`.
func (w *tomlWriter) kv(key string, v detect.Value) {
	w.note(v)
	w.line("%s = %s%s", key, tomlString(v.Value), trailing(v.From))
}

// list writes a multi-line array with a trailing comment per element.
func (w *tomlWriter) list(key string, items []detect.Value) {
	w.line("%s = [", key)
	for _, it := range items {
		w.line("  %s,%s", tomlString(it.Value), trailing(it.From))
	}
	w.line("]")
}

func kindLabel(k detect.Kind) string {
	switch k {
	case detect.Compose:
		return "Docker Compose"
	case detect.Dockerfile:
		return "Dockerfile"
	case detect.Go:
		return "Go"
	case detect.Node:
		return "Node.js"
	case detect.Python:
		return "Python"
	case detect.Rust:
		return "Rust"
	case detect.Static:
		return "static site"
	}
	return string(k)
}

// starterConfig renders a depper.toml for a detection result. The output
// always loads and resolves (a test checks every kind).
func starterConfig(res detect.Result) string {
	var w tomlWriter
	generic := res.Kind == detect.Generic
	container := res.Container != nil
	port := res.Port
	examplePort := port
	if examplePort == 0 {
		examplePort = 8000
	}

	w.line("# depper configuration: https://github.com/frgmt0/depper")
	w.line("#")
	if generic {
		w.line("# Generated by \"depper init\". No known project type was detected, so this is")
		w.line("# a generic template: replace the placeholders.")
	} else {
		w.line("# Generated by \"depper init\" for a %s project.", kindLabel(res.Kind))
		w.line("# Detected from: %s", oneLine(strings.Join(res.Evidence, ", ")))
		w.line("# Lines ending in \"# ...\" are guesses inferred from your repository: check each")
		w.line("# one before deploying.")
		if len(res.Requires) > 0 {
			w.line("# The server needs: %s.", oneLine(strings.Join(res.Requires, ", ")))
		}
	}
	for _, warning := range res.Warnings {
		w.line("# Warning: %s", oneLine(warning))
	}
	w.line("")

	w.line("[service]")
	w.line("name = %s  # from the directory name", tomlString(res.Name))
	if !container {
		w.kv("command", res.Command)
	}
	w.line(`restart = "on-failure"`)
	w.line("")

	w.line("[deploy]")
	w.line("# Replace with your server: an SSH alias, a DNS name, an IPv4 address, or an")
	w.line("# IPv6 address, optionally prefixed with user@ (203.0.113.10 is a documentation address).")
	w.line("# No server set up yet? Run: depper host init root@YOUR_SERVER")
	w.line(`host = "deploy@203.0.113.10"`)
	w.line(`# user = "deploy"                      # alternative to a user@ prefix in host`)
	w.line("# port = 22")
	w.line(`# identity_file = "~/.ssh/id_ed25519"  # no whitespace in the path`)
	w.line(`# known_hosts = "~/.ssh/known_hosts"   # pins host keys (StrictHostKeyChecking=yes); no whitespace in the path`)
	w.line(`# path = "/absolute/path/on/server"    # default: ~/.local/share/depper/<name>; only letters, digits and . _ @ + / -`)
	if len(res.Setup) > 0 {
		w.comment("Commands run on the server inside each new release, before the service starts.")
		w.list("setup", res.Setup)
	} else {
		w.line(`# setup = ["python -m venv .venv", ".venv/bin/pip install -r requirements.txt"]`)
	}
	if len(res.Excludes) > 0 {
		w.comment("Not uploaded, on top of the built-in excludes (.git/, .env, node_modules/, ...).")
		w.list("exclude", res.Excludes)
	} else {
		w.line(`# exclude = ["build/"]`)
	}
	w.line("# keep_releases = 5")
	w.line(`# source = "worktree"                  # or "git" to ship only committed content`)
	w.line("")

	w.line("[env]")
	if port > 0 && !container {
		w.line("PORT = %s%s", tomlString(strconv.Itoa(port)), trailing(res.PortFrom))
	} else {
		w.line(`# PORT = "8000"`)
	}
	w.line("")

	if res.Secrets != nil {
		w.line("# Secrets: a local dotenv file, never committed or synced. Its contents were not read.")
		w.line("[secrets]")
		w.kv("file", *res.Secrets)
	} else {
		w.line("# Secrets: a local dotenv file, never committed or synced.")
		w.line("# [secrets]")
		w.line(`# file = ".env.prod"`)
	}
	w.line("")

	w.line("# Where the service is reachable without a tunnel (LAN, Tailscale, VPS IP).")
	w.line("# [endpoint]")
	w.line(`# url = "http://203.0.113.10:%d"`, examplePort)
	w.line("")

	if port > 0 {
		w.line("# Health check: exactly one of url (checked from the server) or command.")
		w.line("[health]")
		w.line("# Point this at a real health endpoint (for example /health) when your app has one.")
		w.line("url = %s%s", tomlString(res.Health), trailing("assumed from port "+strconv.Itoa(port)))
		w.line(`# command = "curl -fsS http://127.0.0.1:%d/health"  # alternative to url`, port)
		w.line("# expect_status = 200                  # default: any 2xx")
		w.line("# timeout = 5")
		w.line("# start_period = 30")
		w.line("# interval = 2")
	} else {
		w.line("# Health check: exactly one of url (checked from the server) or command.")
		w.line("# [health]")
		w.line(`# url = "http://127.0.0.1:%d/health"`, examplePort)
		w.line(`# command = "curl -fsS http://127.0.0.1:%d/health"`, examplePort)
		w.line("# expect_status = 200                  # default: any 2xx")
		w.line("# timeout = 5")
		w.line("# start_period = 30")
		w.line("# interval = 2")
	}
	w.line("")

	if container {
		c := res.Container
		w.line("# Runs a container instead of [service].command.")
		w.line("[container]")
		w.line(`engine = "podman"  # or "docker" (needs root-equivalent docker group)`)
		switch c.Mode {
		case detect.ModeCompose:
			w.line("compose = %s%s", tomlString(c.Path), trailing(c.From))
			w.line("# ports, volumes and args are not allowed with compose; set them in the compose file")
		default:
			w.line("build = %s%s", tomlString(c.Path), trailing(c.From))
			w.line("# Published on the loopback interface so the server's health check can reach it.")
			w.line("ports = [%s]%s", tomlString(fmt.Sprintf("127.0.0.1:%d:%d", port, port)), trailing(res.PortFrom))
			w.line(`# volumes = ["/srv/data:/data"]`)
		}
	} else {
		w.line("# Run a container instead of [service].command (remove command above).")
		w.line("# [container]")
		w.line(`# engine = "docker"                    # or "podman"`)
		w.line(`# build = "."                          # or image = "ghcr.io/you/app:1.0", or compose = "compose.yaml"`)
		w.line(`# ports = ["127.0.0.1:%d:%d"]      # ports, volumes and args are not allowed with compose`, examplePort, examplePort)
		w.line(`# volumes = ["/srv/data:/data"]`)
	}
	w.line("")

	w.line("# Public HTTPS through a Cloudflare Tunnel (not overridable per target).")
	w.line("# [tunnel]")
	w.line(`# hostname = "app.example.com"`)
	w.line(`# service = "http://127.0.0.1:%d"`, examplePort)
	w.line("")

	w.line("# Environments, selected with --target NAME or DEPPER_TARGET.")
	w.line("# [targets.prod]")
	w.line(`# host = "203.0.113.10"`)
	w.line("# [targets.prod.env]")
	w.line(`# LOG_LEVEL = "warn"`)
	return w.String()
}
