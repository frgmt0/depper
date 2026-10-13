package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/config"
)

func init() { register(newInitCommand) }

func newInitCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "init [REPO]",
		Short: "Create a starter depper.toml",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := "."
			if len(args) == 1 {
				repo = args[0]
			}
			path, err := writeStarterConfig(repo)
			if err != nil {
				return err
			}
			if app.JSON {
				return app.WriteJSON(map[string]string{"created": path})
			}
			app.Printf("Created %s\n", path)
			return nil
		},
	}
}

var unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// serviceNameFor derives a valid [service].name from a directory name.
func serviceNameFor(dir string) string {
	name := unsafeNameChars.ReplaceAllString(filepath.Base(dir), "-")
	name = strings.TrimLeft(name, "-_.")
	if len(name) > 63 {
		name = name[:63]
	}
	if name == "" {
		name = "app"
	}
	return name
}

// writeStarterConfig writes depper.toml into repo, refusing to overwrite an
// existing depper.toml or deploy.toml.
func writeStarterConfig(repo string) (string, error) {
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
	for _, name := range []string{config.FileName, config.LegacyFileName} {
		existing := filepath.Join(abs, name)
		if _, err := os.Lstat(existing); err == nil {
			return "", fmt.Errorf("%s already exists", existing)
		}
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = abs
	}
	path := filepath.Join(abs, config.FileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("creating %s: %w", path, err)
	}
	if _, err := f.WriteString(starterConfig(serviceNameFor(resolved))); err != nil {
		f.Close()
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

func starterConfig(name string) string {
	return `# depper configuration: https://github.com/frgmt0/depper

[service]
name = "` + name + `"
command = "python -m your_app"
restart = "on-failure"

[deploy]
host = "desktop"            # SSH alias, DNS name, IPv4, or IPv6 address (user@ allowed)
# user = "deploy"
# port = 22
# identity_file = "~/.ssh/id_ed25519"
# known_hosts = "~/.ssh/known_hosts"   # pins host keys (StrictHostKeyChecking=yes)
# path = "/absolute/path/on/server"    # default: ~/.local/share/depper/<name>
# setup = ["python -m venv .venv", ".venv/bin/pip install -r requirements.txt"]
# exclude = ["build/"]
# keep_releases = 5
# source = "worktree"                  # or "git" to ship only committed content

[env]
# PORT = "8000"

# Secrets: a local dotenv file, never committed or synced.
# [secrets]
# file = ".env.prod"

# Where the service is reachable without a tunnel (LAN, Tailscale, VPS IP).
# [endpoint]
# url = "http://203.0.113.10:8000"

# Health check: exactly one of url (checked from the server) or command.
# [health]
# url = "http://127.0.0.1:8000/health"
# command = "curl -fsS http://127.0.0.1:8000/health"
# expect_status = 200                  # default: any 2xx
# timeout = 5
# start_period = 30
# interval = 2

# Run a container instead of [service].command (remove command above).
# [container]
# engine = "docker"                    # or "podman"
# build = "."                          # or image = "ghcr.io/you/app:1.0", or compose = "compose.yaml"
# ports = ["127.0.0.1:8000:8000"]
# volumes = ["/srv/data:/data"]

# Public HTTPS through a Cloudflare Tunnel.
# [tunnel]
# hostname = "app.example.com"
# service = "http://127.0.0.1:8000"

# Environments, selected with --target NAME or DEPPER_TARGET.
# [targets.prod]
# host = "203.0.113.10"
# [targets.prod.env]
# LOG_LEVEL = "warn"
`
}
