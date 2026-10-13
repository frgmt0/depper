package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/dotenv"
	"github.com/frgmt0/depper/internal/exitcode"
	"github.com/frgmt0/depper/internal/host"
	"github.com/frgmt0/depper/internal/version"
)

func init() { register(newHostCommand) }

// stdinIsTerminal reports whether r is an interactive terminal (tests
// replace it).
var stdinIsTerminal = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// hostRunner is the host.Runner for app; tests replace it to point at fake
// programs.
var hostRunner = func(app *App) *host.Runner {
	return &host.Runner{Exec: app.Exec, Out: app.Human(), LookupEnv: app.LookupEnv, Version: version.Version}
}

func newHostCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Prepare a fresh Linux host for depper, or show how it was prepared",
	}
	cmd.AddCommand(newHostInitCommand(app), newHostStatusCommand(app))
	return cmd
}

type hostInitFlags struct {
	port          int
	identity      string
	user          string
	key           string
	container     string
	cloudflared   bool
	hardenSSH     bool
	firewall      string
	autoUpdates   bool
	tailscaleEnv  string
	knownHosts    string
	fingerprints  []string
	tofu          bool
	writeConfig   bool
	noWriteConfig bool
	dryRun        bool
	yes           bool
}

func newHostInitCommand(app *App) *cobra.Command {
	var f hostInitFlags
	cmd := &cobra.Command{
		Use:   "init ADMIN@HOST [REPO]",
		Short: "Prepare a fresh VPS (as root or a passwordless-sudo admin) and write the connection to depper.toml",
		Long: `Prepare a fresh Linux host for depper over SSH.

host init pins the host key (after you confirm its fingerprint), creates an
unprivileged deploy user with your public key, enables lingering and its
systemd user manager, installs rsync and an optional container engine and
cloudflared, applies opt-in hardening, records the setup in
/etc/depper/host.json, runs depper doctor as the deploy user, and writes
host, user, port and known_hosts into depper.toml ([deploy], or
[targets.NAME] with --target). Every step is checked first, so running it
again changes nothing.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := hostInitOptions(app, cmd, &f, args)
			if err != nil {
				return err
			}
			runner := hostRunner(app)
			interactive := stdinIsTerminal(app.Stdin)
			if !opts.Yes && !opts.DryRun && !interactive {
				return fmt.Errorf("stdin is not a terminal, so depper cannot ask for confirmation; check the plan with --dry-run, then re-run with --yes")
			}
			if interactive {
				reader := bufio.NewReader(app.Stdin)
				runner.Confirm = func(question string) (bool, error) {
					_, _ = fmt.Fprintf(app.Human(), "%s [y/N] ", question)
					line, err := reader.ReadString('\n')
					if err != nil && !errors.Is(err, io.EOF) {
						return false, err
					}
					answer := strings.ToLower(strings.TrimSpace(line))
					return answer == "y" || answer == "yes", nil
				}
			}
			res, runErr := runner.Init(cmd.Context(), opts)
			if app.JSON && res != nil {
				if err := app.WriteJSON(res); err != nil {
					return err
				}
			}
			return runErr
		},
	}
	fl := cmd.Flags()
	fl.IntVar(&f.port, "port", 22, "SSH port of the host")
	fl.StringVar(&f.identity, "identity", "", "private key for the admin login (default: ssh's own choice)")
	fl.StringVar(&f.user, "user", host.DefaultUser, "deploy account to create")
	fl.StringVar(&f.key, "key", "", "public key to authorize for the deploy user (default: first of ~/.ssh/id_ed25519.pub, id_ecdsa.pub, id_rsa.pub)")
	fl.StringVar(&f.container, "container", "", "container engine to install: podman, docker, or none (default: the engine depper.toml resolves [container].engine to, docker when unset; none without [container])")
	fl.BoolVar(&f.cloudflared, "cloudflared", false, "install cloudflared from Cloudflare's package repository")
	fl.BoolVar(&f.hardenSSH, "harden-ssh", false, "disable password and keyboard-interactive SSH login and root password login (a stricter PermitRootLogin is kept); reverted automatically unless new logins succeed")
	fl.StringVar(&f.firewall, "firewall", "", "enable ufw/firewalld allowing these TCP ports, e.g. 80,443 (the SSH port and every port sshd listens on are always included); reverted automatically unless new logins succeed")
	fl.BoolVar(&f.autoUpdates, "auto-updates", false, "enable automatic security updates (unattended-upgrades or dnf-automatic)")
	fl.StringVar(&f.tailscaleEnv, "tailscale-authkey-env", "", "install Tailscale and join the tailnet with the auth key in this environment variable")
	fl.StringVar(&f.knownHosts, "known-hosts", "", "known_hosts file to pin the host key in (default REPO/.depper/known_hosts)")
	fl.StringArrayVar(&f.fingerprints, "host-key-fingerprint", nil, "expected host key fingerprint (SHA256:..., as ssh-keygen -lf prints; repeatable): only matching keys are pinned, and none matching is refused")
	fl.BoolVar(&f.tofu, "trust-on-first-use", false, "with --yes, pin the host keys the host presents without verifying them (otherwise --yes needs --host-key-fingerprint or an already pinned key)")
	fl.BoolVar(&f.writeConfig, "write-config", true, "write the connection into depper.toml")
	fl.BoolVar(&f.noWriteConfig, "no-write-config", false, "do not modify depper.toml")
	fl.BoolVar(&f.dryRun, "dry-run", false, "show the host keys and the plan, change nothing")
	fl.BoolVarP(&f.yes, "yes", "y", false, "do not ask for confirmation (host key and plan)")
	return cmd
}

// localFile expands ~ and makes path absolute.
func localFile(path string) (string, error) {
	abs, err := filepath.Abs(dotenv.ExpandUser(path))
	if err != nil {
		return "", fmt.Errorf("invalid path %q: %w", path, err)
	}
	return abs, nil
}

func hostInitOptions(app *App, cmd *cobra.Command, f *hostInitFlags, args []string) (*host.Options, error) {
	// The repo-first form (`depper REPO host init ADMIN@HOST`) arrives as
	// REPO ADMIN@HOST.
	if len(args) == 2 {
		if _, _, err := host.ParseDestination(args[0]); err != nil {
			if _, _, err := host.ParseDestination(args[1]); err == nil {
				args = []string{args[1], args[0]}
			}
		}
	}
	admin, addr, err := host.ParseDestination(args[0])
	if err != nil {
		return nil, err
	}
	repo := "."
	if len(args) == 2 {
		repo = args[1]
	}
	repo, err = localFile(repo)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(repo); err == nil {
		repo = resolved
	}
	if info, err := os.Stat(repo); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("repository directory %s does not exist", repo)
	}
	o := &host.Options{Repo: repo, Admin: admin, Host: addr, Port: f.port, User: f.user, Target: app.Target(),
		Cloudflared: f.cloudflared, HardenSSH: f.hardenSSH, AutoUpdates: f.autoUpdates, DryRun: f.dryRun, Yes: f.yes, WriteConfig: true,
		HostKeyFingerprints: f.fingerprints, TrustOnFirstUse: f.tofu}

	if cmd.Flags().Changed("no-write-config") && cmd.Flags().Changed("write-config") && f.writeConfig == f.noWriteConfig {
		return nil, fmt.Errorf("--write-config and --no-write-config contradict each other")
	}
	if f.noWriteConfig || !f.writeConfig {
		o.WriteConfig = false
	}

	if f.identity != "" {
		if o.Identity, err = localFile(f.identity); err != nil {
			return nil, err
		}
		if _, err := os.Stat(o.Identity); err != nil {
			return nil, fmt.Errorf("--identity: %w", err)
		}
	}
	if f.key != "" {
		if o.KeyFile, err = localFile(f.key); err != nil {
			return nil, err
		}
		o.KeyExplicit = true
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot find your home directory to locate an SSH key; pass --key FILE.pub")
		}
		if o.KeyFile, err = host.DefaultKeyFile(home); err != nil {
			return nil, err
		}
	}
	if f.knownHosts != "" {
		if o.KnownHosts, err = localFile(f.knownHosts); err != nil {
			return nil, err
		}
	} else {
		o.KnownHosts = filepath.Join(repo, ".depper", "known_hosts")
	}

	// Without --container, host init installs the engine the resolved
	// configuration uses (host.ContainerAuto).
	if cmd.Flags().Changed("container") {
		if f.container == host.ContainerAuto {
			return nil, fmt.Errorf("--container must be podman, docker, or none")
		}
		o.Container = f.container
	}

	if f.firewall != "" || cmd.Flags().Changed("firewall") {
		ports, err := parsePorts(f.firewall)
		if err != nil {
			return nil, err
		}
		o.Firewall = ports
	}

	if f.tailscaleEnv != "" {
		if err := host.ValidateEnvName(f.tailscaleEnv); err != nil {
			return nil, fmt.Errorf("--tailscale-authkey-env: %w", err)
		}
		key, ok := app.LookupEnv(f.tailscaleEnv)
		if !ok || key == "" {
			return nil, fmt.Errorf("--tailscale-authkey-env: the environment variable %s is not set", f.tailscaleEnv)
		}
		if err := host.ValidateTailscaleKey(key); err != nil {
			return nil, fmt.Errorf("--tailscale-authkey-env %s: %w", f.tailscaleEnv, err)
		}
		o.TailscaleKey = host.NewSecret(key)
	}
	return o, nil
}

// parsePorts parses a comma-separated list of TCP ports.
func parsePorts(s string) ([]int, error) {
	var ports []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(part), "/tcp"))
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("--firewall: invalid port %q (use a comma-separated list such as 22,80,443)", part)
		}
		ports = append(ports, n)
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("--firewall needs at least one port, such as 22,80,443")
	}
	return ports, nil
}

func newHostStatusCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status [REPO]",
		Short: "Show how the deploy host was prepared and check it, as the deploy user (exit 1 on failures)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := app.Config(repoArg(args))
			if err != nil {
				return err
			}
			res, err := hostRunner(app).Status(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			if app.JSON {
				if err := app.WriteJSON(res); err != nil {
					return err
				}
			} else {
				res.WriteHuman(app.Human())
			}
			if res.Failed() {
				return exitcode.Silent(exitcode.Failure)
			}
			return nil
		},
	}
}
